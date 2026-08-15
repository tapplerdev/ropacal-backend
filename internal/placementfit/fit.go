// Package placementfit fits the placement site score from realized outcomes.
//
// The score is a Cobb-Douglas index:
//
//	fill = C · density^b1 · anchor^b2 · pop^b3
//
// which is linear in logs:
//
//	ln(fill) = ln(C) + b1·ln(density) + b2·ln(anchor) + b3·ln(pop)
//
// so the exponents are an ordinary least-squares problem with four unknowns.
// That is small enough to solve in Go with no dependency, which keeps the whole
// loop inside one deployable — the 2026-07 decision was explicitly "batch job
// writes coefficients to Go, NO request-path Python sidecar".
//
// The package is pure: no database, no HTTP, no clock. Everything it needs
// arrives as a slice of Observations, which is what makes it testable.
package placementfit

import (
	"errors"
	"math"
	"sort"
)

// MinObservations is the floor below which a fit is refused outright.
//
// Four coefficients fitted on a handful of rows will happily produce numbers
// that look precise and mean nothing. 40 is not a statistical guarantee — at
// this sample size nothing is — it is the point below which the result is not
// worth the risk of overwriting a working model.
const MinObservations = 40

// Observation is one bin: its site features at fit time and what it actually did.
type Observation struct {
	BinID   string
	Density float64 // ln(1+errandPOI)/ln(1+100), already normalized 0..1
	Anchor  float64 // 1.0 / 0.7 / 0.15
	Pop     float64 // 0.3..1.0, or 1.0 when the ZIP is unknown
	Fill    float64 // realized current-pitch fill rate, %/day
}

// Coefficients is a fitted model.
type Coefficients struct {
	Constant float64 // C, already exponentiated out of log space
	Density  float64
	Anchor   float64
	Pop      float64
}

