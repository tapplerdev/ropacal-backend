package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"

	"ropacal-backend/internal/geo"
	"ropacal-backend/internal/orgdb"

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
	RouteID          string     `json:"route_id"`
	Name             string     `json:"name"`
	SuggestedName    string     `json:"suggested_name"`
	BinIDs           []string   `json:"bin_ids"`
	Bins             []reoptBin `json:"bins"`
	BinCount         int        `json:"bin_count"`
	EstDurationHours float64    `json:"estimated_duration_hours"`
	EstDistanceMiles float64    `json:"estimated_distance_miles"`
	AvgFill          float64    `json:"avg_fill"`
	GeographicArea   string     `json:"geographic_area"`
	SchedulePattern  string     `json:"schedule_pattern"`
}

// SmartReoptimize takes existing route template IDs, fetches all active bins,
// runs OR-Tools CVRP to redistribute bins optimally across the same number
// of templates, and returns the proposed new assignments.
func SmartReoptimize(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
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

		// Flag low performers (but keep them on routes — they still need collection)
		type lowPerformer struct {
			reoptBin
			AvgFill    float64 `json:"avg_fill"`
			CheckCount int     `json:"check_count"`
		}
		var lowPerformers []lowPerformer
		activeBins := allBins // All active bins stay in the optimization
		for _, bin := range allBins {
			var avgFill float64
			var checkCount int
			db.QueryRow(`SELECT COALESCE(AVG(fill_percentage), -1), COUNT(*) FROM checks WHERE bin_id = $1 AND fill_percentage IS NOT NULL`, bin.ID).Scan(&avgFill, &checkCount)

			if checkCount >= 5 && avgFill >= 0 && avgFill < req.LowPerformerThresh {
				lowPerformers = append(lowPerformers, lowPerformer{bin, math.Round(avgFill), checkCount})
				log.Printf("   ⚠️  Low performer Bin #%d (%.1f%% avg over %d checks) — flagged, kept on route", bin.BinNumber, avgFill, checkCount)
			}
		}

		log.Printf("🤖 [REOPT] %d active bins (%d flagged as low performers)", len(activeBins), len(lowPerformers))

		if len(activeBins) < 2 {
			http.Error(w, `{"error":"not enough active bins"}`, http.StatusBadRequest)
			return
		}

		// Get warehouse location
		warehouseLat, warehouseLng := fetchWarehouseLocation(db)

		// Build OSRM distance/duration matrix
		n := len(activeBins) + 1
		coords := make([]string, n)
		coords[0] = fmt.Sprintf("%.6f,%.6f", warehouseLng, warehouseLat)
		for i, b := range activeBins {
			coords[i+1] = fmt.Sprintf("%.6f,%.6f", b.Longitude, b.Latitude)
		}

		log.Printf("🗺️  [REOPT] Calling OSRM Table API (%d locations)...", n)
		distMatrix, durMatrix, err := fetchOSRMMatrices(coords)
		if err != nil {
			if errors.Is(err, errOSRMRequest) {
				log.Printf("❌ [REOPT] OSRM request failed: %v", err)
				http.Error(w, `{"error":"OSRM request failed"}`, http.StatusInternalServerError)
				return
			}
			log.Printf("❌ [REOPT] OSRM response error: %v", err)
			http.Error(w, `{"error":"OSRM returned an error"}`, http.StatusInternalServerError)
			return
		}

		// Calculate number of routes from bin count / max capacity
		numVehicles := int(math.Ceil(float64(len(activeBins)) / float64(req.MaxBinsPerRoute)))
		if numVehicles < 1 {
			numVehicles = 1
		}
		// Balance capacity: ceil(bins/vehicles) + 1 so each route gets roughly equal load
		// e.g. 82 bins / 3 vehicles = 28 per route (not 32/32/18)
		balancedCapacity := int(math.Ceil(float64(len(activeBins))/float64(numVehicles))) + 1

		ortoolsLocs := make([]ortoolsLocation, n)
		ortoolsLocs[0] = ortoolsLocation{ID: "warehouse", Lat: warehouseLat, Lon: warehouseLng, Name: "Warehouse"}
		for i, b := range activeBins {
			ortoolsLocs[i+1] = ortoolsLocation{ID: b.ID, Lat: b.Latitude, Lon: b.Longitude, Name: fmt.Sprintf("Bin #%d", b.BinNumber)}
		}

		log.Printf("🤖 [REOPT] %d bins / %d max = %d vehicles, balanced capacity %d", len(activeBins), req.MaxBinsPerRoute, numVehicles, balancedCapacity)
		log.Printf("🚀 [REOPT] Calling OR-Tools: %d bins, %d vehicles, capacity %d", len(activeBins), numVehicles, balancedCapacity)

		ortoolsResult, err := callORTools(ortoolsLocs, distMatrix, durMatrix, numVehicles, balancedCapacity)
		if err != nil {
			if errors.Is(err, errORToolsParse) {
				log.Printf("❌ [REOPT] OR-Tools parse error: %v", err)
				http.Error(w, `{"error":"failed to parse optimizer response"}`, http.StatusInternalServerError)
				return
			}
			log.Printf("❌ [REOPT] OR-Tools request failed: %v", err)
			http.Error(w, `{"error":"OR-Tools request failed"}`, http.StatusInternalServerError)
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
		type routeCityData struct {
			dominantCity string
			avgLat       float64
			baseName     string
		}
		var routeDominantCities []routeCityData

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

			// Generate base name: dominant city + secondary cities
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

			dominantCity := cities[0].City
			baseName := dominantCity
			if len(cities) > 3 {
				baseName += fmt.Sprintf(" + %d areas", len(cities)-1)
			} else if len(cities) > 1 {
				secondary := make([]string, 0)
				for j := 1; j < len(cities) && j <= 2; j++ {
					secondary = append(secondary, cities[j].City)
				}
				baseName += " / " + strings.Join(secondary, " / ")
			}

			// Track for directional suffix (applied after all routes built)
			avgLat := 0.0
			for _, b := range routeBins {
				avgLat += b.Latitude
			}
			avgLat /= float64(len(routeBins))
			routeDominantCities = append(routeDominantCities, routeCityData{dominantCity, avgLat, baseName})

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
				SuggestedName:    baseName,
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

		// Apply directional suffixes for routes with same dominant city
		cityCounts := make(map[string]int)
		for _, rcd := range routeDominantCities {
			cityCounts[rcd.dominantCity]++
		}
		for city, count := range cityCounts {
			if count <= 1 {
				continue
			}
			// Collect indices of routes with this dominant city, sorted by latitude
			type idxLat struct {
				idx    int
				avgLat float64
			}
			var entries []idxLat
			for i, rcd := range routeDominantCities {
				if rcd.dominantCity == city {
					entries = append(entries, idxLat{i, rcd.avgLat})
				}
			}
			sort.Slice(entries, func(a, b int) bool { return entries[a].avgLat < entries[b].avgLat })
			suffixes := directionalSuffixes(len(entries))
			for j, e := range entries {
				if e.idx < len(resultRoutes) {
					resultRoutes[e.idx].SuggestedName = routeDominantCities[e.idx].baseName + " — " + suffixes[j]
				}
			}
		}

		// Distribute any unassigned bins to nearest routes
		for _, ub := range unassignedBins {
			bestRouteIdx := 0
			bestDist := math.MaxFloat64
			for ri, rr := range resultRoutes {
				for _, rb := range rr.Bins {
					d := geo.HaversineMiles(ub.Latitude, ub.Longitude, rb.Latitude, rb.Longitude)
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

		// Find template IDs that got no bins (should be deleted)
		usedRouteIDs := make(map[string]bool)
		for _, r := range resultRoutes {
			if r.RouteID != "" {
				usedRouteIDs[r.RouteID] = true
			}
		}
		var deleteRouteIDs []string
		for _, id := range req.RouteIDs {
			if !usedRouteIDs[id] {
				deleteRouteIDs = append(deleteRouteIDs, id)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"routes":           resultRoutes,
			"low_performers":   lowPerformers,
			"delete_route_ids": deleteRouteIDs,
			"total_bins":       len(activeBins),
			"solver": map[string]interface{}{
				"runtime_ms":        ortoolsResult.SolverRuntimeMs,
				"feasible":          ortoolsResult.Feasible,
				"unassigned":        len(ortoolsResult.Unassigned),
				"num_vehicles":      numVehicles,
				"balanced_capacity": balancedCapacity,
			},
		})
	}
}
