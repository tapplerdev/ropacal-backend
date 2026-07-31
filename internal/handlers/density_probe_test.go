package handlers

import (
	"math"
	"testing"
)

// The whole reason for the probe: the live curve normalises with maxPOI=20 and
// the API page was ALSO 20, so everything at or above 20 collapsed to one value.
// Ranking is about differences, so tied candidates carry no information.
func TestDensityScoreAt_SaturatesAtItsCeiling(t *testing.T) {
	if got := densityScoreAt(20, liveDensityCeiling); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("count=20 scored %.4f, want exactly 1.0 — this is the ceiling", got)
	}
	// Everything past the ceiling is indistinguishable. This is the defect.
	for _, n := range []int{21, 35, 60, 100} {
		if got := densityScoreAt(n, liveDensityCeiling); got != 1.0 {
			t.Errorf("count=%d scored %.4f, want 1.0 (clamped) — the fixture no longer models the bug", n, got)
		}
	}
	// A wider ceiling keeps them apart, which is the point of measuring.
	a, b := densityScoreAt(20, 60), densityScoreAt(60, 60)
	if !(a < b) {
		t.Errorf("with ceiling 60: score(20)=%.4f is not below score(60)=%.4f", a, b)
	}
}

func TestDensityScoreAt_MonotonicAndBounded(t *testing.T) {
	prev := -1.0
	for n := 0; n <= 60; n++ {
		got := densityScoreAt(n, 60)
		if got < prev {
			t.Fatalf("not monotonic at n=%d: %.4f < %.4f", n, got, prev)
		}
		if got < 0 || got > 1 {
			t.Fatalf("n=%d scored %.4f, outside [0,1]", n, got)
		}
		prev = got
	}
	// A zero or negative ceiling must not divide by zero or NaN.
	if got := densityScoreAt(5, 0); got != 0 {
		t.Errorf("ceiling 0 scored %.4f, want 0", got)
	}
}

// The summary is what decides "theoretical or biting", so its counters have to
// be right or the verdict is worse than no verdict.
func TestSummarizeDensityCeiling(t *testing.T) {
	base := []int{3, 20, 12, 20, 1}
	full := []int{3, 47, 12, 55, 1} // two candidates carry retail the score never saw
	trunc := []bool{false, false, false, true, false}

	st := summarizeDensityCeiling(base, full, trunc)

	if st.Candidates != 5 {
		t.Errorf("Candidates = %d, want 5", st.Candidates)
	}
	if st.WithHeadroom != 2 {
		t.Errorf("WithHeadroom = %d, want 2 (indices 1 and 3)", st.WithHeadroom)
	}
	if st.AtCeiling != 2 {
		t.Errorf("AtCeiling = %d, want 2 (the two 20s already saturate)", st.AtCeiling)
	}
	if st.Truncated != 1 {
		t.Errorf("Truncated = %d, want 1", st.Truncated)
	}
	if st.MaxFull != 55 {
		t.Errorf("MaxFull = %d, want 55", st.MaxFull)
	}
	if st.MedianBase != 12 || st.MedianFull != 12 {
		t.Errorf("medians = %d/%d, want 12/12", st.MedianBase, st.MedianFull)
	}
}

// A run where nothing exceeds the window must report clean, or the probe cries
// wolf and gets ignored.
func TestSummarizeDensityCeiling_NoHeadroomIsClean(t *testing.T) {
	base := []int{2, 5, 9}
	st := summarizeDensityCeiling(base, []int{2, 5, 9}, []bool{false, false, false})
	if st.WithHeadroom != 0 || st.AtCeiling != 0 || st.Truncated != 0 {
		t.Errorf("clean run reported headroom=%d ceiling=%d trunc=%d, want all zero",
			st.WithHeadroom, st.AtCeiling, st.Truncated)
	}
}

// medianInt must not mutate its input — the caller reuses those slices.
func TestMedianInt_DoesNotMutateCaller(t *testing.T) {
	xs := []int{9, 1, 5}
	_ = medianInt(xs)
	if xs[0] != 9 || xs[1] != 1 || xs[2] != 5 {
		t.Errorf("input reordered to %v — the caller's slice was sorted underneath it", xs)
	}
	if medianInt(nil) != 0 {
		t.Error("nil input must be 0, not a panic")
	}
}

// The scored window must stay at the OLD limit while the fetch widens, or this
// stops being a comparison and becomes an uncontrolled change.
func TestBrowseLimits_ScoringWindowUnchanged(t *testing.T) {
	if browseBaselineLimit != 20 {
		t.Errorf("baseline window is %d — it must stay 20 so runs stay comparable with every run before it",
			browseBaselineLimit)
	}
	if browseFetchLimit <= browseBaselineLimit {
		t.Errorf("fetch limit %d does not exceed the baseline %d — there is no headroom to observe",
			browseFetchLimit, browseBaselineLimit)
	}
	if liveDensityCeiling != 20.0 {
		t.Errorf("liveDensityCeiling is %.0f but the v2 scorer uses maxPOI=20 — the probe has drifted from the scorer",
			liveDensityCeiling)
	}
}
