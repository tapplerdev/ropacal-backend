package handlers

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/orgdb"
	"ropacal-backend/internal/placementfit"
)

// Periodic refit of the placement site score from realized outcomes.
//
// This is what makes the weights move as the fleet grows, instead of being
// constants somebody typed in once. It runs as a BATCH job and writes
// coefficients to a table Go reads — never on the request path. That shape was
// chosen deliberately in 2026-07: `ortools-service` is the counter-example, a
// request-path dependency whose outage kills every shift start.
//
// WHY NOT TRAIN FROM placement_decisions. That table records what was
// RECOMMENDED, and most recommendations never become bins, so it cannot supply
// a realized fill rate for most rows. The label lives on bins that actually
// exist and have been checked. placement_decisions remains the right source for
// selection-bias correction later, when there are explore slots and real
// propensities; it is not the training set today.

// refitInterval is deliberately slow. At this fleet size the coefficients are
// noisy, and refitting often means watching them wobble in response to a
// handful of new checks rather than to any real change in the world. Weekly is
// already faster than the fleet grows.
const refitInterval = 7 * 24 * time.Hour

// refitStartupDelay lets boot settle before a job that makes ~80 HERE calls.
const refitStartupDelay = 10 * time.Minute

// StartPlacementRefitWorker runs the refit on a schedule for every active org.
//
// Loops per organization like every other worker here, because a fitted model
// is tenant-specific: one org's fleet says nothing about another's market.
func StartPlacementRefitWorker(root *sqlx.DB) {
	go func() {
		time.Sleep(refitStartupDelay)
		for {
			orgdb.ForEachActiveOrg(root, "PlacementRefit", func(d *orgdb.DB) error {
				RunPlacementRefit(d)
				return nil
			})
			time.Sleep(refitInterval)
		}
	}()
	log.Printf("🧠 [PlacementRefit] worker started (every %s, first run in %s)", refitInterval, refitStartupDelay)
}

