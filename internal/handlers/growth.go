package handlers

import (
	"log"
	"math"
	"net/http"
	"sort"
	"time"

	"ropacal-backend/internal/geo"
	"ropacal-backend/pkg/utils"

	"github.com/jmoiron/sqlx"
)

// binYieldRow is one active bin's trailing-90-day yield picture. The yield
// proxy is Σ fill-percentage over collections (a stand-in for kg until weight
// capture lands — swap the numerator then and everything downstream upgrades).
type binYieldRow struct {
	ID            string   `db:"id" json:"id"`
	BinNumber     int      `db:"bin_number" json:"bin_number"`
	CurrentStreet string   `db:"current_street" json:"current_street"`
	City          string   `db:"city" json:"city"`
	Latitude      *float64 `db:"latitude" json:"latitude"`
	Longitude     *float64 `db:"longitude" json:"longitude"`
	Collections   int      `db:"collections" json:"collections_90d"`
	YieldProxy    float64  `db:"yield_proxy" json:"yield_proxy_90d"`
	Incidents     int      `db:"incidents" json:"incidents_90d"`
	DaysActive    float64  `db:"days_active" json:"days_active_90d"`
	// computed
	YieldPerWeek float64 `db:"-" json:"yield_per_bin_week"`
}

func loadBinYields(db *sqlx.DB) ([]binYieldRow, error) {
	var rows []binYieldRow
	err := db.Select(&rows, `
		SELECT b.id, b.bin_number, b.current_street, b.city, b.latitude, b.longitude,
		       COALESCE(y.collections, 0) AS collections,
		       COALESCE(y.sum_fill, 0) AS yield_proxy,
		       COALESCE(inc.incidents, 0) AS incidents,
		       LEAST(90.0, GREATEST(1.0,
		           (EXTRACT(EPOCH FROM NOW()) - COALESCE(fc.first_check, b.created_at)) / 86400.0
		       )) AS days_active
		FROM bins b
		LEFT JOIN (
			SELECT bin_id, COUNT(*) AS collections, SUM(fill_percentage) AS sum_fill
			FROM checks
			WHERE shift_id IS NOT NULL AND fill_percentage IS NOT NULL
			  AND checked_on > EXTRACT(EPOCH FROM NOW())::BIGINT - 90*86400
			GROUP BY bin_id
		) y ON y.bin_id = b.id
		LEFT JOIN (
			SELECT bin_id, COUNT(*) AS incidents
			FROM zone_incidents
			WHERE reported_at > EXTRACT(EPOCH FROM NOW())::BIGINT - 90*86400
			GROUP BY bin_id
		) inc ON inc.bin_id = b.id
		LEFT JOIN (
			SELECT bin_id, MIN(checked_on) AS first_check FROM checks GROUP BY bin_id
		) fc ON fc.bin_id = b.id
		WHERE b.status = 'active'
		  AND b.latitude IS NOT NULL AND b.longitude IS NOT NULL
		  AND NOT (b.latitude = 0 AND b.longitude = 0)`)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		weeks := rows[i].DaysActive / 7.0
		if weeks > 0 {
			rows[i].YieldPerWeek = rows[i].YieldProxy / weeks
		}
	}
	return rows, nil
}

// GetGrowthBinYields returns the per-bin yield dataset behind the Growth tab's
// hex map. Hex bucketing happens client-side (h3-js) — the backend stays free
// of the cgo H3 bindings.
// GET /api/analytics/growth/bin-yield
func GetGrowthBinYields(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := loadBinYields(db)
		if err != nil {
			log.Printf("❌ Error loading bin yields: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to load bin yields")
			return
		}
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    rows,
		})
	}
}

