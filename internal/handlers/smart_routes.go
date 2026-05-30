package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/pkg/utils"

	"github.com/jmoiron/sqlx"
)

// haversineDistanceMiles returns the distance in miles between two lat/lon points.
func haversineDistanceMiles(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusMiles = 3958.8
	lat1Rad := lat1 * math.Pi / 180
	lat2Rad := lat2 * math.Pi / 180
	deltaLat := (lat2 - lat1) * math.Pi / 180
	deltaLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(deltaLat/2)*math.Sin(deltaLat/2) +
		math.Cos(lat1Rad)*math.Cos(lat2Rad)*
			math.Sin(deltaLon/2)*math.Sin(deltaLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMiles * c
}

type smartBin struct {
	ID               string  `db:"id" json:"id"`
	BinNumber        int     `db:"bin_number" json:"bin_number"`
	CurrentStreet    string  `db:"current_street" json:"current_street"`
	City             string  `db:"city" json:"city"`
	Zip              string  `db:"zip" json:"zip"`
	Latitude         float64 `db:"latitude" json:"latitude"`
	Longitude        float64 `db:"longitude" json:"longitude"`
	FillPercentage   int     `db:"fill_percentage" json:"fill_percentage"`
	LastCheckedAt    *int64  `db:"last_checked_at" json:"-"`
	AvgDailyFillRate float64 `json:"avg_daily_fill_rate"`
	PredictedDaysTo80 float64 `json:"predicted_days_to_80"`
	Tier             string  `json:"tier"` // high, medium, low
	CheckCount       int     `json:"check_count"`
}

type recommendedRoute struct {
	SuggestedName   string     `json:"suggested_name"`
	GeographicArea  string     `json:"geographic_area"`
	SchedulePattern string     `json:"schedule_pattern"`
	Tier            string     `json:"tier"`
	BinIDs          []string   `json:"bin_ids"`
	Bins            []smartBin `json:"bins"`
	Stats           routeStats `json:"stats"`
}

type routeStats struct {
	BinCount              int     `json:"bin_count"`
	AvgFillRate           float64 `json:"avg_fill_rate"`
	EstimatedDurationHours float64 `json:"estimated_duration_hours"`
	EstimatedDistanceMiles float64 `json:"estimated_distance_miles"`
}

// POST /api/manager/routes/generate-smart
func GenerateSmartRoutes(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: POST /api/manager/routes/generate-smart")

		_, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		// Parse optional params
		var params struct {
			RadiusMiles    float64 `json:"radius_miles"`
			MaxBinsPerRoute int    `json:"max_bins_per_route"`
		}
		params.RadiusMiles = 5.0
		params.MaxBinsPerRoute = 35
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&params)
		}
		if q := r.URL.Query().Get("radius_miles"); q != "" {
			if v, err := strconv.ParseFloat(q, 64); err == nil { params.RadiusMiles = v }
		}
		if q := r.URL.Query().Get("max_bins_per_route"); q != "" {
			if v, err := strconv.Atoi(q); err == nil { params.MaxBinsPerRoute = v }
		}

		log.Printf("   Params: radius=%.1f mi, max_bins=%d", params.RadiusMiles, params.MaxBinsPerRoute)

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// Step 1: Fetch all active bins
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		var bins []smartBin
		err := db.Select(&bins, `
			SELECT id, bin_number, current_street, city, zip, latitude, longitude, fill_percentage, last_checked_at
			FROM bins WHERE status = 'active' AND latitude IS NOT NULL AND longitude IS NOT NULL
			ORDER BY bin_number
		`)
		if err != nil {
			log.Printf("❌ Error fetching bins: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch bins")
			return
		}

		log.Printf("📊 Fetched %d active bins", len(bins))

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// Step 2: Calculate fill rates from check history
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		type fillRateRow struct {
			BinID    string  `db:"bin_id"`
			FillRate float64 `db:"fill_rate"`
		}
		var pairs []fillRateRow
		db.Select(&pairs, `
			WITH ordered_checks AS (
				SELECT bin_id, fill_percentage, checked_on,
					LAG(fill_percentage) OVER (PARTITION BY bin_id ORDER BY checked_on) as prev_fill,
					LAG(checked_on) OVER (PARTITION BY bin_id ORDER BY checked_on) as prev_checked_on
				FROM checks WHERE fill_percentage IS NOT NULL
			)
			SELECT bin_id,
				(fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400) as fill_rate
			FROM ordered_checks
			WHERE prev_fill IS NOT NULL
				AND fill_percentage > prev_fill
				AND (checked_on - prev_checked_on) > 3600
				AND (fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400) < 50
		`)

		// Aggregate per bin
		type rateAccum struct { sum float64; count int }
		rateMap := make(map[string]*rateAccum)
		for _, p := range pairs {
			if _, ok := rateMap[p.BinID]; !ok {
				rateMap[p.BinID] = &rateAccum{}
			}
			rateMap[p.BinID].sum += p.FillRate
			rateMap[p.BinID].count++
		}

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// Step 3: Assign fill rates and tiers
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		defaultRate := 5.0
		nowUnix := float64(time.Now().Unix())

		tierCounts := map[string]int{"high": 0, "medium": 0, "low": 0}

		for i := range bins {
			b := &bins[i]
			if acc, exists := rateMap[b.ID]; exists && acc.count >= 2 {
				b.AvgDailyFillRate = math.Round(acc.sum/float64(acc.count)*10) / 10
				b.CheckCount = acc.count
			} else {
				b.AvgDailyFillRate = defaultRate
				if acc, exists := rateMap[b.ID]; exists {
					b.CheckCount = acc.count
				}
			}

			// Predict days until 80%
			currentFill := float64(b.FillPercentage)
			if b.LastCheckedAt != nil && *b.LastCheckedAt > 0 {
				daysSince := (nowUnix - float64(*b.LastCheckedAt)) / 86400.0
				currentFill += daysSince * b.AvgDailyFillRate
				if currentFill > 100 { currentFill = 100 }
			}

			if b.AvgDailyFillRate > 0 && currentFill < 80 {
				b.PredictedDaysTo80 = math.Round((80-currentFill)/b.AvgDailyFillRate*10) / 10
			} else if currentFill >= 80 {
				b.PredictedDaysTo80 = 0
			} else {
				b.PredictedDaysTo80 = 99
			}

			// Tier assignment
			if b.PredictedDaysTo80 <= 4 {
				b.Tier = "high"
			} else if b.PredictedDaysTo80 <= 9 {
				b.Tier = "medium"
			} else {
				b.Tier = "low"
			}
			tierCounts[b.Tier]++
		}

		log.Printf("📊 Tiers: %d high, %d medium, %d low", tierCounts["high"], tierCounts["medium"], tierCounts["low"])

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// Step 4: Geographic clustering (tier-independent)
		// Cluster ALL bins by proximity first, then assign tier within each cluster
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		const maxClusterDiameter = 8.0 // miles — prevents sprawling clusters

		// Sort all bins by latitude for spatial sweep
		sort.Slice(bins, func(i, j int) bool {
			return bins[i].Latitude < bins[j].Latitude
		})

		// Centroid-based greedy clustering
		type cluster struct {
			bins    []smartBin
			centerLat float64
			centerLng float64
		}
		assigned := make(map[string]bool)
		var clusters []cluster

		for i := range bins {
			if assigned[bins[i].ID] {
				continue
			}
			c := cluster{
				bins:      []smartBin{bins[i]},
				centerLat: bins[i].Latitude,
				centerLng: bins[i].Longitude,
			}
			assigned[bins[i].ID] = true

			// Keep scanning for nearby bins, checking distance from cluster CENTER
			changed := true
			for changed {
				changed = false
				for j := range bins {
					if assigned[bins[j].ID] {
						continue
					}
					dist := haversineDistanceMiles(c.centerLat, c.centerLng, bins[j].Latitude, bins[j].Longitude)
					if dist <= maxClusterDiameter/2 {
						c.bins = append(c.bins, bins[j])
						assigned[bins[j].ID] = true
						// Recalculate center
						c.centerLat = 0
						c.centerLng = 0
						for _, b := range c.bins {
							c.centerLat += b.Latitude
							c.centerLng += b.Longitude
						}
						c.centerLat /= float64(len(c.bins))
						c.centerLng /= float64(len(c.bins))
						changed = true
					}
				}
			}

			// Split oversized clusters by latitude
			if len(c.bins) > params.MaxBinsPerRoute {
				sort.Slice(c.bins, func(a, b int) bool { return c.bins[a].Latitude < c.bins[b].Latitude })
				for k := 0; k < len(c.bins); k += params.MaxBinsPerRoute {
					end := k + params.MaxBinsPerRoute
					if end > len(c.bins) { end = len(c.bins) }
					sub := c.bins[k:end]
					sLat, sLng := 0.0, 0.0
					for _, b := range sub { sLat += b.Latitude; sLng += b.Longitude }
					clusters = append(clusters, cluster{bins: sub, centerLat: sLat / float64(len(sub)), centerLng: sLng / float64(len(sub))})
				}
			} else {
				clusters = append(clusters, c)
			}
		}

		log.Printf("📊 Created %d geographic clusters", len(clusters))

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// Step 5: Build route recommendations from clusters
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		var allRoutes []recommendedRoute

		// Track city occurrence for sub-naming (e.g., "San Jose North", "San Jose South")
		cityClusterCount := make(map[string]int)
		for _, c := range clusters {
			cities := make(map[string]int)
			for _, b := range c.bins { cities[b.City]++ }
			dominant := ""
			maxC := 0
			for city, cnt := range cities { if cnt > maxC { dominant = city; maxC = cnt } }
			cityClusterCount[dominant]++
		}

		cityClusterIndex := make(map[string]int)

		for _, c := range clusters {
			if len(c.bins) == 0 {
				continue
			}

			// Determine dominant city and tier
			cityCounts := make(map[string]int)
			var totalFillRate float64
			var binIDs []string
			tierBuckets := map[string]int{"high": 0, "medium": 0, "low": 0}
			for _, b := range c.bins {
				cityCounts[b.City]++
				totalFillRate += b.AvgDailyFillRate
				binIDs = append(binIDs, b.ID)
				tierBuckets[b.Tier]++
			}
			dominantCity := ""
			maxCount := 0
			for city, count := range cityCounts {
				if count > maxCount { dominantCity = city; maxCount = count }
			}

			// Cluster tier = majority tier of its bins
			clusterTier := "low"
			if tierBuckets["high"] >= tierBuckets["medium"] && tierBuckets["high"] >= tierBuckets["low"] {
				clusterTier = "high"
			} else if tierBuckets["medium"] >= tierBuckets["low"] {
				clusterTier = "medium"
			}

			// Estimate route distance
			sorted := make([]smartBin, len(c.bins))
			copy(sorted, c.bins)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].Latitude < sorted[j].Latitude })
			totalDist := 0.0
			for k := 1; k < len(sorted); k++ {
				totalDist += haversineDistanceMiles(sorted[k-1].Latitude, sorted[k-1].Longitude, sorted[k].Latitude, sorted[k].Longitude)
			}

			// Estimate duration: 5 min/bin service + driving at 25 mph avg
			serviceMins := float64(len(c.bins)) * 5
			drivingMins := (totalDist / 25.0) * 60
			totalHours := (serviceMins + drivingMins) / 60.0

			// Schedule pattern based on tier
			schedule := "Weekly"
			if clusterTier == "high" {
				schedule = "Every 3 days"
			} else if clusterTier == "medium" {
				schedule = "Every 5-7 days"
			} else {
				schedule = "Every 10-14 days"
			}

			// Name — add directional suffix if city has multiple clusters
			tierLabel := strings.ToUpper(clusterTier[:1]) + clusterTier[1:]
			name := dominantCity
			if cityClusterCount[dominantCity] > 1 {
				cityClusterIndex[dominantCity]++
				idx := cityClusterIndex[dominantCity]
				// Use directional label based on latitude relative to city's other clusters
				dirLabels := []string{"South", "Central", "North", "East", "West"}
				if idx <= len(dirLabels) {
					name = dominantCity + " " + dirLabels[idx-1]
				} else {
					name = fmt.Sprintf("%s %d", dominantCity, idx)
				}
			}
			if len(cityCounts) > 1 {
				// Multi-city cluster — list secondary cities
				var others []string
				for city := range cityCounts {
					if city != dominantCity { others = append(others, city) }
				}
				sort.Strings(others)
				if len(others) <= 2 {
					name += " / " + strings.Join(others, " / ")
				}
			}
			name += " — " + tierLabel + " Priority"

			allRoutes = append(allRoutes, recommendedRoute{
				SuggestedName:   name,
				GeographicArea:  dominantCity,
				SchedulePattern: schedule,
				Tier:            clusterTier,
				BinIDs:          binIDs,
				Bins:            c.bins,
				Stats: routeStats{
					BinCount:               len(c.bins),
					AvgFillRate:            math.Round(totalFillRate/float64(len(c.bins))*10) / 10,
					EstimatedDurationHours: math.Round(totalHours*10) / 10,
					EstimatedDistanceMiles: math.Round(totalDist*10) / 10,
				},
			})
		}

		// Sort routes: high tier first, then by bin count descending
		tierOrder := map[string]int{"high": 0, "medium": 1, "low": 2}
		sort.Slice(allRoutes, func(i, j int) bool {
			if tierOrder[allRoutes[i].Tier] != tierOrder[allRoutes[j].Tier] {
				return tierOrder[allRoutes[i].Tier] < tierOrder[allRoutes[j].Tier]
			}
			return allRoutes[i].Stats.BinCount > allRoutes[j].Stats.BinCount
		})

		log.Printf("✅ Generated %d route recommendations", len(allRoutes))

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"analysis": map[string]interface{}{
					"total_active_bins":     len(bins),
					"bins_with_check_data":  len(rateMap),
					"tiers":                tierCounts,
				},
				"recommended_routes": allRoutes,
			},
		})
	}
}