// RunPlacementRefit gathers observations, fits, and promotes the result only if
// it beats the do-nothing baseline. Safe to call by hand.
func RunPlacementRefit(db *orgdb.DB) {
	start := time.Now()
	obs, err := gatherObservations(db)
	if err != nil {
		log.Printf("⚠️  [PlacementRefit] could not gather observations: %v", err)
		return
	}
	if len(obs) < placementfit.MinObservations {
		log.Printf("ℹ️  [PlacementRefit] %d usable bins, need %d — skipping (this is expected while the fleet is small)",
			len(obs), placementfit.MinObservations)
		return
	}

	res, err := placementfit.Fit(obs)
	if err != nil {
		log.Printf("⚠️  [PlacementRefit] fit failed: %v", err)
		return
	}

	// Which estimator produced the coefficients that end up stored. Recorded on
	// the row because "density^0.364" and "density^0.170" are both plausible
	// numbers and nothing else in the table would say which model they came from.
	estimator := "ordinary least squares"

	// Censored fit. 100%-full readings are LOWER BOUNDS, not measurements, and
	// they cluster in the fastest sites (live fleet: censored intervals median
	// 13.3 %/day vs 3.99 uncensored), so treating them as exact biases the
	// exponents downward exactly where accuracy matters most.
	//
	// Both models are scored on UNCENSORED intervals only — see
	// EvaluateOnUncensored for why any other yardstick would penalise the
	// censored fit for being correct. Whichever wins on that honest metric is
	// the one carried forward.
	if intervals := gatherCensoredObservations(db, obs); len(intervals) > 0 {
		if cen, nBins, cerr := placementfit.FitCensored(intervals); cerr != nil {
			log.Printf("⚠️  [PlacementRefit] censored fit unavailable (%v) — keeping the ordinary fit", cerr)
		} else {
			olsMAE, base, nEval := placementfit.EvaluateOnUncensored(res.Coef, intervals)
			cenMAE, _, _ := placementfit.EvaluateOnUncensored(cen, intervals)
			log.Printf("🧠 [PlacementRefit] censored fit (%d bins, %d intervals): "+
				"C=%.3f density^%.3f anchor^%.3f pop^%.3f | on %d UNCENSORED intervals: "+
				"censored MAE %.3f vs ordinary %.3f (median baseline %.3f)",
				nBins, len(intervals), cen.Constant, cen.Density, cen.Anchor, cen.Pop,
				nEval, cenMAE, olsMAE, base)
			// TWO conditions, both required.
			//
			// Beating the ordinary fit is not sufficient: on interval-level data
			// both estimators can sit above the median baseline, and adopting
			// "the better of two models that are worse than guessing" would be a
			// regression dressed up as an improvement. The censored fit has to
			// clear the same do-nothing bar everything else here clears.
			switch {
			case cenMAE >= olsMAE:
				log.Printf("   ✘ censored fit did not beat the ordinary one — keeping OLS")
			case cenMAE >= base:
				log.Printf("   ✘ censored fit beats OLS (%.3f < %.3f) but LOSES to the median "+
					"baseline (%.3f) — not adopted. Both estimators are weak on interval-level "+
					"data; this is a statement about the label, not about censoring.",
					cenMAE, olsMAE, base)
			default:
				log.Printf("   ✔ censored fit beats both the ordinary fit and the median baseline — adopting it")
				res.Coef = cen
				// The stored scorecard must describe the coefficients actually
				// stored. res.MAE/RankRho were computed leave-one-out for the OLS
				// fit; carrying them over would attach one model's accuracy to
				// another model's numbers, which is exactly the kind of quietly
				// wrong metadata nobody re-derives later.
				res.MAE, res.MAEBaseline = cenMAE, base
				res.RankRho = 0 // not computed for this estimator; 0 reads as "unknown", not "bad"
				estimator = "censored (Tobit)"
			}
		}
	}

	prev, _ := loadActiveCoefficients(db)
	// The rho is reported WITH the bar it has to clear, because a correlation
	// without its sample size is uninterpretable — this is the whole point of
	// running signalcheck, computed on every refit instead of by hand.
	signal := "NOT DISTINGUISHABLE FROM NOISE"
	if res.SignalIsReal() {
		signal = "signal is real"
	}
	log.Printf("🧠 [PlacementRefit] fitted on %d bins in %v: "+
		"C=%.3f density^%.3f anchor^%.3f pop^%.3f | LOO MAE %.2f vs baseline %.2f | "+
		"rho %+.3f vs bar %.3f — %s",
		res.N, time.Since(start), res.Coef.Constant, res.Coef.Density, res.Coef.Anchor, res.Coef.Pop,
		res.MAE, res.MAEBaseline, res.RankRho, res.SignificanceBar(), signal)
	if prev != nil {
		// Side-by-side so a reader can see whether successive fits are
		// converging or thrashing. At small n thrashing is the thing to watch
		// for, and it is invisible if only the new numbers are printed.
		log.Printf("   previous: C=%.3f density^%.3f anchor^%.3f pop^%.3f",
			prev.Constant, prev.Density, prev.Anchor, prev.Pop)
	}

	// THE GUARDRAIL. A fit that cannot beat "predict the median" has learned
	// nothing, and promoting it would replace a working model with noise. The
	// row is still recorded — a refit that failed is evidence about the data,
	// and discarding it would hide a model that is degrading over time.
	promote := res.BeatsBaseline()
	note := fmt.Sprintf("%s | MAE %.3f vs baseline %.3f (rho %+.3f)", estimator, res.MAE, res.MAEBaseline, res.RankRho)
	if !promote {
		note = "NOT PROMOTED — did not beat median baseline. " + note
		log.Printf("🚫 [PlacementRefit] not promoting: %s", note)
	}

	if err := storeFit(db, res, promote, note); err != nil {
		log.Printf("⚠️  [PlacementRefit] could not store fit: %v", err)
		return
	}
	if promote {
		invalidateCoefficientCache()
		log.Printf("✅ [PlacementRefit] promoted new coefficients (%s)", note)
	}
}

// gatherObservations builds the training set: every active, geocoded bin that
// has a measurable current-pitch fill rate, with the SAME site features the
// scorer computes for a candidate.
//
// Features are recomputed live rather than read from a snapshot, because the
// model must describe the world as the scorer currently sees it. If the POI
// catalogue or the whitelist changes, the fit should move with it.
func gatherObservations(db *orgdb.DB) ([]placementfit.Observation, error) {
	var bins []existingBin
	if err := db.Select(&bins, `SELECT id, bin_number, latitude, longitude, city, zip, fill_percentage
		FROM bins WHERE status = 'active' AND latitude IS NOT NULL AND longitude IS NOT NULL`); err != nil {
		return nil, fmt.Errorf("load bins: %w", err)
	}

	h := &ChatHandler{db: db}
	fillRates := h.currentPitchFillRates()
	if len(fillRates) == 0 {
		return nil, fmt.Errorf("no bin has a measurable current-pitch fill rate")
	}

	type censusRow struct {
		Zip        string `db:"zip"`
		Population int    `db:"population"`
	}
	var census []censusRow
	db.Select(&census, `SELECT zip, population FROM census_income_cache WHERE population > 0`)
	zipPop := map[string]int{}
	for _, c := range census {
		zipPop[c.Zip] = c.Population
	}

	country := organizationCountry(db)

	// Only bins with a label are worth the POI call.
	var labeled []existingBin
	for _, b := range bins {
		if r, ok := fillRates[b.ID]; ok && r > 0 {
			labeled = append(labeled, b)
		}
	}

	out := make([]placementfit.Observation, len(labeled))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6) // same politeness as the recommender's POI pass
	for i, b := range labeled {
		wg.Add(1)
		go func(i int, b existingBin) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			d := scorePOIDensity(b.Latitude, b.Longitude, country)
			out[i] = placementfit.Observation{
				BinID:   b.ID,
				Density: densityScoreFor(d.RetailCount),
				Anchor:  anchorScoreFor(d),
				Pop:     popValueFor(zipPop[stripZipPlus4(b.Zip)]),
				Fill:    fillRates[b.ID],
			}
		}(i, b)
	}
	wg.Wait()

	log.Printf("🧠 [PlacementRefit] gathered %d labeled bins (of %d active)", len(out), len(bins))
	return out, nil
}

