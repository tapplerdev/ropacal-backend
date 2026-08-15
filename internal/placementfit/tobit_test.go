package placementfit

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// synthCensored generates intervals from a KNOWN model and then censors them
// the way reality does: a rate above `cap` is only observed AS `cap`, because
// the bin hit 100% before the driver arrived.
//
// Censoring is therefore concentrated in the fastest sites, which is the whole
// reason ordinary least squares goes wrong here — exactly the pattern measured
// on the live fleet (censored intervals median 13.3 %/day vs 3.99 uncensored).
func synthCensored(nBins, perBin int, c Coefficients, noise, cap float64, seed int64) []CensoredObservation {
	rng := rand.New(rand.NewSource(seed))
	anchors := []float64{1.0, 0.7, 0.15}
	var out []CensoredObservation
	for b := 0; b < nBins; b++ {
		d := 0.2 + rng.Float64()*0.7
		a := anchors[rng.Intn(len(anchors))]
		p := 0.3 + rng.Float64()*0.7
		id := fmt.Sprintf("bin-%d", b)
		for k := 0; k < perBin; k++ {
			true_ := c.Predict(d, a, p) * math.Exp(rng.NormFloat64()*noise)
			rate, cens := true_, false
			if true_ > cap {
				rate, cens = cap, true // observed only as the bound
			}
			out = append(out, CensoredObservation{
				BinID: id, Density: d, Anchor: a, Pop: p, Rate: rate, Censored: cens,
			})
		}
	}
	return out
}

// THE POINT OF THE WHOLE FILE. On censored data, OLS must be visibly biased and
// the censored fit must be closer to the truth. If this ever fails, the Tobit
// path is not earning its complexity and should be deleted rather than trusted.
func TestCensoredFitBeatsOLSOnCensoredData(t *testing.T) {
	truth := Coefficients{Constant: 9.0, Density: 0.90, Anchor: 0.30, Pop: 0.15}
	// cap chosen to censor ~20-25% of intervals, matching the live fleet
	// (200/913 = 21.9%). A cap that censors almost nothing would make this test
	// pass on a margin too thin to mean anything.
	obs := synthCensored(120, 8, truth, 0.35, 5.0, 11)

	var nCens int
	for _, o := range obs {
		if o.Censored {
			nCens++
		}
	}
	rate := 100 * float64(nCens) / float64(len(obs))
	t.Logf("censoring rate: %.1f%% (%d/%d) — live fleet is 21.9%%", rate, nCens, len(obs))
	if rate < 8 || rate > 45 {
		t.Fatalf("test setup wrong: censoring rate %.1f%% is not representative", rate)
	}

	// OLS on the same data, censored values taken at face value.
	plain := make([]Observation, len(obs))
	for i, o := range obs {
		plain[i] = Observation{BinID: o.BinID, Density: o.Density, Anchor: o.Anchor, Pop: o.Pop, Fill: o.Rate}
	}
	olsRes, err := Fit(plain)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	tob, nBins, err := FitCensored(obs)
	if err != nil {
		t.Fatalf("FitCensored: %v", err)
	}

	olsErr := math.Abs(olsRes.Coef.Density - truth.Density)
	tobErr := math.Abs(tob.Density - truth.Density)
	t.Logf("bins=%d truth density=%.3f | OLS %.3f (err %.3f) | Tobit %.3f (err %.3f)",
		nBins, truth.Density, olsRes.Coef.Density, olsErr, tob.Density, tobErr)

	if olsErr <= tobErr {
		t.Errorf("censored fit did not improve on OLS: OLS err %.4f, Tobit err %.4f", olsErr, tobErr)
	}
	// And OLS should be biased DOWNWARD specifically, because censoring removes
	// the top of the distribution where the features are strongest.
	if olsRes.Coef.Density >= truth.Density {
		t.Errorf("expected OLS to under-estimate the density exponent, got %.3f vs truth %.3f",
			olsRes.Coef.Density, truth.Density)
	}
}

// With no censoring the two must agree — otherwise the Tobit path is adding a
// distortion of its own rather than removing one.
func TestCensoredFitMatchesOLSWhenNothingIsCensored(t *testing.T) {
	truth := Coefficients{Constant: 9.0, Density: 0.60, Anchor: 0.25, Pop: 0.12}
	obs := synthCensored(120, 6, truth, 0.25, math.Inf(1), 12) // cap=Inf => never censored

	plain := make([]Observation, len(obs))
	for i, o := range obs {
		plain[i] = Observation{BinID: o.BinID, Density: o.Density, Anchor: o.Anchor, Pop: o.Pop, Fill: o.Rate}
	}
	olsRes, _ := Fit(plain)
	tob, _, err := FitCensored(obs)
	if err != nil {
		t.Fatalf("FitCensored: %v", err)
	}
	if math.Abs(tob.Density-olsRes.Coef.Density) > 0.05 {
		t.Errorf("uncensored data should agree: OLS %.3f vs Tobit %.3f",
			olsRes.Coef.Density, tob.Density)
	}
}

func TestCensoredFitRefusesTooFewBins(t *testing.T) {
	obs := synthCensored(MinObservations-1, 5,
		Coefficients{Constant: 9, Density: 0.5, Anchor: 0.2, Pop: 0.1}, 0.2, 12.0, 13)
	if _, _, err := FitCensored(obs); err == nil {
		t.Fatal("expected refusal below the bin minimum")
	}
}

// Significance must be counted in BINS, not intervals — intervals from one bin
// share a site and are not independent draws.
func TestEffectiveNCountsBinsNotIntervals(t *testing.T) {
	obs := synthCensored(50, 10,
		Coefficients{Constant: 9, Density: 0.5, Anchor: 0.2, Pop: 0.1}, 0.2, 12.0, 14)
	if len(obs) != 500 {
		t.Fatalf("expected 500 intervals, got %d", len(obs))
	}
	if got := EffectiveN(obs); got != 50 {
		t.Errorf("EffectiveN = %d, want 50 (bins, not intervals)", got)
	}
}
