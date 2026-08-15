package handlers

import (
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"ropacal-backend/internal/orgdb"
	"ropacal-backend/internal/placementfit"
)

// Predicted-yield scoring — an OPT-IN alternative to the 0-10 site score.
//
// # WHY THIS EXISTS
//
// The live score is a Cobb-Douglas index scaled to 0-10:
//
//	density^0.4 · anchor^0.3 · fill^0.2 · pop^0.1 · 10
//
// It ranks, but it cannot be WRONG. "8.5" makes no claim about the world, so no
// placement outcome can ever contradict it and no feedback loop can close around
// it. Two defects follow from that, and this file addresses both.
//
//  1. UNFALSIFIABLE OUTPUT. Predicting fill rate in %/day instead makes every
//     recommendation a bet that settles once the bin has a few checks. The
//     exponents stop being permanent guesses: taking logs turns the same
//     multiplicative form into a linear regression, so they are fitted rather
//     than chosen.
//
//  2. CIRCULARITY. The fill term scores a candidate using the realized rate of
//     the NEAREST EXISTING BIN. That is a fact about where the fleet already is,
//     not about the site, so the model can only discover places resembling
//     places already chosen. It is dropped here. (The same circularity was
//     already recognised and removed from the growth ranker.)
//
// HONEST LIMITS — read before trusting a number this produces.
//
// Fitted leave-one-out on 78 bins with a measured current-pitch fill rate:
// MAE 2.14 %/day against 2.25 for predicting the median — a 4.7% improvement.
// Predictions compress into roughly 5-8 %/day while reality spans 2.4-15.5, so
// the model systematically under-calls strong sites and over-calls weak ones.
// That is what a ~6%-of-variance signal looks like when it is forced to state a
// number, and it is the honest picture the 0-10 scale concealed. Treat the
// output as "roughly typical, or notably below typical" and nothing finer.
//
// Kept behind PLACEMENT_PREDICT_FILL so production ordering is untouched until
// somebody deliberately turns it on and compares.
const (
	// Coefficients from the log-space fit; see scratchpad/fit_model.py.
	// ln(fill) = ln(C) + b_density·ln(density) + b_anchor·ln(anchor) + b_pop·ln(pop)
	predictConstant = 9.274
	predictBDensity = 0.364
	predictBAnchor  = 0.109
	predictBPop     = 0.108

	// predictFitN and predictFitMAE record what the coefficients were earned on,
	// so a future reader can tell whether they are still credible at a larger
	// fleet size rather than assuming.
	predictFitN   = 78
	predictFitMAE = 2.14
)

// predictedFillRateEnabled reports whether to score by predicted yield.
func predictedFillRateEnabled() bool {
	return os.Getenv("PLACEMENT_PREDICT_FILL") == "1"
}

// predictedFillCutoff is the minimum predicted %/day worth recommending.
//
// This replaces the arbitrary 4.0 score bar with a number an operator can reason
// about: how fast must a bin fill to justify the servicing trip. Because the
// predictions are compressed (see above) this gate is GENTLER than the old one
// in practice — most candidates clear a 5%/day bar. That is a truthful
// consequence of a weak model, not a reason to pick a scarier threshold.
func predictedFillCutoff() float64 {
	if v := os.Getenv("PLACEMENT_PREDICT_CUTOFF"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return 5.0
}

// predictFillRate returns expected fill in percentage points per day.
//
// Deliberately takes the SAME normalized inputs as the live score, so the two
// can be compared on identical features and any difference is the scoring
// change alone. The fill term is absent by design — that is change #2.
//
// Coefficients come from the most recent PROMOTED fit when one exists, and fall
// back to the constants above otherwise. The fallback is not a formality: a
// fresh database, a failed refit, or an organization too small to fit all land
// there, and none of them should break scoring.
func predictFillRate(db *orgdb.DB, densityScore, anchorScore, popVal float64) float64 {
	c := placementfit.Coefficients{
		Constant: predictConstant,
		Density:  predictBDensity,
		Anchor:   predictBAnchor,
		Pop:      predictBPop,
	}
	if fitted, ok := loadActiveCoefficients(db); ok && fitted != nil {
		c = *fitted
	}
	pred := c.Predict(densityScore, anchorScore, popVal)
	if math.IsNaN(pred) || math.IsInf(pred, 0) {
		return 0
	}
	return math.Round(pred*10) / 10
}

// densityScoreFromCount is the single definition of the density transform.
// The scoring loop and the refit's feature gathering both call it, so a change
// cannot apply to one and not the other — which would fit coefficients against
// features no candidate is ever scored with.
func densityScoreFromCount(retailCount int) float64 {
	v := math.Log(1+float64(retailCount)) / math.Log(1+float64(browseFetchLimit))
	if v > 1.0 {
		return 1.0
	}
	if v < 0 {
		return 0
	}
	return v
}

// Coefficient cache. The scoring loop runs per candidate, so this must not be a
// query per call; the refit is weekly, so a short TTL is ample and staleness is
// bounded by it. invalidateCoefficientCache() makes a promotion visible at once
// rather than up to a TTL later.
var (
	coefMu     sync.RWMutex
	coefCache  = map[string]*placementfit.Coefficients{}
	coefLoaded = map[string]time.Time{}
)

const coefCacheTTL = 15 * time.Minute

func invalidateCoefficientCache() {
	coefMu.Lock()
	defer coefMu.Unlock()
	coefCache = map[string]*placementfit.Coefficients{}
	coefLoaded = map[string]time.Time{}
}

// loadActiveCoefficients returns the promoted fit for the bound tenant, or
// (nil, false) when there is none. Cached per organization: the cache key must
// be the org, or one tenant's fitted model would be served to another.
func loadActiveCoefficients(db *orgdb.DB) (*placementfit.Coefficients, bool) {
	if db == nil {
		return nil, false
	}
	var org string
	if err := db.Get(&org, `SELECT current_setting('app.org_id', true)`); err != nil || org == "" {
		return nil, false
	}

	coefMu.RLock()
	if at, ok := coefLoaded[org]; ok && time.Since(at) < coefCacheTTL {
		c := coefCache[org]
		coefMu.RUnlock()
		return c, c != nil
	}
	coefMu.RUnlock()

	var row struct {
		Constant float64 `db:"coef_constant"`
		Density  float64 `db:"coef_density"`
		Anchor   float64 `db:"coef_anchor"`
		Pop      float64 `db:"coef_pop"`
		NBins    int     `db:"n_bins"`
	}
	err := db.Get(&row, `SELECT coef_constant, coef_density, coef_anchor, coef_pop, n_bins
		FROM placement_model WHERE is_active LIMIT 1`)

	coefMu.Lock()
	defer coefMu.Unlock()
	coefLoaded[org] = time.Now()
	if err != nil {
		// No promoted fit, or the table does not exist yet. Cache the absence
		// too, so a fleet without one does not query on every candidate.
		coefCache[org] = nil
		return nil, false
	}
	c := &placementfit.Coefficients{
		Constant: row.Constant, Density: row.Density,
		Anchor: row.Anchor, Pop: row.Pop,
	}
	coefCache[org] = c
	return c, true
}
