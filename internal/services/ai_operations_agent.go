package services

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"ropacal-backend/internal/services/centrifugo"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type AIOperationsAgent struct {
	db               *sqlx.DB
	client           anthropic.Client
	fcmService       *FCMService
	centrifugoClient *centrifugo.Client
	ticker           *time.Ticker
	stopChan         chan bool
}

func NewAIOperationsAgent(db *sqlx.DB, fcmService *FCMService, centrifugoClient *centrifugo.Client) *AIOperationsAgent {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	client := anthropic.NewClient(option.WithAPIKey(apiKey))

	return &AIOperationsAgent{
		db:               db,
		client:           client,
		fcmService:       fcmService,
		centrifugoClient: centrifugoClient,
		ticker:           time.NewTicker(30 * time.Minute),
		stopChan:         make(chan bool),
	}
}

func (a *AIOperationsAgent) Start(ctx context.Context, wg *sync.WaitGroup) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		log.Println("⚠️ [AIAgent] ANTHROPIC_API_KEY not set — agent disabled")
		return
	}

	log.Println("🤖 [AIAgent] Starting AI Operations Agent (30-minute cycle)")

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Run first cycle after a 2-minute delay (let other services init), but
		// bail immediately if we're already shutting down.
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
		a.runCycle()

		for {
			select {
			case <-ctx.Done():
				log.Println("🤖 [AIAgent] Stopping...")
				return
			case <-a.ticker.C:
				a.runCycle()
			}
		}
	}()
}

func (a *AIOperationsAgent) Stop() {
	a.ticker.Stop()
	a.stopChan <- true
}

