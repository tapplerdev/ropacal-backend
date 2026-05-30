package handlers

import (
	"log"
	"math"
	"net/http"
	"sort"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/pkg/utils"

	"github.com/jmoiron/sqlx"
)

// GET /api/manager/bins/daily-priorities
// Returns bins sorted by predicted fill level, grouped by urgency and city.
func GetDailyPriorities(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/manager/bins/daily-priorities")

		_, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		pacific, _ := time.LoadLocation("America/Los_Angeles")
		now := time.Now().In(pacific)
		nowUnix := time.Now().Unix()

		// Step 1: Fetch all active bins
		type binRow struct {
			ID              string   `db:"id"`
			BinNumber       int      `db:"bin_number"`
			CurrentStreet   string   `db:"current_street"`
			City            string   `db:"city"`
			Zip             string   `db:"zip"`
			Latitude        *float64 `db:"latitude"`
			Longitude       *float64 `db:"longitude"`
			FillPercentage  int      `db:"fill_percentage"`
			LastCheckedAt   *int64   `db:"last_checked_at"`
		}
		var bins []binRow
		err := db.Select(&bins, `
			SELECT id, bin_number, current_street, city, zip, latitude, longitude, fill_percentage, last_checked_at
			FROM bins WHERE status = 'active' AND latitude IS NOT NULL
			ORDER BY bin_number
		`)
		if err != nil {
			log.Printf("❌ Error fetching bins: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch bins")
			return
		}

		// Step 2: Fetch check history for fill rate calculation
		// Get consecutive check pairs where fill increased (bin was filling up, not just collected)
		type checkPair struct {
			BinID    string  `db:"bin_id"`
			FillRate float64 `db:"fill_rate"`
		}
		var pairs []checkPair
		err = db.Select(&pairs, `
			WITH ordered_checks AS (
				SELECT bin_id, fill_percentage, checked_on,
					LAG(fill_percentage) OVER (PARTITION BY bin_id ORDER BY checked_on) as prev_fill,
					LAG(checked_on) OVER (PARTITION BY bin_id ORDER BY checked_on) as prev_checked_on
				FROM checks
				WHERE fill_percentage IS NOT NULL
			)
			SELECT bin_id,
				(fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400) as fill_rate
			FROM ordered_checks
			WHERE prev_fill IS NOT NULL
				AND fill_percentage > prev_fill
				AND (checked_on - prev_checked_on) > 3600
				AND (fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400) < 50
		`)
		if err != nil {
			log.Printf("⚠️  Error fetching check pairs: %v", err)
			// Continue with defaults
		}

		// Build per-bin fill rate map
		type rateAccum struct {
			sum   float64
			count int
		}
		rateMap := make(map[string]*rateAccum)
		for _, p := range pairs {
			if _, ok := rateMap[p.BinID]; !ok {
				rateMap[p.BinID] = &rateAccum{}
			}
			rateMap[p.BinID].sum += p.FillRate
			rateMap[p.BinID].count++
		}

		// Step 3: Calculate predictions for each bin
		type priorityBin struct {
			ID                   string  `json:"id"`
			BinNumber            int     `json:"bin_number"`
			CurrentStreet        string  `json:"current_street"`
			City                 string  `json:"city"`
			Zip                  string  `json:"zip"`
			Latitude             float64 `json:"latitude"`
			Longitude            float64 `json:"longitude"`
			LastFillPercentage   int     `json:"last_fill_percentage"`
			DaysSinceCheck       float64 `json:"days_since_check"`
			AvgDailyFillRate     float64 `json:"avg_daily_fill_rate"`
			EstimatedCurrentFill int     `json:"estimated_current_fill"`
			Urgency              string  `json:"urgency"`
			CheckCount           int     `json:"check_count"`
		}

		var priorities []priorityBin
		defaultFillRate := 5.0 // %/day for bins with insufficient data

		for _, bin := range bins {
			daysSinceCheck := 999.0
			if bin.LastCheckedAt != nil && *bin.LastCheckedAt > 0 {
				daysSinceCheck = float64(nowUnix-*bin.LastCheckedAt) / 86400.0
			}

			// Get fill rate
			fillRate := defaultFillRate
			checkCount := 0
			if acc, exists := rateMap[bin.ID]; exists && acc.count >= 2 {
				fillRate = acc.sum / float64(acc.count)
				checkCount = acc.count
			} else if acc, exists := rateMap[bin.ID]; exists {
				checkCount = acc.count
			}

			// Predict current fill
			estimatedFill := float64(bin.FillPercentage) + (daysSinceCheck * fillRate)
			if estimatedFill > 100 {
				estimatedFill = 100
			}
			if estimatedFill < 0 {
				estimatedFill = 0
			}

			// Categorize urgency
			urgency := "low"
			if estimatedFill >= 80 {
				urgency = "critical"
			} else if estimatedFill >= 60 {
				urgency = "high"
			} else if estimatedFill >= 40 {
				urgency = "medium"
			}

			lat := 0.0
			lng := 0.0
			if bin.Latitude != nil {
				lat = *bin.Latitude
			}
			if bin.Longitude != nil {
				lng = *bin.Longitude
			}

			priorities = append(priorities, priorityBin{
				ID:                   bin.ID,
				BinNumber:            bin.BinNumber,
				CurrentStreet:        bin.CurrentStreet,
				City:                 bin.City,
				Zip:                  bin.Zip,
				Latitude:             lat,
				Longitude:            lng,
				LastFillPercentage:   bin.FillPercentage,
				DaysSinceCheck:       math.Round(daysSinceCheck*10) / 10,
				AvgDailyFillRate:     math.Round(fillRate*10) / 10,
				EstimatedCurrentFill: int(math.Round(estimatedFill)),
				Urgency:              urgency,
				CheckCount:           checkCount,
			})
		}

		// Sort by estimated fill descending
		sort.Slice(priorities, func(i, j int) bool {
			return priorities[i].EstimatedCurrentFill > priorities[j].EstimatedCurrentFill
		})

		// Step 4: Build summary and area groupings
		summary := map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0}
		byArea := make(map[string]map[string]interface{})

		for _, p := range priorities {
			summary[p.Urgency]++

			if _, exists := byArea[p.City]; !exists {
				byArea[p.City] = map[string]interface{}{
					"critical": 0,
					"high":     0,
					"medium":   0,
					"low":      0,
					"bins":     []string{},
				}
			}
			area := byArea[p.City]
			area[p.Urgency] = area[p.Urgency].(int) + 1
			area["bins"] = append(area["bins"].([]string), p.ID)
		}

		log.Printf("📊 Daily priorities: %d critical, %d high, %d medium, %d low (as of %s)",
			summary["critical"], summary["high"], summary["medium"], summary["low"],
			now.Format("2006-01-02 15:04"))

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"as_of":      now.Format(time.RFC3339),
				"summary":    summary,
				"priorities":  priorities,
				"by_area":    byArea,
			},
		})
	}
}
