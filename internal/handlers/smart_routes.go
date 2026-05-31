package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
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
	ID                string  `db:"id" json:"id"`
	BinNumber         int     `db:"bin_number" json:"bin_number"`
	CurrentStreet     string  `db:"current_street" json:"current_street"`
	City              string  `db:"city" json:"city"`
	Zip               string  `db:"zip" json:"zip"`
	Latitude          float64 `db:"latitude" json:"latitude"`
	Longitude         float64 `db:"longitude" json:"longitude"`
	FillPercentage    int     `db:"fill_percentage" json:"fill_percentage"`
	LastCheckedAt     *int64  `db:"last_checked_at" json:"-"`
	AvgDailyFillRate  float64 `json:"avg_daily_fill_rate"`
	PredictedDaysTo80 float64 `json:"predicted_days_to_80"`
	Tier              string  `json:"tier"`
	CheckCount        int     `json:"check_count"`
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
	BinCount               int     `json:"bin_count"`
	AvgFillRate            float64 `json:"avg_fill_rate"`
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

		// Parse params
		var params struct {
			MaxBinsPerRoute int `json:"max_bins_per_route"`
		}
		params.MaxBinsPerRoute = 30
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&params)
		}
		if q := r.URL.Query().Get("max_bins_per_route"); q != "" {
			if v, err := strconv.Atoi(q); err == nil {
				params.MaxBinsPerRoute = v
			}
		}
		log.Printf("   Max bins per route: %d", params.MaxBinsPerRoute)

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

		if len(bins) < 2 {
			utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
				"success": true,
				"data": map[string]interface{}{
					"analysis":           map[string]interface{}{"total_active_bins": len(bins)},
					"recommended_routes": []recommendedRoute{},
				},
			})
			return
		}

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

			currentFill := float64(b.FillPercentage)
			if b.LastCheckedAt != nil && *b.LastCheckedAt > 0 {
				daysSince := (nowUnix - float64(*b.LastCheckedAt)) / 86400.0
				currentFill += daysSince * b.AvgDailyFillRate
				if currentFill > 100 {
					currentFill = 100
				}
			}

			if b.AvgDailyFillRate > 0 && currentFill < 80 {
				b.PredictedDaysTo80 = math.Round((80-currentFill)/b.AvgDailyFillRate*10) / 10
			} else if currentFill >= 80 {
				b.PredictedDaysTo80 = 0
			} else {
				b.PredictedDaysTo80 = 99
			}

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
		// Step 4: Fetch warehouse location
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		var warehouseLat, warehouseLng float64
		var warehouseJSON []byte
		whErr := db.QueryRow(`SELECT value FROM config WHERE key = 'warehouse_location'`).Scan(&warehouseJSON)
		if whErr == nil {
			var wh struct {
				Latitude  float64 `json:"latitude"`
				Longitude float64 `json:"longitude"`
			}
			json.Unmarshal(warehouseJSON, &wh)
			warehouseLat = wh.Latitude
			warehouseLng = wh.Longitude
		}
		if warehouseLat == 0 {
			warehouseLat = 37.6368013
			warehouseLng = -122.1269379
		}
		log.Printf("🏭 Warehouse: (%.6f, %.6f)", warehouseLat, warehouseLng)

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// Step 5: Build OSRM distance/duration matrix
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// Index 0 = warehouse, indices 1..N = bins
		n := len(bins) + 1
		numVehicles := int(math.Ceil(float64(len(bins)) / float64(params.MaxBinsPerRoute)))
		if numVehicles < 1 {
			numVehicles = 1
		}

		// Build coordinate string for OSRM
		coords := make([]string, n)
		coords[0] = fmt.Sprintf("%.6f,%.6f", warehouseLng, warehouseLat) // OSRM uses lon,lat
		for i, b := range bins {
			coords[i+1] = fmt.Sprintf("%.6f,%.6f", b.Longitude, b.Latitude)
		}

		osrmURL := os.Getenv("OSRM_SERVER_URL")
		if osrmURL == "" {
			osrmURL = "http://router.project-osrm.org"
		}
		tableURL := fmt.Sprintf("%s/table/v1/driving/%s?annotations=duration,distance", osrmURL, strings.Join(coords, ";"))

		log.Printf("🗺️  Calling OSRM Table API (%d locations)...", n)
		osrmClient := &http.Client{Timeout: 30 * time.Second}
		osrmResp, err := osrmClient.Get(tableURL)
		if err != nil {
			log.Printf("❌ OSRM request failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch distance matrix from OSRM")
			return
		}
		defer osrmResp.Body.Close()

		osrmBody, _ := io.ReadAll(osrmResp.Body)
		var osrmData struct {
			Code      string      `json:"code"`
			Durations [][]float64 `json:"durations"`
			Distances [][]float64 `json:"distances"`
		}
		if err := json.Unmarshal(osrmBody, &osrmData); err != nil || osrmData.Code != "Ok" {
			log.Printf("❌ OSRM response error: code=%s, err=%v", osrmData.Code, err)
			utils.RespondError(w, http.StatusInternalServerError, "OSRM returned an error")
			return
		}

		// Convert to int matrices
		distMatrix := make([][]int, n)
		durMatrix := make([][]int, n)
		for i := 0; i < n; i++ {
			distMatrix[i] = make([]int, n)
			durMatrix[i] = make([]int, n)
			for j := 0; j < n; j++ {
				distMatrix[i][j] = int(osrmData.Distances[i][j])
				durMatrix[i][j] = int(osrmData.Durations[i][j])
			}
		}
		log.Printf("✅ OSRM matrix: %dx%d", n, n)

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// Step 6: Call OR-Tools multi-vehicle CVRP
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		type ortoolsLocation struct {
			ID   string  `json:"id"`
			Lat  float64 `json:"lat"`
			Lon  float64 `json:"lon"`
			Name string  `json:"name"`
		}

		ortoolsLocs := make([]ortoolsLocation, n)
		ortoolsLocs[0] = ortoolsLocation{ID: "warehouse", Lat: warehouseLat, Lon: warehouseLng, Name: "Warehouse"}
		for i, b := range bins {
			ortoolsLocs[i+1] = ortoolsLocation{ID: b.ID, Lat: b.Latitude, Lon: b.Longitude, Name: fmt.Sprintf("Bin #%d", b.BinNumber)}
		}

		ortoolsReq := map[string]interface{}{
			"locations":           ortoolsLocs,
			"distance_matrix":    distMatrix,
			"duration_matrix":    durMatrix,
			"num_vehicles":       numVehicles,
			"vehicle_capacity":   params.MaxBinsPerRoute,
			"depot_index":        0,
			"max_runtime_seconds": 30,
		}

		ortoolsServiceURL := os.Getenv("ORTOOLS_SERVICE_URL")
		if ortoolsServiceURL == "" {
			ortoolsServiceURL = "http://localhost:8000"
		}

		reqBody, _ := json.Marshal(ortoolsReq)
		log.Printf("🚀 Calling OR-Tools CVRP: %d locations, %d vehicles, capacity %d", n, numVehicles, params.MaxBinsPerRoute)

		ortoolsClient := &http.Client{Timeout: 60 * time.Second}
		ortoolsHTTPResp, err := ortoolsClient.Post(ortoolsServiceURL+"/generate-templates", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			log.Printf("❌ OR-Tools request failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to call route optimizer")
			return
		}
		defer ortoolsHTTPResp.Body.Close()

		ortoolsBody, _ := io.ReadAll(ortoolsHTTPResp.Body)
		var ortoolsResult struct {
			Routes []struct {
				VehicleID     int   `json:"vehicle_id"`
				StopIndices   []int `json:"stop_indices"`
				TotalDistance  int   `json:"total_distance"`
				TotalDuration int   `json:"total_duration"`
			} `json:"routes"`
			Unassigned     []int `json:"unassigned"`
			Feasible       bool  `json:"feasible"`
			SolverRuntimeMs int  `json:"solver_runtime_ms"`
		}
		if err := json.Unmarshal(ortoolsBody, &ortoolsResult); err != nil {
			log.Printf("❌ OR-Tools response parse error: %v", err)
			log.Printf("   Response body: %s", string(ortoolsBody))
			utils.RespondError(w, http.StatusInternalServerError, "Failed to parse optimizer response")
			return
		}
		log.Printf("✅ OR-Tools returned %d routes in %dms (feasible=%v, unassigned=%d)",
			len(ortoolsResult.Routes), ortoolsResult.SolverRuntimeMs, ortoolsResult.Feasible, len(ortoolsResult.Unassigned))

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// Step 7: Build route recommendations from OR-Tools results
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		var allRoutes []recommendedRoute

		// Track how many routes each city appears in (for directional naming)
		cityRouteCount := make(map[string]int)
		type routeCityInfo struct {
			dominantCity string
			avgLat       float64
		}
		var routeCityInfos []routeCityInfo

		// First pass: determine dominant city per route
		for _, ortRoute := range ortoolsResult.Routes {
			if len(ortRoute.StopIndices) == 0 {
				continue
			}
			cityCounts := make(map[string]int)
			totalLat := 0.0
			for _, nodeIdx := range ortRoute.StopIndices {
				binIdx := nodeIdx - 1 // offset by 1 (index 0 = warehouse)
				if binIdx >= 0 && binIdx < len(bins) {
					cityCounts[bins[binIdx].City]++
					totalLat += bins[binIdx].Latitude
				}
			}
			dominant := ""
			maxCount := 0
			for city, count := range cityCounts {
				if count > maxCount {
					dominant = city
					maxCount = count
				}
			}
			cityRouteCount[dominant]++
			routeCityInfos = append(routeCityInfos, routeCityInfo{
				dominantCity: dominant,
				avgLat:       totalLat / float64(len(ortRoute.StopIndices)),
			})
		}

		// Second pass: build routes with proper names
		cityLatitudes := make(map[string][]float64) // city → list of avg lats per route
		for _, info := range routeCityInfos {
			cityLatitudes[info.dominantCity] = append(cityLatitudes[info.dominantCity], info.avgLat)
		}
		// Sort latitudes per city for directional assignment
		for city := range cityLatitudes {
			sort.Float64s(cityLatitudes[city])
		}
		cityDirIndex := make(map[string]int)

		routeInfoIdx := 0
		for _, ortRoute := range ortoolsResult.Routes {
			if len(ortRoute.StopIndices) == 0 {
				continue
			}

			// Collect bins for this route (in OR-Tools optimized order)
			var routeBins []smartBin
			var binIDs []string
			var totalFillRate float64
			cityCounts := make(map[string]int)
			tierBuckets := map[string]int{"high": 0, "medium": 0, "low": 0}

			for _, nodeIdx := range ortRoute.StopIndices {
				binIdx := nodeIdx - 1
				if binIdx >= 0 && binIdx < len(bins) {
					b := bins[binIdx]
					routeBins = append(routeBins, b)
					binIDs = append(binIDs, b.ID)
					totalFillRate += b.AvgDailyFillRate
					cityCounts[b.City]++
					tierBuckets[b.Tier]++
				}
			}

			if len(routeBins) == 0 {
				routeInfoIdx++
				continue
			}

			// Cluster tier = majority
			clusterTier := "low"
			if tierBuckets["high"] >= tierBuckets["medium"] && tierBuckets["high"] >= tierBuckets["low"] {
				clusterTier = "high"
			} else if tierBuckets["medium"] >= tierBuckets["low"] {
				clusterTier = "medium"
			}

			// Schedule pattern
			schedule := "Every 10-14 days"
			if clusterTier == "high" {
				schedule = "Every 3 days"
			} else if clusterTier == "medium" {
				schedule = "Every 5-7 days"
			}

			// Build name
			info := routeCityInfos[routeInfoIdx]
			dominant := info.dominantCity
			tierLabel := strings.ToUpper(clusterTier[:1]) + clusterTier[1:]

			name := dominant
			if cityRouteCount[dominant] > 1 {
				// Assign directional label based on latitude rank
				lats := cityLatitudes[dominant]
				latIdx := 0
				for i, l := range lats {
					if math.Abs(l-info.avgLat) < 0.001 {
						latIdx = i
						break
					}
				}
				dirLabels := []string{"South", "Central", "North"}
				if len(lats) == 2 {
					dirLabels = []string{"South", "North"}
				} else if len(lats) > 3 {
					dirLabels = nil
					for k := 0; k < len(lats); k++ {
						dirLabels = append(dirLabels, fmt.Sprintf("Area %d", k+1))
					}
				}
				if latIdx < len(dirLabels) {
					name += " " + dirLabels[latIdx]
				}
				cityDirIndex[dominant]++
			}

			// Add secondary cities
			var others []string
			for city := range cityCounts {
				if city != dominant {
					others = append(others, city)
				}
			}
			sort.Strings(others)
			if len(others) > 0 && len(others) <= 2 {
				name += " / " + strings.Join(others, " / ")
			} else if len(others) > 2 {
				name += fmt.Sprintf(" + %d areas", len(others))
			}
			name += " — " + tierLabel + " Priority"

			// Distance/duration from OR-Tools (convert meters → miles, seconds → hours)
			distMiles := float64(ortRoute.TotalDistance) / 1609.34
			durHours := float64(ortRoute.TotalDuration) / 3600.0

			allRoutes = append(allRoutes, recommendedRoute{
				SuggestedName:   name,
				GeographicArea:  dominant,
				SchedulePattern: schedule,
				Tier:            clusterTier,
				BinIDs:          binIDs,
				Bins:            routeBins,
				Stats: routeStats{
					BinCount:               len(routeBins),
					AvgFillRate:            math.Round(totalFillRate/float64(len(routeBins))*10) / 10,
					EstimatedDurationHours: math.Round(durHours*10) / 10,
					EstimatedDistanceMiles: math.Round(distMiles*10) / 10,
				},
			})
			routeInfoIdx++
		}

		// Sort: high first, then by bin count
		tierOrder := map[string]int{"high": 0, "medium": 1, "low": 2}
		sort.Slice(allRoutes, func(i, j int) bool {
			if tierOrder[allRoutes[i].Tier] != tierOrder[allRoutes[j].Tier] {
				return tierOrder[allRoutes[i].Tier] < tierOrder[allRoutes[j].Tier]
			}
			return allRoutes[i].Stats.BinCount > allRoutes[j].Stats.BinCount
		})

		log.Printf("✅ Generated %d route recommendations", len(allRoutes))

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// Debug: Log each route for analysis
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("📊 SMART ROUTE GENERATION RESULTS")
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		for i, route := range allRoutes {
			log.Printf("")
			log.Printf("  Route %d: %s", i+1, route.SuggestedName)
			log.Printf("    Tier: %s | Schedule: %s", route.Tier, route.SchedulePattern)
			log.Printf("    Bins: %d | Distance: %.1f mi | Duration: %.1f hrs",
				route.Stats.BinCount, route.Stats.EstimatedDistanceMiles, route.Stats.EstimatedDurationHours)
			log.Printf("    Avg fill rate: %.1f%%/day", route.Stats.AvgFillRate)
			log.Printf("    Cities: %s", route.GeographicArea)
			log.Printf("    Bin order:")
			for j, b := range route.Bins {
				log.Printf("      %d. Bin #%d — %s, %s (%.1f%%/day, tier=%s)",
					j+1, b.BinNumber, b.CurrentStreet, b.City, b.AvgDailyFillRate, b.Tier)
			}
		}
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"analysis": map[string]interface{}{
					"total_active_bins":    len(bins),
					"bins_with_check_data": len(rateMap),
					"tiers":               tierCounts,
					"optimizer": map[string]interface{}{
						"solver_runtime_ms": ortoolsResult.SolverRuntimeMs,
						"num_vehicles":      numVehicles,
						"feasible":          ortoolsResult.Feasible,
						"unassigned":        len(ortoolsResult.Unassigned),
					},
				},
				"recommended_routes": allRoutes,
			},
		})
	}
}