func (a *AIOperationsAgent) runCycle() {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	hour := now.Hour()

	// Only run during business hours (6 AM - 8 PM Pacific)
	if hour < 6 || hour > 20 {
		log.Printf("🤖 [AIAgent] Outside business hours (%d:00 PT) — skipping", hour)
		return
	}

	log.Printf("🤖 [AIAgent] ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("🤖 [AIAgent] Running cycle at %s", now.Format("3:04 PM"))

	// Check for critical bins
	a.checkCriticalBins()

	// Check for stale bins (not checked in 30+ days)
	a.checkStaleBins()

	// Chronic "No bin" no-shows: active bins drivers keep failing to find
	a.checkMissingCandidates()

	// Bins drivers repeatedly can't reach (silent service failures)
	a.checkChronicallyInaccessible()

	// Check route performance
	a.checkRoutePerformance()

	// Quadrant rule: weak site vs over-visited
	a.checkUnderperformingBins()

	// Overflow rule: collections arriving (near-)full
	a.checkOverflowBins()

	// Stranded inventory: warehouse dwellers + half-finished moves
	a.checkStrandedInventory()

	// Watchlist probation: matured watches graduate or escalate
	a.evaluateWatchlist()

	log.Printf("🤖 [AIAgent] Cycle complete")
	log.Printf("🤖 [AIAgent] ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}

// checkCriticalBins finds bins estimated to be at 90%+ fill with no active shift covering them
func (a *AIOperationsAgent) checkCriticalBins() {
	type criticalBin struct {
		ID               string `db:"id"`
		BinNumber        int    `db:"bin_number"`
		CurrentStreet    string `db:"current_street"`
		City             string `db:"city"`
		FillPercentage   int    `db:"fill_percentage"`
		LastCheckedAt    *int64 `db:"last_checked_at"`
		AvgDailyFillRate float64
	}

	// Get bins with fill >= 80%
	var bins []criticalBin
	err := a.db.Select(&bins, `
		SELECT id, bin_number, current_street, city, fill_percentage, last_checked_at
		FROM bins WHERE status = 'active' AND fill_percentage >= 80
	`)
	if err != nil || len(bins) == 0 {
		return
	}

	// Check which bins are already on active shifts
	var coveredBinIDs []string
	a.db.Select(&coveredBinIDs, `
		SELECT DISTINCT rt.bin_id FROM route_tasks rt
		JOIN shifts s ON rt.shift_id = s.id
		WHERE s.status IN ('active', 'ready') AND rt.is_completed = 0 AND rt.is_deleted = false AND rt.bin_id IS NOT NULL
	`)
	coveredSet := map[string]bool{}
	for _, id := range coveredBinIDs {
		coveredSet[id] = true
	}

	// Filter to uncovered critical bins
	var uncovered []criticalBin
	for _, b := range bins {
		if !coveredSet[b.ID] {
			uncovered = append(uncovered, b)
		}
	}

	if len(uncovered) == 0 {
		return
	}

	log.Printf("🤖 [AIAgent] Found %d critical bins not on any active shift", len(uncovered))

	// Check for existing pending recommendations to avoid duplicates
	for _, bin := range uncovered {
		var existing int
		a.db.Get(&existing, `SELECT COUNT(*) FROM ai_recommendations WHERE entity_id = $1 AND type = 'bin_overflow' AND status = 'pending'`, bin.ID)
		if existing > 0 {
			continue
		}

		// Calculate days since check
		daysSinceCheck := 0.0
		if bin.LastCheckedAt != nil {
			daysSinceCheck = float64(time.Now().Unix()-*bin.LastCheckedAt) / 86400
		}

		title := fmt.Sprintf("Bin #%d at %d%% fill — needs collection", bin.BinNumber, bin.FillPercentage)
		description := fmt.Sprintf("Bin #%d at %s, %s is at %d%% fill", bin.BinNumber, bin.CurrentStreet, bin.City, bin.FillPercentage)
		if daysSinceCheck > 1 {
			description += fmt.Sprintf(" and hasn't been checked in %.0f days", daysSinceCheck)
		}
		description += ". Not currently on any active shift."

		severity := "high"
		if bin.FillPercentage >= 90 {
			severity = "critical"
		}

		a.createRecommendation("bin_overflow", "bin", bin.ID, title, description, severity,
			"Add to nearest active driver's route or create emergency collection shift",
			fmt.Sprintf("Fill level at %d%% exceeds threshold. %.0f days since last check.", bin.FillPercentage, daysSinceCheck))
	}
}

// checkStaleBins finds bins not checked in 30+ days with 0% fill
func (a *AIOperationsAgent) checkStaleBins() {
	type staleBin struct {
		ID             string  `db:"id"`
		BinNumber      int     `db:"bin_number"`
		CurrentStreet  string  `db:"current_street"`
		City           string  `db:"city"`
		DaysSinceCheck float64 `db:"days_since_check"`
	}

	var bins []staleBin
	a.db.Select(&bins, `
		SELECT id, bin_number, current_street, city,
			COALESCE((EXTRACT(EPOCH FROM NOW())::BIGINT - last_checked_at)::float / 86400, 999) as days_since_check
		FROM bins
		WHERE status = 'active' AND fill_percentage <= 5
			AND (last_checked_at IS NULL OR (EXTRACT(EPOCH FROM NOW())::BIGINT - last_checked_at) > 2592000)
	`)

	for _, bin := range bins {
		var existing int
		a.db.Get(&existing, `SELECT COUNT(*) FROM ai_recommendations WHERE entity_id = $1 AND type = 'bin_retire' AND status = 'pending'`, bin.ID)
		if existing > 0 {
			continue
		}

		title := fmt.Sprintf("Bin #%d at 0%% for %.0f days — consider retiring", bin.BinNumber, bin.DaysSinceCheck)
		description := fmt.Sprintf("Bin #%d at %s, %s has been at 0%% fill for %.0f days. This location may no longer generate donations.", bin.BinNumber, bin.CurrentStreet, bin.City, bin.DaysSinceCheck)

		a.createRecommendation("bin_retire", "bin", bin.ID, title, description, "low",
			"Retire this bin and redeploy to a higher-performing location",
			fmt.Sprintf("Bin has been at 0%% for %.0f days, suggesting the location has very low donation activity.", bin.DaysSinceCheck))
	}
}

// checkMissingCandidates flags ACTIVE bins that drivers have repeatedly skipped
// with a "No bin" reason on recent shifts — a strong signal the bin is actually
// gone. This is the first consumer of route_tasks.task_data->>'skip_reason'.
// Suggests marking the bin missing (which also flags a no-go zone at the spot and
// removes it from future routing), pending manager confirmation.
func (a *AIOperationsAgent) checkMissingCandidates() {
	const windowDays = 45
	const threshold = 3 // distinct shifts where the bin was a "No bin" no-show

	type cand struct {
		ID            string `db:"id"`
		BinNumber     int    `db:"bin_number"`
		CurrentStreet string `db:"current_street"`
		City          string `db:"city"`
		NoBinShifts   int    `db:"no_bin_shifts"`
	}

	var bins []cand
	err := a.db.Select(&bins, `
		SELECT b.id, b.bin_number, b.current_street, b.city,
		       COUNT(DISTINCT rt.shift_id) AS no_bin_shifts
		FROM route_tasks rt
		JOIN shifts s ON s.id = rt.shift_id
		JOIN bins b   ON b.id = rt.bin_id
		WHERE rt.skipped = true
		  AND rt.is_deleted = false
		  AND b.status = 'active'
		  AND rt.task_data->>'skip_reason' ILIKE '%no bin%'
		  AND s.created_at > (EXTRACT(EPOCH FROM NOW())::BIGINT - $1)
		  -- Only count no-shows AFTER the bin's most recent relocation, so a bin
		  -- that was moved/redeployed doesn't drag stale "No bin" reports from its
		  -- OLD location (which would false-flag a freshly-placed bin as missing).
		  AND s.created_at > GREATEST(
		        COALESCE((SELECT MAX(cl.created_at) FROM bin_change_log cl
		                  WHERE cl.bin_id = b.id
		                    AND cl.change_type IN ('address_change','coordinates_change')), 0),
		        COALESCE((SELECT MAX(mr.updated_at) FROM bin_move_requests mr
		                  WHERE mr.bin_id = b.id AND mr.status = 'completed'
		                    AND mr.move_type IN ('relocation','redeployment')), 0),
		        -- The legacy driver-app relocation path (POST /bins/{id}/moves) logs the
		        -- move ONLY in the moves table (no bin_change_log / bin_move_requests row),
		        -- so read the physical-move log too: any row means the bin changed location.
		        COALESCE((SELECT MAX(m.moved_on) FROM moves m WHERE m.bin_id = b.id), 0)
		      )
		GROUP BY b.id, b.bin_number, b.current_street, b.city
		HAVING COUNT(DISTINCT rt.shift_id) >= $2
	`, windowDays*86400, threshold)
	if err != nil {
		log.Printf("❌ [AIAgent] checkMissingCandidates query failed: %v", err)
		return
	}
	if len(bins) > 0 {
		log.Printf("🤖 [AIAgent] Found %d chronic 'No bin' no-show bin(s)", len(bins))
	}

	for _, bin := range bins {
		title := fmt.Sprintf("Bin #%d reported \"No bin\" on %d shifts — likely missing", bin.BinNumber, bin.NoBinShifts)
		description := fmt.Sprintf("Bin #%d at %s, %s was a \"No bin\" no-show on %d separate shifts in the last %d days but is still marked active — so it keeps getting routed. It's probably gone or moved.",
			bin.BinNumber, bin.CurrentStreet, bin.City, bin.NoBinShifts, windowDays)
		reasoning := fmt.Sprintf("Drivers could not find the bin on %d distinct shifts in %d days while it remained active. Repeated \"No bin\" reports strongly indicate the bin is no longer at this location.", bin.NoBinShifts, windowDays)

		// If a pending rec already exists, REFRESH its count/text and extend its life
		// rather than skip. Unlike other rules, the no-show count IS the primary urgency
		// signal here (3 vs 10 no-shows changes the decision), so it must not freeze at
		// its first value while the bin keeps getting missed.
		var existingID string
		if err := a.db.Get(&existingID, `SELECT id FROM ai_recommendations WHERE entity_id = $1 AND type = 'bin_missing_candidate' AND status = 'pending' LIMIT 1`, bin.ID); err == nil && existingID != "" {
			if _, uerr := a.db.Exec(`UPDATE ai_recommendations SET title = $1, description = $2, reasoning = $3, expires_at = $4 WHERE id = $5`,
				title, description, reasoning, time.Now().Unix()+7*86400, existingID); uerr != nil {
				log.Printf("❌ [AIAgent] failed to refresh missing-candidate rec %s: %v", existingID, uerr)
			}
			continue
		}

		a.createRecommendation("bin_missing_candidate", "bin", bin.ID, title, description, "medium",
			"Mark this bin missing — removes it from routing and flags the location as a no-go zone",
			reasoning)
	}
}

// checkChronicallyInaccessible flags ACTIVE bins that drivers repeatedly can't reach.
// An "inaccessible" incident completes the task WITHOUT a fill reading or photo (the
// bin couldn't be opened), so the stop silently shows as "collected/0%" while the bin
// keeps filling — a failure that never surfaces on the shift view. ≥3 inaccessible
// reports in the window means the location itself is the problem (gated, blocked,
// no-entry), not a one-off. Same relocation guard as checkMissingCandidates: only count
// incidents AFTER the bin's last move, so a relocated bin doesn't drag stale reports
// from its old spot. Mirrors [[bin-ops-gotchas]] chronic-signal pattern.
func (a *AIOperationsAgent) checkChronicallyInaccessible() {
	const windowDays = 45
	const threshold = 3 // distinct "inaccessible" incidents

	type cand struct {
		ID              string `db:"id"`
		BinNumber       int    `db:"bin_number"`
		CurrentStreet   string `db:"current_street"`
		City            string `db:"city"`
		AccessIncidents int    `db:"access_incidents"`
	}

	var bins []cand
	err := a.db.Select(&bins, `
		SELECT b.id, b.bin_number, b.current_street, b.city,
		       COUNT(DISTINCT zi.shift_id) AS access_incidents
		FROM zone_incidents zi
		JOIN bins b ON b.id = zi.bin_id
		WHERE zi.incident_type = 'inaccessible'
		  -- Only DRIVER shift reports: a real service attempt that failed. Manager desk
		  -- reports + field observations have shift_id=NULL and aren't "couldn't service
		  -- it on a run", so they must not trip this rule (which claims "on N shifts").
		  AND zi.shift_id IS NOT NULL
		  AND b.status = 'active'
		  AND zi.reported_at > (EXTRACT(EPOCH FROM NOW())::BIGINT - $1)
		  -- Only count access failures AFTER the bin's most recent relocation, so a
		  -- moved bin doesn't carry stale "inaccessible" reports from its old location.
		  AND zi.reported_at > GREATEST(
		        COALESCE((SELECT MAX(cl.created_at) FROM bin_change_log cl
		                  WHERE cl.bin_id = b.id
		                    AND cl.change_type IN ('address_change','coordinates_change')), 0),
		        COALESCE((SELECT MAX(mr.updated_at) FROM bin_move_requests mr
		                  WHERE mr.bin_id = b.id AND mr.status = 'completed'
		                    AND mr.move_type IN ('relocation','redeployment')), 0),
		        COALESCE((SELECT MAX(m.moved_on) FROM moves m WHERE m.bin_id = b.id), 0)
		      )
		GROUP BY b.id, b.bin_number, b.current_street, b.city
		HAVING COUNT(DISTINCT zi.shift_id) >= $2
	`, windowDays*86400, threshold)
	if err != nil {
		log.Printf("❌ [AIAgent] checkChronicallyInaccessible query failed: %v", err)
		return
	}
	if len(bins) > 0 {
		log.Printf("🤖 [AIAgent] Found %d chronically-inaccessible bin(s)", len(bins))
	}

	for _, bin := range bins {
		title := fmt.Sprintf("Bin #%d inaccessible on %d shifts — can't be serviced", bin.BinNumber, bin.AccessIncidents)
		description := fmt.Sprintf("Bin #%d at %s, %s was reported inaccessible on %d separate shifts in the last %d days but is still active and routed. Each visit fails silently (no collection, shows as 0%%), so it's likely overflowing. Relocate it or resolve access with the property.",
			bin.BinNumber, bin.CurrentStreet, bin.City, bin.AccessIncidents, windowDays)
		reasoning := fmt.Sprintf("Drivers reported \"inaccessible\" on %d distinct shifts in %d days while the bin stayed active. Repeated access failures mean the bin cannot be serviced at this location.", bin.AccessIncidents, windowDays)

		// Refresh an existing pending rec (the incident count is the load-bearing
		// urgency signal, so it must track upward, not freeze at its first value).
		var existingID string
		if err := a.db.Get(&existingID, `SELECT id FROM ai_recommendations WHERE entity_id = $1 AND type = 'bin_access_issue' AND status = 'pending' LIMIT 1`, bin.ID); err == nil && existingID != "" {
			if _, uerr := a.db.Exec(`UPDATE ai_recommendations SET title = $1, description = $2, reasoning = $3, expires_at = $4 WHERE id = $5`,
				title, description, reasoning, time.Now().Unix()+7*86400, existingID); uerr != nil {
				log.Printf("❌ [AIAgent] failed to refresh access-issue rec %s: %v", existingID, uerr)
			}
			continue
		}

		a.createRecommendation("bin_access_issue", "bin", bin.ID, title, description, "high",
			"Relocate this bin or resolve access with the property — drivers repeatedly can't reach it",
			reasoning)
	}
}

// checkRoutePerformance checks shift history for underperforming routes
func (a *AIOperationsAgent) checkRoutePerformance() {
	type routePerf struct {
		RouteID       *string `db:"route_id"`
		AvgCompletion float64 `db:"avg_completion"`
		ShiftCount    int     `db:"shift_count"`
	}

	var routes []routePerf
	a.db.Select(&routes, `
		SELECT route_id, AVG(completion_rate) as avg_completion, COUNT(*) as shift_count
		FROM shift_history
		WHERE route_id IS NOT NULL AND ended_at > EXTRACT(EPOCH FROM NOW())::BIGINT - 2592000
		GROUP BY route_id
		HAVING COUNT(*) >= 3 AND AVG(completion_rate) < 60
	`)

	for _, route := range routes {
		if route.RouteID == nil {
			continue
		}

		var existing int
		a.db.Get(&existing, `SELECT COUNT(*) FROM ai_recommendations WHERE entity_id = $1 AND type = 'route_split' AND status = 'pending'`, *route.RouteID)
		if existing > 0 {
			continue
		}

		// Get route name
		var routeName string
		a.db.Get(&routeName, `SELECT COALESCE(name, 'Unnamed Route') FROM routes WHERE id = $1`, *route.RouteID)

		title := fmt.Sprintf("Route '%s' averaging %.0f%% completion — consider splitting", routeName, route.AvgCompletion)
		description := fmt.Sprintf("Route '%s' has averaged %.0f%% completion rate over the last %d shifts. Drivers may not be finishing the full route.",
			routeName, route.AvgCompletion, route.ShiftCount)

		a.createRecommendation("route_split", "route", *route.RouteID, title, description, "medium",
			"Split this route into two shorter routes or remove low-priority bins",
			fmt.Sprintf("%.0f%% avg completion over %d shifts suggests the route is too long or has problematic bins.", route.AvgCompletion, route.ShiftCount))
	}
}

// checkUnderperformingBins applies the demand-vs-cadence QUADRANT rule.
// Low fill AT COLLECTION alone is ambiguous — it splits on the site's own
// demand (median fill %/day between checks):
//
//	weak site    (fills slowly too)      -> relocation candidate
//	over-visited (fills fine, we're early) -> reduce cadence, NOT a move
//
// Relocation recs are suppressed when the bin already has an open move
// (one-open-move invariant) and for bins with <5 collections (small sample).
func (a *AIOperationsAgent) checkUnderperformingBins() {
	type row struct {
		ID            string   `db:"id"`
		BinNumber     int      `db:"bin_number"`
		CurrentStreet string   `db:"current_street"`
		City          string   `db:"city"`
		MedianFill    float64  `db:"median_fill"`
		Collections   int      `db:"collections"`
		FillRate      *float64 `db:"fill_rate"` // median %/day between checks; NULL = not enough pairs
		OpenMoves     int      `db:"open_moves"`
	}

	var bins []row
	err := a.db.Select(&bins, `
		WITH recent AS (
			-- last 6 collections per bin (checks recorded on a shift)
			SELECT bin_id, fill_percentage,
			       ROW_NUMBER() OVER (PARTITION BY bin_id ORDER BY checked_on DESC) AS rn
			FROM checks
			WHERE shift_id IS NOT NULL AND fill_percentage IS NOT NULL
		),
		coll AS (
			SELECT bin_id,
			       PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY fill_percentage) AS median_fill,
			       COUNT(*) AS collections
			FROM recent WHERE rn <= 6 GROUP BY bin_id
		),
		pairs AS (
			SELECT bin_id,
			       fill_percentage - LAG(fill_percentage) OVER (PARTITION BY bin_id ORDER BY checked_on) AS dfill,
			       (checked_on - LAG(checked_on) OVER (PARTITION BY bin_id ORDER BY checked_on)) / 86400.0 AS ddays
			FROM checks WHERE fill_percentage IS NOT NULL
		),
		rate AS (
			SELECT bin_id,
			       PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY dfill / ddays) AS fill_rate
			FROM pairs WHERE dfill > 0 AND ddays > 0.25 GROUP BY bin_id
		)
		SELECT b.id, b.bin_number, b.current_street, b.city,
		       coll.median_fill, coll.collections,
		       rate.fill_rate,
		       (SELECT COUNT(*) FROM bin_move_requests mr
		        WHERE mr.bin_id = b.id AND mr.status NOT IN ('completed','cancelled')) AS open_moves
		FROM bins b
		JOIN coll ON coll.bin_id = b.id
		LEFT JOIN rate ON rate.bin_id = b.id
		WHERE b.status = 'active' AND coll.collections >= 5 AND coll.median_fill < 30`)
	if err != nil {
		log.Printf("❌ [AIAgent] Failed to query quadrant bins: %v", err)
		return
	}

	log.Printf("🤖 [AIAgent] Quadrant rule: %d bins collected below 30%% median (last 6)", len(bins))

	for _, bin := range bins {
		rateVal := 0.0
		if bin.FillRate != nil {
			rateVal = *bin.FillRate
		}
		if rateVal >= 1.5 {
			// Over-visited: the site fills fine — we come too early.
			var existing int
			a.db.Get(&existing, `SELECT COUNT(*) FROM ai_recommendations WHERE entity_id = $1 AND type = 'bin_reduce_cadence' AND status = 'pending'`, bin.ID)
			if existing > 0 {
				continue
			}
			a.startWatch(bin.ID, fmt.Sprintf("Cadence review: median %.0f%% at collection, fills %.1f%%/day", bin.MedianFill, rateVal), rateVal, 14)
			a.createRecommendation(
				"bin_reduce_cadence", "bin", bin.ID,
				fmt.Sprintf("Bin #%d is over-visited — collect it less often", bin.BinNumber),
				fmt.Sprintf("Bin #%d at %s, %s fills %.1f%%/day but its last %d collections happened at a median of just %.0f%% — each visit costs a stop for a fraction of a load.",
					bin.BinNumber, bin.CurrentStreet, bin.City, rateVal, bin.Collections, bin.MedianFill),
				"medium",
				"Reduce this bin's visit cadence (move it to a less frequent route template)",
				fmt.Sprintf("Quadrant: fill rate %.1f%%/day (healthy) with median %.0f%% at collection (low) = over-visited, not a weak site.", rateVal, bin.MedianFill),
			)
		} else {
			// Weak site: fills slowly AND collected low — relocation candidate.
			if bin.OpenMoves > 0 {
				continue // one open move per bin — don't recommend a second
			}
			var existing int
			a.db.Get(&existing, `SELECT COUNT(*) FROM ai_recommendations WHERE entity_id = $1 AND type = 'bin_relocate' AND status = 'pending'`, bin.ID)
			if existing > 0 {
				continue
			}
			a.createRecommendation(
				"bin_relocate", "bin", bin.ID,
				fmt.Sprintf("Bin #%d is a weak site — consider relocating", bin.BinNumber),
				fmt.Sprintf("Bin #%d at %s, %s fills only %.1f%%/day and its last %d collections had a median of %.0f%%. The location itself has low donation activity.",
					bin.BinNumber, bin.CurrentStreet, bin.City, rateVal, bin.Collections, bin.MedianFill),
				"low",
				"Relocate to a higher-traffic location (schedule a move request)",
				fmt.Sprintf("Quadrant: fill rate %.1f%%/day (weak) AND median %.0f%% at collection (low) = the site, not the cadence.", rateVal, bin.MedianFill),
			)
		}
	}
}

// checkOverflowBins finds bins whose recent collections arrive (near-)full —
// donations are being turned away between visits.
func (a *AIOperationsAgent) checkOverflowBins() {
	type row struct {
		ID            string `db:"id"`
		BinNumber     int    `db:"bin_number"`
		CurrentStreet string `db:"current_street"`
		City          string `db:"city"`
		FullCount     int    `db:"full_count"`
	}
	var bins []row
	err := a.db.Select(&bins, `
		WITH recent AS (
			SELECT bin_id, fill_percentage,
			       ROW_NUMBER() OVER (PARTITION BY bin_id ORDER BY checked_on DESC) AS rn
			FROM checks WHERE shift_id IS NOT NULL AND fill_percentage IS NOT NULL
		)
		SELECT b.id, b.bin_number, b.current_street, b.city,
		       COUNT(*) FILTER (WHERE recent.fill_percentage >= 90) AS full_count
		FROM bins b JOIN recent ON recent.bin_id = b.id AND recent.rn <= 3
		WHERE b.status = 'active'
		GROUP BY b.id, b.bin_number, b.current_street, b.city
		HAVING COUNT(*) FILTER (WHERE recent.fill_percentage >= 90) >= 2`)
	if err != nil {
		log.Printf("❌ [AIAgent] Failed to query overflow bins: %v", err)
		return
	}
	log.Printf("🤖 [AIAgent] Overflow rule: %d bins at >=90%% on 2 of last 3 collections", len(bins))

	for _, bin := range bins {
		var existing int
		a.db.Get(&existing, `SELECT COUNT(*) FROM ai_recommendations WHERE entity_id = $1 AND type = 'bin_increase_cadence' AND status = 'pending'`, bin.ID)
		if existing > 0 {
			continue
		}
		a.createRecommendation(
			"bin_increase_cadence", "bin", bin.ID,
			fmt.Sprintf("Bin #%d keeps overflowing — collect it more often", bin.BinNumber),
			fmt.Sprintf("Bin #%d at %s, %s was at 90%%+ on %d of its last 3 collections. A full bin silently turns donors away between visits.",
				bin.BinNumber, bin.CurrentStreet, bin.City, bin.FullCount),
			"high",
			"Increase this bin's visit cadence, or place a sibling bin nearby if already at max",
			fmt.Sprintf("%d of last 3 collections at >=90%% fill.", bin.FullCount),
		)
	}
}

// checkStrandedInventory flags idle capital: bins parked in the warehouse for
// 21+ days, and two-leg moves whose pickup completed but dropoff never ran.
func (a *AIOperationsAgent) checkStrandedInventory() {
	// Warehouse dwellers (updated_at approximates the store date — it is
	// touched when the store move completes and rarely after).
	type whRow struct {
		ID        string `db:"id"`
		BinNumber int    `db:"bin_number"`
		Days      int    `db:"days"`
	}
	var stored []whRow
	if err := a.db.Select(&stored, `
		SELECT id, bin_number,
		       ((EXTRACT(EPOCH FROM NOW())::BIGINT - updated_at) / 86400)::int AS days
		FROM bins
		WHERE status = 'in_storage'
		  AND (EXTRACT(EPOCH FROM NOW())::BIGINT - updated_at) > 21 * 86400
		  AND NOT EXISTS (SELECT 1 FROM bin_move_requests mr WHERE mr.bin_id = bins.id AND mr.status NOT IN ('completed','cancelled'))`); err != nil {
		log.Printf("❌ [AIAgent] Failed to query warehouse dwellers: %v", err)
	} else {
		log.Printf("🤖 [AIAgent] Stranded inventory: %d bins in warehouse 21+ days", len(stored))
		for _, b := range stored {
			var existing int
			a.db.Get(&existing, `SELECT COUNT(*) FROM ai_recommendations WHERE entity_id = $1 AND type = 'bin_redeploy' AND status = 'pending'`, b.ID)
			if existing > 0 {
				continue
			}
			a.createRecommendation(
				"bin_redeploy", "bin", b.ID,
				fmt.Sprintf("Bin #%d has sat in the warehouse for %d days", b.BinNumber, b.Days),
				fmt.Sprintf("Bin #%d has been in storage for %d days with no open move. A warehoused bin earns nothing.", b.BinNumber, b.Days),
				"medium",
				"Redeploy via the shift builder (deploy from warehouse) or schedule a redeployment move",
				fmt.Sprintf("in_storage for %d days, no open move request.", b.Days),
			)
		}
	}

	// Stranded two-leg moves: pickup done 7+ days ago, dropoff still live.
	type mvRow struct {
		MoveID    string `db:"move_id"`
		BinNumber int    `db:"bin_number"`
		Days      int    `db:"days"`
	}
	var stranded []mvRow
	if err := a.db.Select(&stranded, `
		SELECT mr.id AS move_id, COALESCE(b.bin_number, 0) AS bin_number,
		       ((EXTRACT(EPOCH FROM NOW())::BIGINT - pk.completed_at) / 86400)::int AS days
		FROM bin_move_requests mr
		JOIN bins b ON b.id = mr.bin_id
		JOIN route_tasks pk ON pk.move_request_id = mr.id AND pk.task_type = 'pickup'
		     AND pk.is_completed = 1 AND pk.is_deleted = false
		JOIN route_tasks dr ON dr.move_request_id = mr.id AND dr.task_type = 'dropoff'
		     AND dr.is_completed = 0 AND dr.is_deleted = false
		WHERE mr.status IN ('assigned','in_progress')
		  AND (EXTRACT(EPOCH FROM NOW())::BIGINT - pk.completed_at) > 7 * 86400`); err != nil {
		log.Printf("❌ [AIAgent] Failed to query stranded moves: %v", err)
	} else {
		log.Printf("🤖 [AIAgent] Stranded moves: %d picked up 7+ days ago, never dropped off", len(stranded))
		for _, m := range stranded {
			var existing int
			a.db.Get(&existing, `SELECT COUNT(*) FROM ai_recommendations WHERE entity_id = $1 AND type = 'move_stranded' AND status = 'pending'`, m.MoveID)
			if existing > 0 {
				continue
			}
			a.createRecommendation(
				"move_stranded", "move_request", m.MoveID,
				fmt.Sprintf("Bin #%d was picked up %d days ago but never dropped off", m.BinNumber, m.Days),
				fmt.Sprintf("The move for bin #%d completed its pickup leg %d days ago; the dropoff has not run. The bin is on a truck or in limbo.", m.BinNumber, m.Days),
				"high",
				"Schedule the dropoff leg (assign the move to an upcoming shift)",
				fmt.Sprintf("Pickup completed %d days ago; dropoff task still live.", m.Days),
			)
		}
	}
}

// startWatch puts a bin on probation: the agent re-measures its demand after
// evaluateDays and either graduates it (improved) or escalates to a
// relocation recommendation. One open watch per bin.
func (a *AIOperationsAgent) startWatch(binID, reason string, baselineRate float64, evaluateDays int) {
	var open int
	a.db.Get(&open, `SELECT COUNT(*) FROM bin_watchlist WHERE bin_id = $1 AND status = 'watching'`, binID)
	if open > 0 {
		return
	}
	now := time.Now().Unix()
	if _, err := a.db.Exec(`
		INSERT INTO bin_watchlist (id, bin_id, reason, baseline_fill_rate, started_at, evaluate_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New().String(), binID, reason, baselineRate, now, now+int64(evaluateDays)*86400); err != nil {
		log.Printf("❌ [AIAgent] Failed to start watch for bin %s: %v", binID, err)
		return
	}
	log.Printf("🤖 [AIAgent] Watch started: bin %s for %d days (baseline %.1f%%/day)", binID, evaluateDays, baselineRate)
}

// evaluateWatchlist processes matured watches: fill rate improved >=20% over
// baseline -> graduated; otherwise escalate to a relocation recommendation
// (suppressed when the bin already has an open move — one-open-move rule).
func (a *AIOperationsAgent) evaluateWatchlist() {
	type watch struct {
		ID        string   `db:"id"`
		BinID     string   `db:"bin_id"`
		BinNumber int      `db:"bin_number"`
		Reason    string   `db:"reason"`
		Baseline  *float64 `db:"baseline_fill_rate"`
		Current   *float64 `db:"current_rate"`
		OpenMoves int      `db:"open_moves"`
	}
	var due []watch
	err := a.db.Select(&due, `
		WITH pairs AS (
			SELECT bin_id,
			       fill_percentage - LAG(fill_percentage) OVER (PARTITION BY bin_id ORDER BY checked_on) AS dfill,
			       (checked_on - LAG(checked_on) OVER (PARTITION BY bin_id ORDER BY checked_on)) / 86400.0 AS ddays
			FROM checks WHERE fill_percentage IS NOT NULL
			  AND checked_on > EXTRACT(EPOCH FROM NOW())::BIGINT - 30*86400
		),
		rate AS (
			SELECT bin_id, PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY dfill / ddays) AS current_rate
			FROM pairs WHERE dfill > 0 AND ddays > 0.25 GROUP BY bin_id
		)
		SELECT w.id, w.bin_id, COALESCE(b.bin_number, 0) AS bin_number,
		       w.reason, w.baseline_fill_rate, rate.current_rate,
		       (SELECT COUNT(*) FROM bin_move_requests mr
		        WHERE mr.bin_id = w.bin_id AND mr.status NOT IN ('completed','cancelled')) AS open_moves
		FROM bin_watchlist w
		JOIN bins b ON b.id = w.bin_id
		LEFT JOIN rate ON rate.bin_id = w.bin_id
		WHERE w.status = 'watching' AND w.evaluate_at <= EXTRACT(EPOCH FROM NOW())::BIGINT`)
	if err != nil {
		log.Printf("❌ [AIAgent] Failed to load matured watches: %v", err)
		return
	}
	if len(due) == 0 {
		return
	}
	log.Printf("🤖 [AIAgent] Evaluating %d matured watch(es)", len(due))
	now := time.Now().Unix()

	for _, w := range due {
		baseline := 0.0
		if w.Baseline != nil {
			baseline = *w.Baseline
		}
		current := 0.0
		if w.Current != nil {
			current = *w.Current
		}
		// Two watch flavors share the table but NOT the semantics:
		//  - cadence review: baseline = the bin's OWN prior rate; graduating
		//    means genuinely improving (+20%).
		//  - new-territory probe (enrollNewTerritoryProbe): baseline = the
		//    NETWORK median; a probe succeeds by MEETING typical demand, not
		//    beating it by 20%.
		isProbe := strings.HasPrefix(w.Reason, "New-territory probe")

		if isProbe && w.Current == nil {
			// No usable fill data means nobody VISITED the probe — a routing
			// gap, not a site verdict. Extend instead of escalating on absence
			// of evidence (the stale-bin flagger surfaces unvisited bins).
			a.db.Exec(`UPDATE bin_watchlist SET evaluate_at = $1 WHERE id = $2`, now+14*86400, w.ID)
			log.Printf("🧭 [AIAgent] Probe bin #%d matured with no fill data — extending watch 14d (bin likely unvisited)", w.BinNumber)
			continue
		}

		var improved bool
		if isProbe {
			improved = baseline > 0 && current >= baseline
		} else {
			improved = baseline > 0 && current >= baseline*1.2
		}

		status := "escalated"
		if improved {
			status = "improved"
		}
		a.db.Exec(`UPDATE bin_watchlist SET status = $1, resolved_at = $2 WHERE id = $3`, status, now, w.ID)

		if improved {
			log.Printf("🤖 [AIAgent] Watch graduated: bin #%d (%.1f -> %.1f %%/day)", w.BinNumber, baseline, current)
			continue
		}
		if w.OpenMoves > 0 {
			continue
		}
		var existing int
		a.db.Get(&existing, `SELECT COUNT(*) FROM ai_recommendations WHERE entity_id = $1 AND type = 'bin_relocate' AND status = 'pending'`, w.BinID)
		if existing > 0 {
			continue
		}
		if isProbe {
			a.createRecommendation(
				"bin_relocate", "bin", w.BinID,
				fmt.Sprintf("New-territory bin #%d isn't filling — consider pulling it back", w.BinNumber),
				fmt.Sprintf("Bin #%d was placed in unproven territory and enrolled as a 14-day probe. It fills %.1f%%/day vs the network's typical %.1f%%/day — the location isn't generating demand.", w.BinNumber, current, baseline),
				"medium",
				"Relocate to a scored candidate location, or return it to the warehouse",
				fmt.Sprintf("New-territory probe matured below the network-median bar: %.1f%%/day vs %.1f%%/day typical.", current, baseline),
			)
		} else {
			a.createRecommendation(
				"bin_relocate", "bin", w.BinID,
				fmt.Sprintf("Bin #%d didn't improve after its probation — relocate it", w.BinNumber),
				fmt.Sprintf("Bin #%d was watched after a cadence review. Its fill rate is %.1f%%/day vs a %.1f%%/day baseline — the site, not the schedule, is the constraint.", w.BinNumber, current, baseline),
				"medium",
				"Relocate to a scored candidate location (use the suggested destinations picker)",
				fmt.Sprintf("Watch matured without improvement: baseline %.1f%%/day, current %.1f%%/day (needs +20%% to graduate).", baseline, current),
			)
		}
	}
}

func (a *AIOperationsAgent) createRecommendation(recType, entityType, entityID, title, description, severity, action, reasoning string) {
	id := uuid.New().String()
	now := time.Now().Unix()
	expiresAt := now + (7 * 86400) // Expire in 7 days

	_, err := a.db.Exec(`
		INSERT INTO ai_recommendations (id, type, entity_type, entity_id, title, description, severity, recommended_action, status, source, reasoning, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', 'ai_agent', $9, $10, $11)
	`, id, recType, entityType, entityID, title, description, severity, action, reasoning, now, expiresAt)

	if err != nil {
		log.Printf("❌ [AIAgent] Failed to create recommendation: %v", err)
		return
	}

	log.Printf("🤖 [AIAgent] Created recommendation: [%s] %s", severity, title)

	// Broadcast to managers via Centrifugo
	if a.centrifugoClient != nil {
		ctx := context.Background()
		a.centrifugoClient.PublishCompanyEvent(ctx, "ai_recommendation_created", map[string]interface{}{
			"id":       id,
			"type":     recType,
			"title":    title,
			"severity": severity,
		})
	}

	// Send push notification for critical/high severity
	if (severity == "critical" || severity == "high") && a.fcmService != nil {
		var tokens []string
		a.db.Select(&tokens, `SELECT token FROM fcm_tokens WHERE user_id IN (SELECT id FROM users WHERE role = 'admin')`)
		if len(tokens) > 0 {
			a.fcmService.SendMulticast(tokens, "AI Recommendation", title, map[string]string{
				"type":              "ai_recommendation",
				"recommendation_id": id,
			})
		}
	}
}
