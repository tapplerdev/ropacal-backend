package placementfit

import (
	"errors"
	"math"
)

// Censored regression (Tobit) for the fill-rate label.
//
// THE PROBLEM. A fill rate is measured between two checks. When the later check
// reads exactly 100%, the bin filled at SOME point in that window — possibly
// days before the driver arrived. The observed rate is therefore a LOWER BOUND,
// not a measurement.
//
// This is not a rounding detail, because the censoring is not random. Measured
// on the live fleet (2026-08-14): 200 of 913 usable intervals end at 100%, and
// their median observed rate is 13.30 %/day against 3.99 %/day for uncensored
// intervals. Censoring concentrates in the FASTEST sites, so ordinary least
// squares — which treats a lower bound as if it were exact — is biased downward
// precisely where the model most needs to be right.
//
// WHY NOT JUST DROP THEM. That was tried in 2026-07 and made things worse
// (rho +0.167 -> +0.090). A bin found full is the strongest available evidence
// that a site is good; deleting those rows deletes the signal along with the
// bias.
//
// THE FIX. Maximum likelihood where each observation contributes according to
// what is actually known about it:
//
//	uncensored y : the normal density at y            — "the rate was this"
//	censored   y : the survival function above y      — "the rate was at least this"
//
// Maximised over (beta, log sigma) by Nelder-Mead. Five parameters and a smooth
// objective, so a derivative-free simplex is more than adequate and avoids
// hand-derived gradients that can be subtly wrong in ways tests do not catch.

// CensoredObservation is one measurement interval, not one bin.
//
// Interval-level on purpose: 90% of bins have at least one censored interval, so
// a per-bin flag would mark almost everything and throw away the distinction
// that matters. Features repeat across a bin's intervals; BinID records that so
// the caller can keep significance honest (see EffectiveN).
type CensoredObservation struct {
	BinID    string
	Density  float64
	Anchor   float64
	Pop      float64
	Rate     float64 // observed %/day for this interval
	Censored bool    // true when the interval ended at 100% — Rate is a lower bound
}

var ErrOptimizerFailed = errors.New("placementfit: censored fit did not converge")

// normalCDF is the standard normal CDF via the error function.
func normalCDF(z float64) float64 {
	return 0.5 * math.Erfc(-z/math.Sqrt2)
}

// tobitNegLogLik returns the negative log-likelihood for params = [b0..b3, logSigma].
//
// Works in LOG RATE space, matching the Cobb-Douglas form the rest of the
// package fits: a multiplicative model with lognormal error.
func tobitNegLogLik(params []float64, x [][]float64, y []float64, censored []bool) float64 {
	sigma := math.Exp(params[4])
	if sigma <= 0 || math.IsInf(sigma, 0) || math.IsNaN(sigma) {
		return math.Inf(1)
	}
	var nll float64
	for i := range y {
		xb := params[0] + params[1]*x[i][1] + params[2]*x[i][2] + params[3]*x[i][3]
		z := (y[i] - xb) / sigma
		if censored[i] {
			// P(true >= observed) = 1 - Phi(z). Floored so a hopeless parameter
			// vector costs a large finite penalty instead of returning -Inf and
			// stalling the simplex on a flat plateau.
			s := 1 - normalCDF(z)
			if s < 1e-12 {
				s = 1e-12
			}
			nll -= math.Log(s)
		} else {
			nll += 0.5*z*z + params[4] + 0.5*math.Log(2*math.Pi)
		}
	}
	if math.IsNaN(nll) {
		return math.Inf(1)
	}
	return nll
}

// nelderMead minimises f from an initial guess. Standard coefficients.
func nelderMead(f func([]float64) float64, start []float64, maxIter int) ([]float64, bool) {
	n := len(start)
	simplex := make([][]float64, n+1)
	vals := make([]float64, n+1)
	for i := range simplex {
		p := append([]float64(nil), start...)
		if i > 0 {
			step := 0.5 * math.Abs(p[i-1])
			if step < 0.1 {
				step = 0.1
			}
			p[i-1] += step
		}
		simplex[i] = p
		vals[i] = f(p)
	}

	centroidExcl := func(worst int) []float64 {
		c := make([]float64, n)
		for i, p := range simplex {
			if i == worst {
				continue
			}
			for j := range p {
				c[j] += p[j] / float64(n)
			}
		}
		return c
	}
	combine := func(a, b []float64, t float64) []float64 {
		out := make([]float64, n)
		for j := range out {
			out[j] = a[j] + t*(a[j]-b[j])
		}
		return out
	}

	for iter := 0; iter < maxIter; iter++ {
		// order: best..worst
		for i := 0; i < len(vals); i++ {
			for j := i + 1; j < len(vals); j++ {
				if vals[j] < vals[i] {
					vals[i], vals[j] = vals[j], vals[i]
					simplex[i], simplex[j] = simplex[j], simplex[i]
				}
			}
		}
		if math.Abs(vals[len(vals)-1]-vals[0]) < 1e-10 {
			return simplex[0], true
		}
		worst := len(simplex) - 1
		c := centroidExcl(worst)

		refl := combine(c, simplex[worst], 1.0)
		fr := f(refl)
		switch {
		case fr < vals[0]:
			exp := combine(c, simplex[worst], 2.0)
			if fe := f(exp); fe < fr {
				simplex[worst], vals[worst] = exp, fe
			} else {
				simplex[worst], vals[worst] = refl, fr
			}
		case fr < vals[worst-1]:
			simplex[worst], vals[worst] = refl, fr
		default:
			con := combine(c, simplex[worst], -0.5)
			if fc := f(con); fc < vals[worst] {
				simplex[worst], vals[worst] = con, fc
			} else {
				// shrink toward the best vertex
				for i := 1; i < len(simplex); i++ {
					for j := range simplex[i] {
						simplex[i][j] = simplex[0][j] + 0.5*(simplex[i][j]-simplex[0][j])
					}
					vals[i] = f(simplex[i])
				}
			}
		}
	}
	return simplex[0], false
}