// GetGrowthCandidates scores every unconverted potential location for
// deployment, using only data we already hold:
//
//	YieldPrior      — distance-weighted mean yield/bin-week of bins within
//	                  1.5 km (kernel exp(-d/600m)), normalized to network P90
//	Gap             — distance to the nearest bin, capped at 1200 m (whitespace)
//	IncidentRisk    — kernel over ACTIVE no-go zone centers (exp(-d/400m));
//	                  inside a zone's radius disqualifies outright
//	Cannibalization — how much of the prior comes from close (<800 m)
//	                  neighbors that a new bin would split rather than grow
//
// Score = 100 × (0.55·YieldPrior + 0.45·Gap), clamped to [0,100].
//
// IncidentRisk and Cannibalization are still computed and emitted for the
// dashboard, but no longer weigh on the score: the 2026-07 calibration
// (93 live bins vs fill-rate %/day) measured both at ρ≈0 — risk stays a
// HARD rule (inside a no-go zone still zeroes the score), not a gradient.
// RouteFit (OR-Tools insertion cost) and HostQuality (address history) are
// deferred — noted so the omission is explicit.
// GET /api/analytics/growth/candidates
func GetGrowthCandidates(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bins, err := loadBinYields(db)
		if err != nil {
			log.Printf("❌ Error loading bin yields for scoring: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to score candidates")
			return
		}

		type candidate struct {
			ID        string   `db:"id" json:"id"`
			Address   string   `db:"address" json:"address"`
			Street    string   `db:"street" json:"street"`
			City      string   `db:"city" json:"city"`
			Latitude  *float64 `db:"latitude" json:"latitude"`
			Longitude *float64 `db:"longitude" json:"longitude"`
			Notes     *string  `db:"notes" json:"notes,omitempty"`
			// computed
			Score           float64 `db:"-" json:"score"`
			YieldPrior      float64 `db:"-" json:"yield_prior"`
			Gap             float64 `db:"-" json:"gap"`
			IncidentRisk    float64 `db:"-" json:"incident_risk"`
			Cannibalization float64 `db:"-" json:"cannibalization"`
			InNoGoZone      bool    `db:"-" json:"in_no_go_zone"`
			NearestBinM     float64 `db:"-" json:"nearest_bin_m"`
		}
		var cands []candidate
		if err := db.Select(&cands, `
			SELECT id, address, street, city, latitude, longitude, notes
			FROM potential_locations
			WHERE converted_to_bin_id IS NULL
			  AND latitude IS NOT NULL AND longitude IS NOT NULL
			  AND NOT (latitude = 0 AND longitude = 0)`); err != nil {
			log.Printf("❌ Error loading candidates: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to load candidates")
			return
		}

		type zone struct {
			Lat    float64 `db:"center_latitude"`
			Lng    float64 `db:"center_longitude"`
			Radius float64 `db:"radius_meters"`
		}
		var zones []zone
		if err := db.Select(&zones, `
			SELECT center_latitude, center_longitude, radius_meters
			FROM no_go_zones WHERE status = 'active'`); err != nil {
			log.Printf("⚠️  Failed to load no-go zones for scoring: %v", err)
		}

		// Network P90 of yield/bin-week anchors the YieldPrior normalization.
		yields := make([]float64, 0, len(bins))
		for _, b := range bins {
			yields = append(yields, b.YieldPerWeek)
		}
		sort.Float64s(yields)
		p90 := 1.0
		if len(yields) > 0 {
			p90 = yields[int(float64(len(yields))*0.9)]
			if p90 <= 0 {
				p90 = 1.0
			}
		}

		clamp01 := func(v float64) float64 { return math.Max(0, math.Min(1, v)) }

		for i := range cands {
			c := &cands[i]
			if c.Latitude == nil || c.Longitude == nil {
				continue
			}
			lat, lng := *c.Latitude, *c.Longitude

			var priorWeighted, priorWeights, closeWeighted float64
			nearest := math.MaxFloat64
			for _, b := range bins {
				d := geo.HaversineMeters(lat, lng, *b.Latitude, *b.Longitude)
				if d < nearest {
					nearest = d
				}
				if d <= 1500 {
					wgt := math.Exp(-d / 600.0)
					priorWeighted += wgt * b.YieldPerWeek
					priorWeights += wgt
				}
				if d <= 800 {
					closeWeighted += math.Exp(-d/400.0) * b.YieldPerWeek
				}
			}
			priorRaw := 0.0
			if priorWeights > 0 {
				priorRaw = priorWeighted / priorWeights
			}
			c.YieldPrior = clamp01(priorRaw / p90)
			if nearest == math.MaxFloat64 {
				nearest = 1200
			}
			c.NearestBinM = math.Round(nearest)
			c.Gap = clamp01(nearest / 1200.0)
			if priorRaw > 0 {
				c.Cannibalization = clamp01(closeWeighted / (priorRaw * 3.0))
			}

			risk := 0.0
			for _, z := range zones {
				d := geo.HaversineMeters(lat, lng, z.Lat, z.Lng)
				if d <= z.Radius {
					c.InNoGoZone = true
				}
				risk += math.Exp(-d / 400.0)
			}
			c.IncidentRisk = clamp01(risk)

			if c.InNoGoZone {
				c.Score = 0
			} else {
				// Calibrated 2026-07: incident-risk & cannibalization dropped from
				// the gradient (ρ≈0 vs fill-rate on 93 bins); no-go stays a hard zero.
				c.Score = math.Max(0, math.Min(100,
					100*(0.55*c.YieldPrior+0.45*c.Gap)))
			}
			c.Score = math.Round(c.Score)
		}

		sort.Slice(cands, func(i, j int) bool { return cands[i].Score > cands[j].Score })

		var warehouseAvailable int
		_ = db.Get(&warehouseAvailable, `SELECT COUNT(*) FROM bins WHERE status = 'in_storage'`)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"candidates":          cands,
				"warehouse_available": warehouseAvailable,
				"scored_at":           time.Now().Unix(),
			},
		})
	}
}