// Predict returns expected fill in %/day.
func (c Coefficients) Predict(density, anchor, pop float64) float64 {
	d := math.Max(density, 0.01)
	a := math.Max(anchor, 0.01)
	p := math.Max(pop, 0.01)
	v := c.Constant * math.Pow(d, c.Density) * math.Pow(a, c.Anchor) * math.Pow(p, c.Pop)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// Result carries the fit plus the honest scorecard needed to decide whether to
// promote it. The scorecard is not optional: a fit with no out-of-sample number
// attached is not evidence of anything.
type Result struct {
	Coef        Coefficients
	N           int
	MAE         float64 // leave-one-out mean absolute error, %/day
	MAEBaseline float64 // same for "predict the median" — the bar to beat
	RankRho     float64 // out-of-sample Spearman
}

// BeatsBaseline reports whether the fit is better than guessing the median.
// A model that cannot clear this has learned nothing and must not be promoted.
func (r Result) BeatsBaseline() bool {
	return r.MAEBaseline > 0 && r.MAE < r.MAEBaseline
}

// SignificanceBar is the |rho| a fit of this size must exceed to be distinguishable
// from noise at p<.05, two-tailed (normal approximation, 1.96/sqrt(n-1)).
//
// It is a FUNCTION OF n, deliberately. The Python sidecar's `signalcheck` used
// fixed thresholds (_STRONG=0.35) that ignore sample size, which is why it once
// printed "MODERATE — proceed" for a rho the sample could not actually support.
// A bar that does not move with n will eventually bless noise.
func (r Result) SignificanceBar() float64 {
	if r.N < 3 {
		return math.NaN()
	}
	return 1.96 / math.Sqrt(float64(r.N-1))
}

// SignalIsReal reports whether the out-of-sample ranking clears the bar for its
// own sample size. This is the question `signalcheck` exists to answer, computed
// on every refit rather than by hand.
func (r Result) SignalIsReal() bool {
	bar := r.SignificanceBar()
	return !math.IsNaN(bar) && math.Abs(r.RankRho) >= bar
}

var (
	ErrTooFewObservations = errors.New("placementfit: too few observations")
	ErrSingular           = errors.New("placementfit: singular design matrix")
)

// usable filters out rows the log transform cannot accept. A zero or negative
// fill rate is not a weak signal, it is an unusable one — ln(0) is -Inf and
// would poison every coefficient.
func usable(obs []Observation) []Observation {
	out := make([]Observation, 0, len(obs))
	for _, o := range obs {
		if o.Fill > 0 && o.Density > 0 && o.Anchor > 0 && o.Pop > 0 {
			out = append(out, o)
		}
	}
	return out
}

func design(obs []Observation) ([][]float64, []float64) {
	X := make([][]float64, len(obs))
	y := make([]float64, len(obs))
	for i, o := range obs {
		X[i] = []float64{1, math.Log(o.Density), math.Log(o.Anchor), math.Log(o.Pop)}
		y[i] = math.Log(o.Fill)
	}
	return X, y
}

// ols solves the normal equations by Gaussian elimination with partial pivoting.
// Four unknowns, so the cost is irrelevant and the clarity is worth more than a
// decomposition.
func ols(X [][]float64, y []float64) ([]float64, error) {
	k := len(X[0])
	A := make([][]float64, k)
	for a := range A {
		A[a] = make([]float64, k+1)
		for b := 0; b < k; b++ {
			var s float64
			for i := range X {
				s += X[i][a] * X[i][b]
			}
			A[a][b] = s
		}
		var s float64
		for i := range X {
			s += X[i][a] * y[i]
		}
		A[a][k] = s
	}
	for c := 0; c < k; c++ {
		p := c
		for r := c + 1; r < k; r++ {
			if math.Abs(A[r][c]) > math.Abs(A[p][c]) {
				p = r
			}
		}
		A[c], A[p] = A[p], A[c]
		if math.Abs(A[c][c]) < 1e-12 {
			return nil, ErrSingular
		}
		for r := 0; r < k; r++ {
			if r == c {
				continue
			}
			f := A[r][c] / A[c][c]
			for cc := c; cc <= k; cc++ {
				A[r][cc] -= f * A[c][cc]
			}
		}
	}
	beta := make([]float64, k)
	for i := 0; i < k; i++ {
		beta[i] = A[i][k] / A[i][i]
	}
	return beta, nil
}

func toCoef(beta []float64) Coefficients {
	return Coefficients{
		Constant: math.Exp(beta[0]),
		Density:  beta[1],
		Anchor:   beta[2],
		Pop:      beta[3],
	}
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// Fit fits the model and scores it LEAVE-ONE-OUT.
//
// In-sample error is not reported at all, deliberately. With four coefficients
// and fewer than a hundred rows an in-sample number only demonstrates that the
// model can memorise; it would make every refit look like an improvement and
// the promotion guardrail meaningless.
func Fit(obs []Observation) (Result, error) {
	obs = usable(obs)
	if len(obs) < MinObservations {
		return Result{}, ErrTooFewObservations
	}

	X, y := design(obs)
	beta, err := ols(X, y)
	if err != nil {
		return Result{}, err
	}

	var sumErr, sumBase float64
	preds := make([]float64, len(obs))
	actual := make([]float64, len(obs))
	for i := range obs {
		trX := make([][]float64, 0, len(obs)-1)
		trY := make([]float64, 0, len(obs)-1)
		trFill := make([]float64, 0, len(obs)-1)
		for j := range obs {
			if j == i {
				continue
			}
			trX = append(trX, X[j])
			trY = append(trY, y[j])
			trFill = append(trFill, y[j])
		}
		b, err := ols(trX, trY)
		if err != nil {
			return Result{}, err
		}
		p := toCoef(b).Predict(obs[i].Density, obs[i].Anchor, obs[i].Pop)
		base := math.Exp(median(trFill))
		preds[i] = p
		actual[i] = obs[i].Fill
		sumErr += math.Abs(p - obs[i].Fill)
		sumBase += math.Abs(base - obs[i].Fill)
	}

	n := float64(len(obs))
	return Result{
		Coef:        toCoef(beta),
		N:           len(obs),
		MAE:         sumErr / n,
		MAEBaseline: sumBase / n,
		RankRho:     spearman(preds, actual),
	}, nil
}

func rank(v []float64) []float64 {
	idx := make([]int, len(v))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return v[idx[a]] < v[idx[b]] })
	r := make([]float64, len(v))
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && v[idx[j+1]] == v[idx[i]] {
			j++
		}
		avg := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			r[idx[k]] = avg
		}
		i = j + 1
	}
	return r
}

func spearman(a, b []float64) float64 {
	if len(a) < 3 || len(a) != len(b) {
		return 0
	}
	ra, rb := rank(a), rank(b)
	var ma, mb float64
	for i := range ra {
		ma += ra[i]
		mb += rb[i]
	}
	ma /= float64(len(ra))
	mb /= float64(len(rb))
	var num, da, db float64
	for i := range ra {
		num += (ra[i] - ma) * (rb[i] - mb)
		da += (ra[i] - ma) * (ra[i] - ma)
		db += (rb[i] - mb) * (rb[i] - mb)
	}
	if da == 0 || db == 0 {
		return 0
	}
	return num / math.Sqrt(da*db)
}
