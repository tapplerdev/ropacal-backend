package handlers

import (
	"math"
	"testing"
)

// The defect this whole area exists to fix, frozen as a test.
//
// densityScore used to normalise with maxPOI=20 while the API page was ALSO 20,
// so the count could never exceed the ceiling and the curve never separated
// anything near the top. Then measurement showed the real problem was worse:
// the 20-item page is sorted by DISTANCE, so the count was mostly "how many of
// the nearest 20 things happened to be shops" — median 9 where 23 existed, and
// 28% of candidate pairs ordered wrongly against the truth.
//
// Scoring now reads the full window, so the ceiling has to clear the real range
// or the feature goes from noisy to flat.
func TestDensityCurve_SeparatesRealCounts(t *testing.T) {
	// Measured on live Toronto candidates.
	const observedMedian, observedMax = 23, 75 // live Toronto, 2026-07-31

	// Under the OLD ceiling every one of those clamps to 1.0 — indistinguishable.
	if densityScoreAt(observedMedian, 20) != 1.0 || densityScoreAt(observedMax, 20) != 1.0 {
		t.Fatal("fixture no longer models the old bug: real counts should have clamped at ceiling 20")
	}

	// Under the live ceiling they must stay apart, in the right order.
	lo := densityScoreAt(9, liveDensityCeiling)
	mid := densityScoreAt(observedMedian, liveDensityCeiling)
	hi := densityScoreAt(observedMax, liveDensityCeiling)
	if !(lo < mid && mid < hi) {
		t.Errorf("real counts not separated: 9=%.3f %d=%.3f %d=%.3f", lo, observedMedian, mid, observedMax, hi)
	}
	// And the top of the real range must not already be pinned, or the next
	// denser location found is indistinguishable from this one.
	if hi >= 1.0 {
		t.Errorf("the observed maximum (%d) already saturates at %.3f — the ceiling is too low", observedMax, hi)
	}
}

func TestDensityScoreAt_MonotonicAndBounded(t *testing.T) {
	prev := -1.0
	for n := 0; n <= 120; n++ {
		got := densityScoreAt(n, liveDensityCeiling)
		if got < prev {
			t.Fatalf("not monotonic at n=%d: %.4f < %.4f", n, got, prev)
		}
		if got < 0 || got > 1 {
			t.Fatalf("n=%d scored %.4f, outside [0,1]", n, got)
		}
		prev = got
	}
	// Past the ceiling it must clamp rather than exceed 1.0.
	if got := densityScoreAt(1000, liveDensityCeiling); got != 1.0 {
		t.Errorf("n=1000 scored %.4f, want a clamped 1.0", got)
	}
	// A zero or negative ceiling must not divide by zero or return NaN.
	if got := densityScoreAt(5, 0); got != 0 || math.IsNaN(got) {
		t.Errorf("ceiling 0 scored %v, want 0", got)
	}
}

// The summary reports whether the old window was losing information. Its
// counters have to be right or the verdict is worse than no verdict.
func TestSummarizeDensityCeiling(t *testing.T) {
	base := []int{3, 20, 12, 20, 1}
	full := []int{3, 47, 12, 61, 1} // two candidates carry retail the old window missed
	trunc := []bool{false, false, false, true, false}

	st := summarizeDensityCeiling(base, full, trunc)

	if st.Candidates != 5 {
		t.Errorf("Candidates = %d, want 5", st.Candidates)
	}
	if st.WithHeadroom != 2 {
		t.Errorf("WithHeadroom = %d, want 2 (indices 1 and 3)", st.WithHeadroom)
	}
	// AtCeiling is measured against the LIVE ceiling, so only the count that
	// actually reaches it counts — 61 does, the 20s no longer do.
	if st.AtCeiling != 0 {
		t.Errorf("AtCeiling = %d, want 0 — base counts of 20 are well under a ceiling of %.0f",
			st.AtCeiling, liveDensityCeiling)
	}
	if st.Truncated != 1 {
		t.Errorf("Truncated = %d, want 1", st.Truncated)
	}
	if st.MaxFull != 61 {
		t.Errorf("MaxFull = %d, want 61", st.MaxFull)
	}
	if st.MedianBase != 12 || st.MedianFull != 12 {
		t.Errorf("medians = %d/%d, want 12/12", st.MedianBase, st.MedianFull)
	}
}

// A run where nothing exceeded the old window must report clean, or the probe
// cries wolf and gets ignored.
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

// The probe reads the same ceiling the scorer uses. If they drift, the probe's
// verdicts describe a scorer that no longer exists.
func TestLimits_ProbeAndScorerAgree(t *testing.T) {
	if browseFetchLimit <= browseBaselineLimit {
		t.Errorf("fetch limit %d does not exceed the old window %d — nothing to compare against",
			browseFetchLimit, browseBaselineLimit)
	}
	// The ceiling must be the largest OBSERVABLE count, not a hand-picked number.
	// Picking one by eye was wrong twice: 20 could never be reached, and 60 was
	// exceeded by a live count of 75 within a single run. Anything below the
	// fetch limit clamps real candidates together and destroys their order.
	if liveDensityCeiling != float64(browseFetchLimit) {
		t.Errorf("ceiling %.0f != fetch limit %d — counts between them would clamp to an identical 1.0",
			liveDensityCeiling, browseFetchLimit)
	}
	// It must also not be so high that ordinary locations bunch near zero.
	if densityScoreAt(9, liveDensityCeiling) < 0.4 {
		t.Errorf("a typical count of 9 scores %.3f — the ceiling is high enough to flatten the low end",
			densityScoreAt(9, liveDensityCeiling))
	}
}