// GetWeeklyGrowthPlan assembles the week's growth actions into one reviewable
// plan: bins on probation, matured escalations (relocations with the scorer
// as destination picker), warehouse redeploys, and last week's applied
// outcomes — so the plan grades itself over time.
// GET /api/analytics/growth/weekly-plan
func GetWeeklyGrowthPlan(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Unix()

		type watchRow struct {
			BinID      string   `db:"bin_id" json:"bin_id"`
			BinNumber  int      `db:"bin_number" json:"bin_number"`
			Street     string   `db:"current_street" json:"current_street"`
			Reason     string   `db:"reason" json:"reason"`
			Baseline   *float64 `db:"baseline_fill_rate" json:"baseline_fill_rate"`
			EvaluateAt int64    `db:"evaluate_at" json:"evaluate_at"`
		}
		watching := []watchRow{} // non-nil so empty marshals as [], not null
		if err := db.Select(&watching, `
			SELECT w.bin_id, COALESCE(b.bin_number,0) AS bin_number, b.current_street,
			       w.reason, w.baseline_fill_rate, w.evaluate_at
			FROM bin_watchlist w JOIN bins b ON b.id = w.bin_id
			WHERE w.status = 'watching' ORDER BY w.evaluate_at ASC`); err != nil {
			log.Printf("❌ Weekly plan: watchlist query failed: %v", err)
		}

		type recRow struct {
			ID        string `db:"id" json:"id"`
			Type      string `db:"type" json:"type"`
			EntityID  string `db:"entity_id" json:"entity_id"`
			BinNumber int    `db:"bin_number" json:"bin_number"`
			Street    string `db:"current_street" json:"current_street"`
			Title     string `db:"title" json:"title"`
			Severity  string `db:"severity" json:"severity"`
			CreatedAt int64  `db:"created_at" json:"created_at"`
		}
		loadRecs := func(recType string) []recRow {
			rows := []recRow{}
			if err := db.Select(&rows, `
				SELECT r.id, r.type, COALESCE(r.entity_id,'') AS entity_id,
				       COALESCE(b.bin_number,0) AS bin_number, COALESCE(b.current_street,'') AS current_street,
				       r.title, r.severity, r.created_at
				FROM ai_recommendations r
				LEFT JOIN bins b ON b.id = r.entity_id
				WHERE r.type = $1 AND r.status = 'pending'
				  AND (r.expires_at IS NULL OR r.expires_at > $2)
				  AND (r.snoozed_until IS NULL OR r.snoozed_until < $2)
				ORDER BY r.created_at DESC`, recType, now); err != nil {
				log.Printf("❌ Weekly plan: %s recs query failed: %v", recType, err)
			}
			return rows
		}

		// Last week's applied outcomes: actioned recs by type.
		type outcomeRow struct {
			Type  string `db:"type" json:"type"`
			Count int    `db:"count" json:"count"`
		}
		applied := []outcomeRow{}
		if err := db.Select(&applied, `
			SELECT type, COUNT(*) AS count FROM ai_recommendations
			WHERE actioned_at IS NOT NULL AND actioned_at > $1 - 7*86400
			GROUP BY type`, now); err != nil {
			log.Printf("❌ Weekly plan: outcomes query failed: %v", err)
		}

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"watching":     watching,
				"escalations":  loadRecs("bin_relocate"),
				"redeploys":    loadRecs("bin_redeploy"),
				"cadence_ups":  loadRecs("bin_increase_cadence"),
				"applied_7d":   applied,
				"generated_at": now,
			},
		})
	}
}