// gatherCensoredObservations expands the per-bin training set into interval-level
// observations tagged with censoring.
//
// Features come from the already-gathered per-bin rows rather than being
// recomputed, so the two fits see identical site measurements and the ONLY
// difference between them is how 100%-full readings are treated. Recomputing
// here would also mean a second round of ~80 HERE calls for no benefit.
func gatherCensoredObservations(db *orgdb.DB, binObs []placementfit.Observation) []placementfit.CensoredObservation {
	feat := make(map[string]placementfit.Observation, len(binObs))
	for _, o := range binObs {
		feat[o.BinID] = o
	}

	h := &ChatHandler{db: db}
	var out []placementfit.CensoredObservation
	for _, iv := range h.currentPitchIntervals() {
		f, ok := feat[iv.BinID]
		if !ok {
			continue // bin has no site features (inactive, ungeocoded)
		}
		out = append(out, placementfit.CensoredObservation{
			BinID:    iv.BinID,
			Density:  f.Density,
			Anchor:   f.Anchor,
			Pop:      f.Pop,
			Rate:     iv.Rate,
			Censored: iv.Censored,
		})
	}
	return out
}

// The three transforms below are shared with the scoring loop by construction:
// if they ever diverge, the model would be fitted on features that are not the
// ones a candidate is scored with, and the coefficients would be meaningless.
// They are functions rather than inline arithmetic for exactly that reason.

func densityScoreFor(retailCount int) float64 {
	return densityScoreFromCount(retailCount)
}

// anchorScoreFor approximates the scoring loop's tiering from a density scan.
//
// The scorer tiers on the CANDIDATE BUSINESS NAME first and falls back to the
// scan's anchor detection. A bin has no candidate business name, so only the
// fallback is available: a detected anchor scores 0.7, otherwise the non-anchor
// floor.
//
// This looks like it should bias the fitted anchor exponent downward. IT DOES
// NOT — measured on the same 78 bins, full tiering gives +0.1094 and this
// collapsed form gives +0.1137, and the collapsed fit is marginally more
// accurate (LOO MAE 2.139 vs 2.142). The reason is that the tiers do not
// separate on outcome: median fill is 8.12 %/day for tier 1 (n=15), 8.06 for
// tier 2 (n=21), and 6.05 for no anchor (n=42). Anchor-vs-none is the real
// split; tier 1 vs tier 2 carries nothing.
//
// So do not "fix" this by threading full tiering through — it buys 0.004 on one
// coefficient. The open question runs the other way: the 1.0/0.7 split in the
// CANDIDATE scorer may itself be unearned. Recheck at ~150 bins before acting;
// see PLACEMENT_ALGORITHM.md.
func anchorScoreFor(d densityReading) float64 {
	if d.HasAnchor {
		return 0.7
	}
	return 0.15
}

func popValueFor(pop int) float64 {
	if pop <= 0 {
		return 1.0 // unknown ZIP is neutral, never a penalty
	}
	v := float64(pop) / 50000.0
	if v > 1.0 {
		v = 1.0
	}
	if v < 0.3 {
		v = 0.3
	}
	return v
}

// storeFit records the fit. Promotion is a single transaction so there can never
// be two active rows or zero.
func storeFit(db *orgdb.DB, res placementfit.Result, promote bool, note string) error {
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	if promote {
		if _, err := tx.Exec(`UPDATE placement_model SET is_active = FALSE WHERE is_active`); err != nil {
			return fmt.Errorf("deactivate previous: %w", err)
		}
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`INSERT INTO placement_model
		(id, organization_id, fitted_at, coef_constant, coef_density, coef_anchor, coef_pop,
		 n_bins, mae, mae_baseline, rank_rho, is_active, note, created_at)
		VALUES ($1, current_setting('app.org_id'), $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $2)`,
		uuid.NewString(), now,
		res.Coef.Constant, res.Coef.Density, res.Coef.Anchor, res.Coef.Pop,
		res.N, res.MAE, res.MAEBaseline, res.RankRho, promote, note); err != nil {
		return fmt.Errorf("insert fit: %w", err)
	}
	return tx.Commit()
}
