package handlers

import (
	"encoding/json"
	"log"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"

	"ropacal-backend/internal/orgdb"
)

// PHASE 0 of the placement learning loop.
//
// The recommender scores candidates, returns the winners, and forgets everything
// else. That makes the hand-tuned exponents in the site score
// (density^0.4 · anchor^0.3 · fill^0.2 · pop^0.1) permanently unverifiable: no
// record exists of what a candidate scored at decision time, so nothing can ever
// be regressed against what it actually yielded.
//
// This writes that record. `ropacal-placement` (the Python sidecar repo) reads
// `placement_decisions` to re-fit those exponents from realized fill — turning
// them from guesses into estimates. Nothing here changes what is recommended;
// it only makes the decision auditable after the fact.
//
// WHY THE REJECTS MATTER AS MUCH AS THE WINNERS. Training only on placed bins
// learns from spots we already believed in, and can never discover a gate that
// throws away good locations. That failure has already happened once here: the
// 2026-07 calibration found the ESRI income gate pointed the WRONG way
// (ρ = −0.15), filtering out good sites. Without the losing shortlist that class
// of error is invisible, and propensity/selection-bias correction is impossible.
//
// FAILURE POLICY: best-effort, never blocking. A logging failure must not cost a
// user their recommendations — the whole feature is an offline learning aid.

// placementModelVersion marks rows produced by the hand-tuned formula. NULL is
// reserved by the Python contract for "legacy, pre-logging"; 0 means "the
// hand-tuned v2 site score", so re-fits can separate eras.
const placementModelVersion = 0

// placementDecision is one candidate's frozen decision-time record.
type placementDecision struct {
	Lat, Lng     float64
	Score        float64
	Outcome      string // "placed" | "rejected"
	RejectReason string // gate that dropped it; "" when placed
	Features     placementFeatures
}

// placementFeatures is the FROZEN feature vector. These five keys and their
// spelling are a contract with ropacal_placement/features.py (FEATURE_SPEC) —
// a rename on either side silently desyncs training. Change them together and
// bump placementModelVersion.
type placementFeatures struct {
	RetailDensity  float64 `json:"retail_density"`  // errand-retail POIs nearby (calibration ρ=+0.39, lead signal)
	AnchorStrength float64 `json:"anchor_strength"` // 0 / 0.7 / 1.0 tiered anchor tenant (ρ=+0.35)
	// CONTRACT DIVERGENCE, deliberate and flagged. Python names this
	// "daytime_pop" (ESRI daytime population). The Go request path does not
	// compute that — ESRI daytime-worker ratio was measured at ρ=−0.20 in the
	// 2026-07 calibration and dropped. What is written here is the RAW
	// residential population of the candidate's ZIP, which is the `pop` term the
	// live formula actually uses (ρ=+0.20, borderline). Raw, not the normalized
	// 0.3–1.0 score, because features.py log-transforms this column and a
	// normalized value would make the fitted coefficient meaningless.
	// Rename on the Python side, or source real daytime population, before
	// treating this feature as what its name claims.
	DaytimePop      float64 `json:"daytime_pop"`
	DistNearestBinM float64 `json:"dist_nearest_bin_mi"` // MILES — whitespace / anti-cannibalization
	MedianIncome    float64 `json:"median_income"`       // unconstrained: ρ=−0.15 and confounded, let the data decide
}

// logPlacementDecisions records one recommendation run.
//
// Runs on the caller's goroutine deliberately: the org-bound handle is scoped to
// the request, and a detached goroutine could outlive it. The insert is a single
// multi-row statement, so the cost is one round-trip regardless of candidate
// count.
func logPlacementDecisions(db *orgdb.DB, areaLabel string, seed int64, decisions []placementDecision) {
	if db == nil || len(decisions) == 0 {
		return
	}

	now := time.Now().Unix()
	// Placeholders are built rather than using a helper so a single malformed
	// row cannot abort the batch through sqlx's rebinding.
	const cols = 11
	args := make([]interface{}, 0, len(decisions)*cols)
	sql := `INSERT INTO placement_decisions
		(id, decided_at, area_label, request_seed, candidate_lat, candidate_lng,
		 features, model_version, score_at_decision, propensity, outcome, reject_reason)
		VALUES `

	valid := 0
	for _, d := range decisions {
		// A NaN/Inf score would poison a regression far downstream and is
		// impossible to trace back. Drop the row instead.
		if math.IsNaN(d.Score) || math.IsInf(d.Score, 0) {
			continue
		}
		featJSON, err := json.Marshal(d.Features)
		if err != nil {
			continue
		}
		if valid > 0 {
			sql += ","
		}
		n := valid * cols
		sql += placeholderRow(n)
		// propensity is 1.0: selection is a deterministic argmax today. When an
		// explore slot lands, randomized picks must log their true probability —
		// causal/dr.py reads this field rather than estimating it, and a constant
		// 1.0 makes debiasing impossible (which is expected for now, not a bug).
		args = append(args,
			uuid.NewString(), now, nullIfEmpty(areaLabel), seed,
			d.Lat, d.Lng, featJSON, placementModelVersion,
			d.Score, 1.0, d.Outcome, nullIfEmpty(d.RejectReason),
		)
		valid++
	}
	if valid == 0 {
		return
	}

	if _, err := db.Exec(sql, args...); err != nil {
		// Best-effort by design: the user already has their recommendations.
		log.Printf("⚠️  [PlacementLog] could not record %d decisions: %v", valid, err)
		return
	}
	placed := 0
	for _, d := range decisions {
		if d.Outcome == "placed" {
			placed++
		}
	}
	log.Printf("🧠 [PlacementLog] recorded %d decisions (%d placed, %d rejected) area=%q",
		valid, placed, valid-placed, areaLabel)
}

// placeholderRow renders ($1,$2,...,$11) offset by n.
func placeholderRow(n int) string {
	out := "("
	for i := 1; i <= 11; i++ {
		if i > 1 {
			out += ","
		}
		out += "$" + strconv.Itoa(n+i)
	}
	return out + ")"
}

// nullIfEmpty keeps empty strings out of nullable columns, so "no reason" reads
// as NULL rather than "" in the training data.
func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
