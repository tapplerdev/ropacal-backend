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
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

type reoptBin struct {
	ID            string  `db:"id" json:"id"`
	BinNumber     int     `db:"bin_number" json:"bin_number"`
	CurrentStreet string  `db:"current_street" json:"current_street"`
	City          string  `db:"city" json:"city"`
	Latitude      float64 `db:"latitude" json:"latitude"`
	Longitude     float64 `db:"longitude" json:"longitude"`
	FillPct       int     `db:"fill_percentage" json:"fill_percentage"`
}

type reoptRoute struct {
	RouteID              string     `json:"route_id"`
	Name                 string     `json:"name"`
	SuggestedName        string     `json:"suggested_name"`
	BinIDs               []string   `json:"bin_ids"`
	Bins                 []reoptBin `json:"bins"`
	BinCount             int        `json:"bin_count"`
	EstDurationHours     float64    `json:"estimated_duration_hours"`
	EstDistanceMiles     float64    `json:"estimated_distance_miles"`
	AvgFill              float64    `json:"avg_fill"`
	GeographicArea       string     `json:"geographic_area"`
	SchedulePattern      string     `json:"schedule_pattern"`
}

// SmartReoptimize takes existing route template IDs, fetches all active bins,
// runs OR-Tools CVRP to redistribute bins optimally across the same number
// of templates, and returns the proposed new assignments.
func SmartReoptimize(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RouteIDs           []string `json:"route_ids"`
			MaxBinsPerRoute    int      `json:"max_bins_per_route"`
			LowPerformerThresh float64  `json:"low_performer_threshold"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
			return
		}
		if len(req.RouteIDs) == 0 {
			http.Error(w, `{"error":"route_ids required"}`, http.StatusBadRequest)
			return
		}
		if req.MaxBinsPerRoute <= 0 {
			req.MaxBinsPerRoute = 30
		}
		if req.LowPerformerThresh <= 0 {
			req.LowPerformerThresh = 15
		}

		numTemplates := len(req.RouteIDs)
		log.Printf("🤖 [REOPT] Smart reoptimize: %d templates, max %d bins/route, %.0f%% threshold",
			numTemplates, req.MaxBinsPerRoute, req.LowPerformerThresh)

		// Fetch existing route names for mapping
		type routeInfo struct {
			ID   string `db:"id"`
			Name string `db:"name"`
		}
		var existingRoutes []routeInfo
		query, args, _ := sqlx.In(`SELECT id, name FROM routes WHERE id IN (?)`, req.RouteIDs)
		query = db.Rebind(query)
		db.Select(&existingRoutes, query, args...)
		routeNameMap := make(map[string]string)
		for _, r := range existingRoutes {
			routeNameMap[r.ID] = r.Name
		}

		// Fetch all active bins with coordinates
		var allBins []reoptBin
		err := db.Select(&allBins, `
			SELECT id, bin_number, current_street, city, latitude, longitude, fill_percentage
			FROM bins WHERE status = 'active' AND latitude IS NOT NULL AND longitude IS NOT NULL
			ORDER BY bin_number
		`)
		if err != nil {
			log.Printf("❌ [REOPT] Failed to fetch bins: %v", err)
			http.Error(w, `{"error":"failed to fetch bins"}`, http.StatusInternalServerError)
			return
		}

		// Filter out low performers
		var removedBins []reoptBin
		var activeBins []reoptBin
		for _, bin := range allBins {
			var avgFill float64
			var checkCount int
			db.QueryRow(`SELECT COALESCE(AVG(fill_percentage), -1), COUNT(*) FROM checks WHERE bin_id = $1 AND fill_percentage IS NOT NULL`, bin.ID).Scan(&avgFill, &checkCount)

			if checkCount >= 5 && avgFill >= 0 && avgFill < req.LowPerformerThresh {
				removedBins = append(removedBins, bin)
				log.Printf("   ⚠️  Excluding low performer Bin #%d (%.1f%% avg over %d checks)", bin.BinNumber, avgFill, checkCount)
				continue
			}
			activeBins = append(activeBins, bin)
		}

		log.Printf("🤖 [REOPT] %d active bins (%d low performers excluded)", len(activeBins), len(removedBins))

		if len(activeBins) < 2 {
			http.Error(w, `{"error":"not enough active bins"}`, http.StatusBadRequest)
			return
		}

		// Get warehouse location
		var warehouseLat, warehouseLng float64 = 37.6368013, -122.1269379
		var whJSON []byte
		if err := db.QueryRow(`SELECT value FROM config WHERE key = 'warehouse_location'`).Scan(&whJSON); err == nil {
			var wh struct {
				Lat float64 `json:"latitude"`
				Lng float64 `json:"longitude"`
			}
			if json.Unmarshal(whJSON, &wh) == nil {
				warehouseLat, warehouseLng = wh.Lat, wh.Lng
			}
		}

		// Build OSRM distance/duration matrix
		n := len(activeBins) + 1
		coords := make([]string, n)
		coords[0] = fmt.Sprintf("%.6f,%.6f", warehouseLng, warehouseLat)
		for i, b := range activeBins {
			coords[i+1] = fmt.Sprintf("%.6f,%.6f", b.Longitude, b.Latitude)
		}

		osrmURL := os.Getenv("OSRM_SERVER_URL")
		if osrmURL == "" {
			osrmURL = "http://router.project-osrm.org"
		}
		tableURL := fmt.Sprintf("%s/table/v1/driving/%s?annotations=duration,distance", osrmURL, strings.Join(coords, ";"))

		log.Printf("🗺️  [REOPT] Calling OSRM Table API (%d locations)...", n)
		osrmClient := &http.Client{Timeout: 30 * time.Second}
		osrmResp, err := osrmClient.Get(tableURL)
		if err != nil {
			log.Printf("❌ [REOPT] OSRM request failed: %v", err)
			http.Error(w, `{"error":"OSRM request failed"}`, http.StatusInternalServerError)
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
			log.Printf("❌ [REOPT] OSRM response error: %v", err)
			http.Error(w, `{"error":"OSRM returned an error"}`, http.StatusInternalServerError)
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

		// Call OR-Tools with soft capacity: allow +2 overflow to avoid tiny leftover routes
		softCapacity := req.MaxBinsPerRoute + 2

		type ortoolsLoc struct {
			ID   string  `json:"id"`
			Lat  float64 `json:"lat"`
			Lon  float64 `json:"lon"`
			Name string  `json:"name"`
		}
		ortoolsLocs := make([]ortoolsLoc, n)
		ortoolsLocs[0] = ortoolsLoc{ID: "warehouse", Lat: warehouseLat, Lon: warehouseLng, Name: "Warehouse"}
		for i, b := range activeBins {
			ortoolsLocs[i+1] = ortoolsLoc{ID: b.ID, Lat: b.Latitude, Lon: b.Longitude, Name: fmt.Sprintf("Bin #%d", b.BinNumber)}
		}

		ortoolsReq := map[string]interface{}{
			"locations":           ortoolsLocs,
			"distance_matrix":    distMatrix,
			"duration_matrix":    durMatrix,
			"num_vehicles":       numTemplates,
			"vehicle_capacity":   softCapacity,
			"depot_index":        0,
			"max_runtime_seconds": 30,
		}

		ortoolsServiceURL := os.Getenv("ORTOOLS_SERVICE_URL")
		if ortoolsServiceURL == "" {
			ortoolsServiceURL = "http://localhost:8000"
		}

		reqBody, _ := json.Marshal(ortoolsReq)
		log.Printf("🚀 [REOPT] Calling OR-Tools: %d bins, %d vehicles, capacity %d", len(activeBins), numTemplates, softCapacity)

		ortoolsClient := &http.Client{Timeout: 60 * time.Second}
		ortoolsHTTPResp, err := ortoolsClient.Post(ortoolsServiceURL+"/generate-templates", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			log.Printf("❌ [REOPT] OR-Tools request failed: %v", err)
			http.Error(w, `{"error":"OR-Tools request failed"}`, http.StatusInternalServerError)
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
			Unassigned      []int `json:"unassigned"`
			Feasible        bool  `json:"feasible"`
			SolverRuntimeMs int   `json:"solver_runtime_ms"`
		}
		if err := json.Unmarshal(ortoolsBody, &ortoolsResult); err != nil {
			log.Printf("❌ [REOPT] OR-Tools parse error: %v, body: %s", err, string(ortoolsBody))
			http.Error(w, `{"error":"failed to parse optimizer response"}`, http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [REOPT] OR-Tools: %d routes, %dms, feasible=%v, unassigned=%d",
			len(ortoolsResult.Routes), ortoolsResult.SolverRuntimeMs, ortoolsResult.Feasible, len(ortoolsResult.Unassigned))

		// Handle unassigned: distribute to nearest route
		unassignedBins := make([]reoptBin, 0)
		for _, idx := range ortoolsResult.Unassigned {
			if idx > 0 && idx <= len(activeBins) {
				unassignedBins = append(unassignedBins, activeBins[idx-1])
			}
		}

		// Build result routes
		resultRoutes := make([]reoptRoute, 0, len(ortoolsResult.Routes))

		for i, ortRoute := range ortoolsResult.Routes {
			routeBins := make([]reoptBin, 0)
			binIDs := make([]string, 0)

			for _, stopIdx := range ortRoute.StopIndices {
				if stopIdx == 0 {
					continue // skip warehouse
				}
				binIdx := stopIdx - 1
				if binIdx >= 0 && binIdx < len(activeBins) {
					routeBins = append(routeBins, activeBins[binIdx])
					binIDs = append(binIDs, activeBins[binIdx].ID)
				}
			}

			if len(routeBins) == 0 {
				continue
			}

			// Compute avg fill
			totalFill := 0.0
			for _, b := range routeBins {
				totalFill += float64(b.FillPct)
			}
			avgFill := math.Round(totalFill/float64(len(routeBins))*10) / 10

			// Duration: driving + 5 min per bin service
			durationSec := float64(ortRoute.TotalDuration) + float64(len(routeBins))*300
			durationHours := math.Round(durationSec/3600*10) / 10
			distMiles := math.Round(float64(ortRoute.TotalDistance)/1609.34*10) / 10

			// Generate name: dominant city + secondary cities
			cityCount := make(map[string]int)
			for _, b := range routeBins {
				cityCount[b.City]++
			}
			type cityEntry struct {
				City  string
				Count int
			}
			var cities []cityEntry
			for c, cnt := range cityCount {
				cities = append(cities, cityEntry{c, cnt})
			}
			sort.Slice(cities, func(a, b int) bool { return cities[a].Count > cities[b].Count })

			suggestedName := cities[0].City
			if len(cities) > 1 {
				secondary := make([]string, 0)
				for j := 1; j < len(cities) && j <= 2; j++ {
					secondary = append(secondary, cities[j].City)
				}
				if len(cities) > 3 {
					suggestedName += fmt.Sprintf(" + %d areas", len(cities)-1)
				} else if len(secondary) > 0 {
					suggestedName += " / " + strings.Join(secondary, " / ")
				}
			}

			// Map to existing template ID (by index)
			routeID := ""
			originalName := ""
			if i < len(req.RouteIDs) {
				routeID = req.RouteIDs[i]
				originalName = routeNameMap[routeID]
			}

			// Schedule suggestion based on avg fill
			schedule := "Weekly"
			if avgFill >= 60 {
				schedule = "Every 3 days"
			} else if avgFill >= 35 {
				schedule = "Every 5 days"
			}

			resultRoutes = append(resultRoutes, reoptRoute{
				RouteID:          routeID,
				Name:             originalName,
				SuggestedName:    suggestedName,
				BinIDs:           binIDs,
				Bins:             routeBins,
				BinCount:         len(routeBins),
				EstDurationHours: durationHours,
				EstDistanceMiles: distMiles,
				AvgFill:          avgFill,
				GeographicArea:   cities[0].City,
				SchedulePattern:  schedule,
			})
		}

		// Distribute any unassigned bins to nearest routes
		for _, ub := range unassignedBins {
			bestRouteIdx := 0
			bestDist := math.MaxFloat64
			for ri, rr := range resultRoutes {
				for _, rb := range rr.Bins {
					d := haversineDist(ub.Latitude, ub.Longitude, rb.Latitude, rb.Longitude)
					if d < bestDist {
						bestDist = d
						bestRouteIdx = ri
					}
				}
			}
			resultRoutes[bestRouteIdx].Bins = append(resultRoutes[bestRouteIdx].Bins, ub)
			resultRoutes[bestRouteIdx].BinIDs = append(resultRoutes[bestRouteIdx].BinIDs, ub.ID)
			resultRoutes[bestRouteIdx].BinCount++
			log.Printf("   ➕ Unassigned Bin #%d → %s (%.1f mi)", ub.BinNumber, resultRoutes[bestRouteIdx].SuggestedName, bestDist)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"routes":       resultRoutes,
			"removed_bins": removedBins,
			"total_bins":   len(activeBins),
			"solver": map[string]interface{}{
				"runtime_ms": ortoolsResult.SolverRuntimeMs,
				"feasible":   ortoolsResult.Feasible,
				"unassigned": len(ortoolsResult.Unassigned),
			},
		})
	}
}

func haversineDist(lat1, lon1, lat2, lon2 float64) float64 {
	R := 3958.8
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
