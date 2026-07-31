package handlers

import (
	"log"
	"math"
	"sort"
)

// Measurement for the density-ceiling question: is the errand-retail signal
// saturating before it can rank anything?
//
// The concern, from the code as it stood: HERE was asked for 20 POIs within
// 300m, and densityScore normalised with maxPOI=20 — the same number. So a
// 20-tenant strip and a 60-tenant power centre produced an identical score.
// That flattens the signal exactly among the best sites, and errand density is
// the LEAD feature (2026-07 calibration, ρ=+0.39, ahead of anchors at +0.35).
//
// Nothing here changes a score. The fetch limit was raised so the true count is
// visible; scoring still reads only the first 20 results, so this run is
// directly comparable with every run before it. These logs decide whether the
// ceiling is theoretical or biting before anything acts on it.

// densityScoreAt is the live normalisation, extracted so alternatives can be
// compared against it on identical inputs: ln(1+n) / ln(1+ceiling), clamped.
func densityScoreAt(count int, ceiling float64) float64 {
	if ceiling <= 0 {
		return 0
	}
	v := math.Log(1+float64(count)) / math.Log(1+ceiling)
	return math.Min(1.0, v)
}

// densityCeilingStats is what one recommendation run observed.
type densityCeilingStats struct {
	Candidates   int
	WithHeadroom int // more retail POIs existed beyond the scored window
	AtCeiling    int // scored count already saturates densityScore (>= 20)
	Truncated    int // a full page came back — even the raised limit may clip
	MaxFull      int
	MedianBase   int
	MedianFull   int
}

func summarizeDensityCeiling(base, full []int, truncated []bool) densityCeilingStats {
	st := densityCeilingStats{Candidates: len(base)}
	for i := range base {
		if full[i] > base[i] {
			st.WithHeadroom++
		}
		if float64(base[i]) >= liveDensityCeiling {
			st.AtCeiling++
		}
		if i < len(truncated) && truncated[i] {
			st.Truncated++
		}
		if full[i] > st.MaxFull {
			st.MaxFull = full[i]
		}
	}
	st.MedianBase = medianInt(base)
	st.MedianFull = medianInt(full)
	return st
}

func medianInt(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	c := append([]int(nil), xs...)
	sort.Ints(c)
	return c[len(c)/2]
}

// liveDensityCeiling mirrors maxPOI in the v2 site score, which is tied to the
// fetch limit — the largest count that can be observed. A test pins them
// together, because a hand-picked ceiling was wrong twice: 20 (unreachable by
// construction) and then 60 (exceeded by a live count of 75 within one run).
const liveDensityCeiling = float64(browseFetchLimit)

// logDensityCeilingReport prints the per-run verdict.
func logDensityCeilingReport(base, full []int, truncated []bool) {
	if len(base) == 0 {
		return
	}
	st := summarizeDensityCeiling(base, full, truncated)

	log.Printf("🔬 [DensityCeiling] %d candidates | median retail POIs: scored=%d actual=%d | max actual=%d",
		st.Candidates, st.MedianBase, st.MedianFull, st.MaxFull)
	log.Printf("🔬 [DensityCeiling] %d/%d candidates had MORE retail nearby than the scored window saw; "+
		"%d already saturate densityScore (count >= %.0f); %d hit the raised fetch limit",
		st.WithHeadroom, st.Candidates, st.AtCeiling, liveDensityCeiling, st.Truncated)

	if st.WithHeadroom == 0 && st.AtCeiling == 0 {
		log.Printf("🔬 [DensityCeiling] VERDICT: ceiling is THEORETICAL for this run — " +
			"no candidate had more retail than the scored window, so raising maxPOI would change nothing")
		return
	}

	// What the alternatives would have produced, on this run's real counts.
	// Ranking only cares about ORDER, so the number that matters is how many
	// candidates a curve leaves TIED at the top — ties carry no information.
	tiedNow, tiedWide := 0, 0
	for i := range base {
		if densityScoreAt(base[i], liveDensityCeiling) >= 1.0 {
			tiedNow++
		}
		if densityScoreAt(full[i], float64(st.MaxFull)) >= 1.0 {
			tiedWide++
		}
	}
	log.Printf("🔬 [DensityCeiling] tied-at-max under CURRENT curve (ceiling %.0f, scored counts): %d/%d | "+
		"under WIDE curve (ceiling %d, actual counts): %d/%d",
		liveDensityCeiling, tiedNow, st.Candidates, st.MaxFull, tiedWide, st.Candidates)
	log.Printf("🔬 [DensityCeiling] VERDICT: ceiling is BITING — %d candidates carry retail the score never counted. "+
		"Widening maxPOI and scoring the full window would separate them.", st.WithHeadroom)
}