// FitCensored fits the model treating 100%-full readings as lower bounds.
//
// Returns coefficients in the same form as Fit, so the two are interchangeable
// downstream and can be compared on identical data.
func FitCensored(obs []CensoredObservation) (Coefficients, int, error) {
	x := make([][]float64, 0, len(obs))
	y := make([]float64, 0, len(obs))
	cen := make([]bool, 0, len(obs))
	bins := map[string]struct{}{}
	for _, o := range obs {
		if o.Rate <= 0 || o.Density <= 0 || o.Anchor <= 0 || o.Pop <= 0 {
			continue
		}
		x = append(x, []float64{1, math.Log(o.Density), math.Log(o.Anchor), math.Log(o.Pop)})
		y = append(y, math.Log(o.Rate))
		cen = append(cen, o.Censored)
		bins[o.BinID] = struct{}{}
	}
	if len(bins) < MinObservations {
		return Coefficients{}, len(bins), ErrTooFewObservations
	}

	// Start from the OLS solution: it is the right neighbourhood, and starting
	// the simplex somewhere sensible matters more than the simplex settings.
	beta, err := ols(x, y)
	if err != nil {
		return Coefficients{}, len(bins), err
	}
	var ss float64
	for i := range y {
		r := y[i] - (beta[0] + beta[1]*x[i][1] + beta[2]*x[i][2] + beta[3]*x[i][3])
		ss += r * r
	}
	sigma := math.Sqrt(ss / float64(len(y)))
	if sigma <= 0 {
		sigma = 1
	}

	start := []float64{beta[0], beta[1], beta[2], beta[3], math.Log(sigma)}
	best, ok := nelderMead(func(p []float64) float64 {
		return tobitNegLogLik(p, x, y, cen)
	}, start, 20000)
	if !ok {
		return Coefficients{}, len(bins), ErrOptimizerFailed
	}

	return Coefficients{
		Constant: math.Exp(best[0]),
		Density:  best[1],
		Anchor:   best[2],
		Pop:      best[3],
	}, len(bins), nil
}

// EvaluateOnUncensored scores a model against ONLY the uncensored intervals,
// and returns (MAE, medianBaselineMAE, n).
//
// This is the only fair yardstick when comparing a censored fit to an ordinary
// one. A Tobit model predicts the LATENT rate — what the bin would have shown
// had anyone looked in time — so measuring it against observations that were
// truncated at 100% would penalise it precisely for being right. Uncensored
// intervals are unbiased measurements, so both models can be judged on them
// without favouring either.
//
// Held-out evaluation is the caller's job; this is the metric, not the split.
func EvaluateOnUncensored(c Coefficients, obs []CensoredObservation) (mae, baseline float64, n int) {
	var rates []float64
	for _, o := range obs {
		if !o.Censored && o.Rate > 0 {
			rates = append(rates, o.Rate)
		}
	}
	if len(rates) == 0 {
		return math.NaN(), math.NaN(), 0
	}
	med := median(rates)

	var sum, base float64
	for _, o := range obs {
		if o.Censored || o.Rate <= 0 {
			continue
		}
		p := c.Predict(o.Density, o.Anchor, o.Pop)
		sum += math.Abs(p - o.Rate)
		base += math.Abs(med - o.Rate)
		n++
	}
	return sum / float64(n), base / float64(n), n
}

// EffectiveN is the count of distinct BINS, not intervals.
//
// Intervals from one bin share a site and are not independent draws, so using
// the interval count for a significance bar would badly overstate confidence.
// Bin count is the conservative, honest denominator.
func EffectiveN(obs []CensoredObservation) int {
	bins := map[string]struct{}{}
	for _, o := range obs {
		bins[o.BinID] = struct{}{}
	}
	return len(bins)
}
