package placementfit

import (
	"errors"
	"math"
	"math/rand"
	"testing"
)

// synth builds observations from a KNOWN model, so a successful fit has to
// recover coefficients we already know the answer to. Without this the fit
// could be quietly wrong in a way no amount of staring at real data reveals.
func synth(n int, c Coefficients, noise float64, seed int64) []Observation {
	rng := rand.New(rand.NewSource(seed))
	anchors := []float64{1.0, 0.7, 0.15}
	obs := make([]Observation, n)
	for i := range obs {
		d := 0.2 + rng.Float64()*0.7
		a := anchors[rng.Intn(len(anchors))]
		p := 0.3 + rng.Float64()*0.7
		fill := c.Predict(d, a, p) * math.Exp(rng.NormFloat64()*noise)
		obs[i] = Observation{BinID: "b", Density: d, Anchor: a, Pop: p, Fill: fill}
	}
	return obs
}

func TestRecoversKnownCoefficients(t *testing.T) {
	want := Coefficients{Constant: 9.0, Density: 0.36, Anchor: 0.11, Pop: 0.10}
	res, err := Fit(synth(400, want, 0.01, 1))
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	check := func(name string, got, exp float64) {
		if math.Abs(got-exp) > 0.05 {
			t.Errorf("%s = %.4f, want ~%.4f", name, got, exp)
		}
	}
	check("constant", res.Coef.Constant, want.Constant)
	check("density", res.Coef.Density, want.Density)
	check("anchor", res.Coef.Anchor, want.Anchor)
	check("pop", res.Coef.Pop, want.Pop)
}

func TestRefusesTooFewObservations(t *testing.T) {
	obs := synth(MinObservations-1, Coefficients{Constant: 9, Density: 0.3, Anchor: 0.1, Pop: 0.1}, 0.1, 2)
	if _, err := Fit(obs); !errors.Is(err, ErrTooFewObservations) {
		t.Fatalf("expected ErrTooFewObservations, got %v", err)
	}
}

// Rows the log transform cannot accept must be dropped, not crash the fit and
// not silently become -Inf coefficients.
func TestDropsUnusableRows(t *testing.T) {
	obs := synth(60, Coefficients{Constant: 9, Density: 0.3, Anchor: 0.1, Pop: 0.1}, 0.05, 3)
	obs = append(obs,
		Observation{Density: 0.5, Anchor: 0.7, Pop: 0.5, Fill: 0},  // zero fill
		Observation{Density: 0, Anchor: 0.7, Pop: 0.5, Fill: 5},    // zero density
		Observation{Density: 0.5, Anchor: 0.7, Pop: 0.5, Fill: -2}, // negative fill
	)
	res, err := Fit(obs)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if res.N != 60 {
		t.Errorf("N = %d, want 60 (three unusable rows dropped)", res.N)
	}
	if math.IsNaN(res.Coef.Density) || math.IsInf(res.Coef.Density, 0) {
		t.Errorf("unusable rows poisoned the fit: %+v", res.Coef)
	}
}

// On data generated from a real relationship the fit must beat the median.
func TestBeatsBaselineOnRealSignal(t *testing.T) {
	res, err := Fit(synth(200, Coefficients{Constant: 9, Density: 0.9, Anchor: 0.4, Pop: 0.2}, 0.05, 4))
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if !res.BeatsBaseline() {
		t.Errorf("expected fit to beat baseline: MAE=%.3f baseline=%.3f", res.MAE, res.MAEBaseline)
	}
}

// THE GUARDRAIL TEST. On pure noise the fit must NOT claim to beat the median,
// because that is the only thing standing between a promoted model and one that
// has learned nothing. If this ever passes trivially the promotion check is
// worthless.
func TestDoesNotBeatBaselineOnNoise(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	obs := make([]Observation, 150)
	for i := range obs {
		obs[i] = Observation{
			Density: 0.2 + rng.Float64()*0.7,
			Anchor:  []float64{1.0, 0.7, 0.15}[rng.Intn(3)],
			Pop:     0.3 + rng.Float64()*0.7,
			Fill:    2 + rng.Float64()*13, // independent of every feature
		}
	}
	res, err := Fit(obs)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if res.MAE < res.MAEBaseline*0.95 {
		t.Errorf("fit claimed a real improvement on pure noise: MAE=%.3f baseline=%.3f",
			res.MAE, res.MAEBaseline)
	}
}

func TestPredictHandlesDegenerateInput(t *testing.T) {
	c := Coefficients{Constant: 9, Density: 0.36, Anchor: 0.11, Pop: 0.10}
	if v := c.Predict(0, 0, 0); v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		t.Errorf("Predict(0,0,0) = %v, want a finite positive number", v)
	}
}

// The significance bar must MOVE WITH n. A fixed threshold is what made the
// Python signalcheck print "proceed" for a rho its sample could not support.
func TestSignificanceBarShrinksWithN(t *testing.T) {
	small := Result{N: 40, RankRho: 0.30}
	large := Result{N: 400, RankRho: 0.30}
	if !(small.SignificanceBar() > large.SignificanceBar()) {
		t.Fatalf("bar must be stricter at small n: n=40 %.3f, n=400 %.3f",
			small.SignificanceBar(), large.SignificanceBar())
	}
	// Same rho: not credible on 40 rows, clearly credible on 400.
	if small.SignalIsReal() {
		t.Errorf("rho=0.30 at n=40 (bar %.3f) must not count as real", small.SignificanceBar())
	}
	if !large.SignalIsReal() {
		t.Errorf("rho=0.30 at n=400 (bar %.3f) should count as real", large.SignificanceBar())
	}
}
