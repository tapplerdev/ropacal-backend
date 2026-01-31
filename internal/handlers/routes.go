package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"time"

	"ropacal-backend/internal/models"
	"ropacal-backend/internal/services"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// HERE Maps API credentials
const (
	HereAppID  = "Ne2aZIKc9CIno1Fw4RyB"
	HereAPIKey = "0kdpGpu5ZODbrzc6QDiPmSarJSD_6BpqyCdm3ghNuzc"
)

// GetRoutes returns all route blueprints
func GetRoutes(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var routes []models.Route
		err := db.Select(&routes, `
			SELECT id, name, description, geographic_area, schedule_pattern,
			       bin_count, estimated_duration_hours, created_by_user_id,
			       created_at, updated_at
			FROM routes
			ORDER BY created_at DESC
		`)
		if err != nil {
			http.Error(w, "Failed to fetch routes", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(routes)
	}
}

// GetRoute returns a single route with its bins
func GetRoute(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		routeID := chi.URLParam(r, "id")

		// Get route
		var route models.Route
		err := db.Get(&route, `
			SELECT id, name, description, geographic_area, schedule_pattern,
			       bin_count, estimated_duration_hours, created_by_user_id,
			       created_at, updated_at
			FROM routes
			WHERE id = $1
		`, routeID)
		if err == sql.ErrNoRows {
			http.Error(w, "Route not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "Failed to fetch route", http.StatusInternalServerError)
			return
		}

		// Get route bins with details
		type BinWithSequence struct {
			models.Bin
			SequenceOrder int `db:"sequence_order"`
		}

		var binsWithSequence []BinWithSequence
		err = db.Select(&binsWithSequence, `
			SELECT b.id, b.bin_number, b.current_street, b.city, b.zip,
			       b.last_moved, b.last_checked, b.status, COALESCE(b.fill_percentage, 0) as fill_percentage,
			       b.checked, b.move_requested, b.latitude, b.longitude,
			       b.created_at, b.updated_at, rb.sequence_order
			FROM bins b
			INNER JOIN route_bins rb ON b.id = rb.bin_id
			WHERE rb.route_id = $1
			ORDER BY rb.sequence_order ASC
		`, routeID)
		if err != nil {
			http.Error(w, "Failed to fetch route bins", http.StatusInternalServerError)
			return
		}

		// Convert to response format
		bins := make([]models.BinInRoute, len(binsWithSequence))
		for i, bws := range binsWithSequence {
			bins[i] = models.BinInRoute{
				BinResponse:   bws.Bin.ToBinResponse(),
				SequenceOrder: bws.SequenceOrder,
			}
		}

		response := models.RouteWithBins{
			Route: route,
			Bins:  bins,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// CreateRoute creates a new route blueprint
func CreateRoute(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req models.CreateRouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate required fields
		if req.Name == "" || req.GeographicArea == "" || len(req.BinIDs) == 0 {
			http.Error(w, "Missing required fields: name, geographic_area, and bin_ids", http.StatusBadRequest)
			return
		}

		// Get user ID from context (set by auth middleware)
		userID, _ := r.Context().Value("user_id").(string)

		// Generate UUID and timestamp
		id := uuid.New().String()
		now := time.Now().Unix()

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			http.Error(w, "Failed to start transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		// Prepare optional fields
		var description *string
		if req.Description != "" {
			description = &req.Description
		}

		var schedulePattern *string
		if req.SchedulePattern != "" {
			schedulePattern = &req.SchedulePattern
		}

		var createdBy *string
		if userID != "" {
			createdBy = &userID
		}

		// Insert route
		_, err = tx.Exec(`
			INSERT INTO routes (
				id, name, description, geographic_area, schedule_pattern,
				bin_count, estimated_duration_hours, created_by_user_id,
				created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`,
			id, req.Name, description, req.GeographicArea, schedulePattern,
			len(req.BinIDs), req.EstimatedDurationHours, createdBy, now, now,
		)
		if err != nil {
			http.Error(w, "Failed to create route", http.StatusInternalServerError)
			return
		}

		// Insert route_bins
		for i, binID := range req.BinIDs {
			_, err = tx.Exec(`
				INSERT INTO route_bins (route_id, bin_id, sequence_order, created_at)
				VALUES ($1, $2, $3, $4)
			`, id, binID, i+1, now)
			if err != nil {
				http.Error(w, "Failed to add bins to route", http.StatusInternalServerError)
				return
			}
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
			return
		}

		// Fetch created route
		var created models.Route
		err = db.Get(&created, `
			SELECT id, name, description, geographic_area, schedule_pattern,
			       bin_count, estimated_duration_hours, created_by_user_id,
			       created_at, updated_at
			FROM routes
			WHERE id = $1
		`, id)
		if err != nil {
			http.Error(w, "Failed to fetch created route", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(created)
	}
}

// UpdateRoute updates an existing route
func UpdateRoute(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		routeID := chi.URLParam(r, "id")

		var req models.UpdateRouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		now := time.Now().Unix()

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			http.Error(w, "Failed to start transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		// Build dynamic update query
		updates := []string{}
		args := []interface{}{}
		argCount := 1

		if req.Name != nil {
			updates = append(updates, "name = $"+string(rune('0'+argCount)))
			args = append(args, *req.Name)
			argCount++
		}
		if req.Description != nil {
			updates = append(updates, "description = $"+string(rune('0'+argCount)))
			args = append(args, *req.Description)
			argCount++
		}
		if req.GeographicArea != nil {
			updates = append(updates, "geographic_area = $"+string(rune('0'+argCount)))
			args = append(args, *req.GeographicArea)
			argCount++
		}
		if req.SchedulePattern != nil {
			updates = append(updates, "schedule_pattern = $"+string(rune('0'+argCount)))
			args = append(args, *req.SchedulePattern)
			argCount++
		}
		if req.EstimatedDurationHours != nil {
			updates = append(updates, "estimated_duration_hours = $"+string(rune('0'+argCount)))
			args = append(args, *req.EstimatedDurationHours)
			argCount++
		}

		// Update bin_ids if provided
		if req.BinIDs != nil {
			updates = append(updates, "bin_count = $"+string(rune('0'+argCount)))
			args = append(args, len(req.BinIDs))
			argCount++

			// Delete existing bin associations
			_, err = tx.Exec("DELETE FROM route_bins WHERE route_id = $1", routeID)
			if err != nil {
				http.Error(w, "Failed to update route bins", http.StatusInternalServerError)
				return
			}

			// Insert new bin associations
			for i, binID := range req.BinIDs {
				_, err = tx.Exec(`
					INSERT INTO route_bins (route_id, bin_id, sequence_order, created_at)
					VALUES ($1, $2, $3, $4)
				`, routeID, binID, i+1, now)
				if err != nil {
					http.Error(w, "Failed to add bins to route", http.StatusInternalServerError)
					return
				}
			}
		}

		// Always update updated_at
		updates = append(updates, "updated_at = $"+string(rune('0'+argCount)))
		args = append(args, now)
		argCount++

		// Add route ID as final parameter
		args = append(args, routeID)

		// Execute update if there are changes
		if len(updates) > 1 { // More than just updated_at
			query := "UPDATE routes SET " + joinStrings(updates, ", ") + " WHERE id = $" + string(rune('0'+argCount))
			_, err = tx.Exec(query, args...)
			if err != nil {
				http.Error(w, "Failed to update route", http.StatusInternalServerError)
				return
			}
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
			return
		}

		// Fetch updated route
		var updated models.Route
		err = db.Get(&updated, `
			SELECT id, name, description, geographic_area, schedule_pattern,
			       bin_count, estimated_duration_hours, created_by_user_id,
			       created_at, updated_at
			FROM routes
			WHERE id = $1
		`, routeID)
		if err == sql.ErrNoRows {
			http.Error(w, "Route not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "Failed to fetch updated route", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(updated)
	}
}

// DeleteRoute deletes a route blueprint
func DeleteRoute(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		routeID := chi.URLParam(r, "id")

		result, err := db.Exec("DELETE FROM routes WHERE id = $1", routeID)
		if err != nil {
			http.Error(w, "Failed to delete route", http.StatusInternalServerError)
			return
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			http.Error(w, "Route not found", http.StatusNotFound)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// DuplicateRoute creates a copy of an existing route
func DuplicateRoute(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sourceRouteID := chi.URLParam(r, "id")

		var req models.DuplicateRouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if req.Name == "" {
			http.Error(w, "Name is required", http.StatusBadRequest)
			return
		}

		// Get source route
		var sourceRoute models.Route
		err := db.Get(&sourceRoute, `
			SELECT id, name, description, geographic_area, schedule_pattern,
			       bin_count, estimated_duration_hours, created_by_user_id,
			       created_at, updated_at
			FROM routes
			WHERE id = $1
		`, sourceRouteID)
		if err == sql.ErrNoRows {
			http.Error(w, "Source route not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "Failed to fetch source route", http.StatusInternalServerError)
			return
		}

		// Get source route bins
		var sourceBins []models.RouteBin
		err = db.Select(&sourceBins, `
			SELECT id, route_id, bin_id, sequence_order, created_at
			FROM route_bins
			WHERE route_id = $1
			ORDER BY sequence_order ASC
		`, sourceRouteID)
		if err != nil {
			http.Error(w, "Failed to fetch source route bins", http.StatusInternalServerError)
			return
		}

		// Get user ID from context
		userID, _ := r.Context().Value("user_id").(string)

		// Generate new UUID and timestamp
		newID := uuid.New().String()
		now := time.Now().Unix()

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			http.Error(w, "Failed to start transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		var createdBy *string
		if userID != "" {
			createdBy = &userID
		}

		// Create new route (duplicate)
		_, err = tx.Exec(`
			INSERT INTO routes (
				id, name, description, geographic_area, schedule_pattern,
				bin_count, estimated_duration_hours, created_by_user_id,
				created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`,
			newID, req.Name, sourceRoute.Description, sourceRoute.GeographicArea,
			sourceRoute.SchedulePattern, sourceRoute.BinCount,
			sourceRoute.EstimatedDurationHours, createdBy, now, now,
		)
		if err != nil {
			http.Error(w, "Failed to create duplicate route", http.StatusInternalServerError)
			return
		}

		// Copy route bins
		for _, bin := range sourceBins {
			_, err = tx.Exec(`
				INSERT INTO route_bins (route_id, bin_id, sequence_order, created_at)
				VALUES ($1, $2, $3, $4)
			`, newID, bin.BinID, bin.SequenceOrder, now)
			if err != nil {
				http.Error(w, "Failed to copy route bins", http.StatusInternalServerError)
				return
			}
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
			return
		}

		// Fetch created route
		var created models.Route
		err = db.Get(&created, `
			SELECT id, name, description, geographic_area, schedule_pattern,
			       bin_count, estimated_duration_hours, created_by_user_id,
			       created_at, updated_at
			FROM routes
			WHERE id = $1
		`, newID)
		if err != nil {
			http.Error(w, "Failed to fetch duplicated route", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(created)
	}
}

// OptimizeRoutePreview returns an optimized route order using Mapbox Optimization API
func OptimizeRoutePreview(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			BinIDs        []string `json:"bin_ids"`
			StartLocation *struct {
				Latitude  float64 `json:"latitude"`
				Longitude float64 `json:"longitude"`
			} `json:"start_location"` // Optional
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if len(req.BinIDs) == 0 {
			http.Error(w, "bin_ids cannot be empty", http.StatusBadRequest)
			return
		}

		// Mapbox Optimization API has a 12 waypoint limit on free tier
		if len(req.BinIDs) > 12 {
			http.Error(w, "Cannot optimize more than 12 bins (Mapbox API limit)", http.StatusBadRequest)
			return
		}

		log.Printf("🎯 Optimizing route preview for %d bins using Mapbox API", len(req.BinIDs))

		// Fetch bins from database
		query := `
			SELECT id, bin_number, current_street, latitude, longitude, fill_percentage
			FROM bins
			WHERE id = ANY($1)
			AND latitude IS NOT NULL
			AND longitude IS NOT NULL
		`
		var bins []models.Bin
		if err := db.Select(&bins, query, pq.Array(req.BinIDs)); err != nil {
			log.Printf("❌ Error fetching bins: %v", err)
			http.Error(w, "Failed to fetch bins", http.StatusInternalServerError)
			return
		}

		if len(bins) == 0 {
			http.Error(w, "No valid bins found", http.StatusNotFound)
			return
		}

		// Determine start location (warehouse)
		warehouseLoc := services.GetWarehouseLocation()
		startLocation := services.OptimizerLocation{
			Latitude:  warehouseLoc.Latitude,
			Longitude: warehouseLoc.Longitude,
		}

		if req.StartLocation != nil {
			startLocation = services.OptimizerLocation{
				Latitude:  req.StartLocation.Latitude,
				Longitude: req.StartLocation.Longitude,
			}
		}

		log.Printf("📍 Start location: (%.6f, %.6f)", startLocation.Latitude, startLocation.Longitude)

		// Build Mapbox Optimization API URL
		// Format: warehouse;bin1;bin2;...;warehouse (explicit round trip)
		coordinates := fmt.Sprintf("%.6f,%.6f", startLocation.Longitude, startLocation.Latitude)
		binIndexMap := make(map[int]string) // Map Mapbox waypoint index to bin ID

		for i, bin := range bins {
			coordinates += fmt.Sprintf(";%.6f,%.6f", *bin.Longitude, *bin.Latitude)
			binIndexMap[i+1] = bin.ID // +1 because warehouse is at index 0
		}

		// Add warehouse at the end for explicit round trip
		coordinates += fmt.Sprintf(";%.6f,%.6f", startLocation.Longitude, startLocation.Latitude)

		// Mapbox Optimization API endpoint
		// source=first: start at warehouse (first coordinate)
		// destination=last: end at warehouse (last coordinate)
		// roundtrip=false: we explicitly added warehouse at both ends
		mapboxToken := "pk.eyJ1IjoiYmlubHl5YWkiLCJhIjoiY21pNzN4bzlhMDVheTJpcHdqd2FtYjhpeSJ9.sQM8WHE2C9zWH0xG107xhw"
		mapboxURL := fmt.Sprintf(
			"https://api.mapbox.com/optimized-trips/v1/mapbox/driving/%s?source=first&destination=last&roundtrip=false&overview=full&geometries=geojson&access_token=%s",
			coordinates,
			mapboxToken,
		)

		log.Printf("🌐 Calling Mapbox Optimization API...")

		// Make request to Mapbox
		resp, err := http.Get(mapboxURL)
		if err != nil {
			log.Printf("❌ Mapbox API error: %v", err)
			http.Error(w, "Failed to call Mapbox API", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			log.Printf("❌ Mapbox API returned status %d: %s", resp.StatusCode, string(body))
			http.Error(w, "Mapbox API request failed", http.StatusInternalServerError)
			return
		}

		// Parse Mapbox response - first read the raw body for debugging
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("❌ Failed to read Mapbox response body: %v", err)
			http.Error(w, "Failed to read Mapbox response", http.StatusInternalServerError)
			return
		}
		log.Printf("📡 Raw Mapbox Response: %s", string(bodyBytes))

		var mapboxResponse struct {
			Code      string `json:"code"`
			Waypoints []struct {
				WaypointIndex int `json:"waypoint_index"`
				TripsIndex    int `json:"trips_index"`
			} `json:"waypoints"` // At root level!
			Trips []struct {
				Distance float64 `json:"distance"` // meters
				Duration float64 `json:"duration"` // seconds
			} `json:"trips"`
		}

		if err := json.Unmarshal(bodyBytes, &mapboxResponse); err != nil {
			log.Printf("❌ Failed to parse Mapbox response: %v", err)
			http.Error(w, "Failed to parse Mapbox response", http.StatusInternalServerError)
			return
		}

		if mapboxResponse.Code != "Ok" || len(mapboxResponse.Trips) == 0 {
			log.Printf("❌ Mapbox API returned code: %s", mapboxResponse.Code)
			http.Error(w, "Mapbox optimization failed", http.StatusInternalServerError)
			return
		}

		trip := mapboxResponse.Trips[0]
		log.Printf("✅ Mapbox optimized route: %.2f km, %.2f minutes",
			trip.Distance/1000, trip.Duration/60)

		// Debug: Log waypoints from Mapbox
		log.Printf("📊 Mapbox returned %d waypoints:", len(mapboxResponse.Waypoints))
		for i, wp := range mapboxResponse.Waypoints {
			log.Printf("   Waypoint %d: WaypointIndex=%d, TripsIndex=%d", i, wp.WaypointIndex, wp.TripsIndex)
		}
		log.Printf("📊 BinIndexMap: %+v", binIndexMap)

		// Extract optimized bin order from waypoints
		// Skip first and last waypoints (warehouse start/end)
		optimizedBinIDs := make([]string, 0, len(bins))
		for i, waypoint := range mapboxResponse.Waypoints {
			log.Printf("   Processing waypoint %d: index=%d", i, waypoint.WaypointIndex)

			// Skip warehouse (index 0) - it appears at start and potentially end
			if waypoint.WaypointIndex == 0 {
				log.Printf("   → Skipping warehouse waypoint")
				continue
			}

			if binID, exists := binIndexMap[waypoint.WaypointIndex]; exists {
				log.Printf("   → Found bin ID: %s", binID)
				optimizedBinIDs = append(optimizedBinIDs, binID)
			} else {
				log.Printf("   → ⚠️ No bin found for waypoint index %d", waypoint.WaypointIndex)
			}
		}

		log.Printf("🔄 Optimized bin order: %v", optimizedBinIDs)

		// Create map for quick lookup of bin details
		binMap := make(map[string]models.Bin)
		for _, bin := range bins {
			binMap[bin.ID] = bin
		}

		// Build response with bin details in optimized order
		type BinInSequence struct {
			ID             string  `json:"id"`
			BinNumber      int     `json:"bin_number"`
			CurrentStreet  string  `json:"current_street"`
			Latitude       float64 `json:"latitude"`
			Longitude      float64 `json:"longitude"`
			FillPercentage int     `json:"fill_percentage"`
			SequenceOrder  int     `json:"sequence_order"`
		}

		binsInSequence := make([]BinInSequence, len(optimizedBinIDs))
		for i, binID := range optimizedBinIDs {
			bin := binMap[binID]
			binsInSequence[i] = BinInSequence{
				ID:             bin.ID,
				BinNumber:      bin.BinNumber,
				CurrentStreet:  bin.CurrentStreet,
				Latitude:       *bin.Latitude,
				Longitude:      *bin.Longitude,
				FillPercentage: *bin.FillPercentage,
				SequenceOrder:  i + 1,
			}
		}

		// Use Mapbox's distance and duration (convert to km and hours)
		totalDistanceKm := trip.Distance / 1000.0
		durationHours := trip.Duration / 3600.0

		// Add collection time (5 minutes per bin)
		minutesPerBin := 5.0
		collectionTimeHours := (float64(len(bins)) * minutesPerBin) / 60.0
		totalDurationHours := durationHours + collectionTimeHours

		response := struct {
			OptimizedBinIDs      []string        `json:"optimized_bin_ids"`
			TotalDistanceKm      float64         `json:"total_distance_km"`
			EstimatedDurationHrs float64         `json:"estimated_duration_hours"`
			Bins                 []BinInSequence `json:"bins"`
		}{
			OptimizedBinIDs:      optimizedBinIDs,
			TotalDistanceKm:      totalDistanceKm,
			EstimatedDurationHrs: totalDurationHours,
			Bins:                 binsInSequence,
		}

		log.Printf("✅ Route optimized: %.2f km, %.2f hours (including %.0f min collection time)",
			response.TotalDistanceKm, response.EstimatedDurationHrs, float64(len(bins))*minutesPerBin)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// haversineDistance calculates the distance between two GPS coordinates in kilometers
func haversineDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371.0 // Earth's radius in kilometers

	// Convert to radians
	lat1Rad := lat1 * math.Pi / 180
	lat2Rad := lat2 * math.Pi / 180
	deltaLat := (lat2 - lat1) * math.Pi / 180
	deltaLon := (lon2 - lon1) * math.Pi / 180

	// Haversine formula
	a := math.Sin(deltaLat/2)*math.Sin(deltaLat/2) +
		math.Cos(lat1Rad)*math.Cos(lat2Rad)*
			math.Sin(deltaLon/2)*math.Sin(deltaLon/2)

	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return earthRadius * c
}

// Helper function to join strings
func joinStrings(strs []string, sep string) string {
	if len(strs) == 0 {
		return ""
	}
	result := strs[0]
	for _, s := range strs[1:] {
		result += sep + s
	}
	return result
}

// TestHereOptimization - Test endpoint for HERE Waypoints Sequence API with raw coordinates
// This endpoint doesn't require database bins - just send coordinates directly
func TestHereOptimization(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Locations []struct {
				Name      string  `json:"name"`
				Latitude  float64 `json:"latitude"`
				Longitude float64 `json:"longitude"`
			} `json:"locations"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if len(req.Locations) == 0 {
			http.Error(w, "locations cannot be empty", http.StatusBadRequest)
			return
		}

		// HERE Waypoints Sequence API supports up to 202 waypoints
		if len(req.Locations) > 202 {
			http.Error(w, "Cannot optimize more than 202 locations (HERE API limit)", http.StatusBadRequest)
			return
		}

		// Get optional query parameters for testing traffic features
		enableTraffic := r.URL.Query().Get("enableTraffic") == "true"
		departureTime := r.URL.Query().Get("departureTime") // ISO 8601 format

		// If traffic is enabled but no departure time specified, use current time
		if enableTraffic && departureTime == "" {
			departureTime = time.Now().Format(time.RFC3339)
		}

		log.Printf("🧪 Testing HERE route optimization for %d locations", len(req.Locations))
		if enableTraffic {
			log.Printf("🚦 Traffic: ENABLED")
			if departureTime != "" {
				log.Printf("⏰ Departure Time: %s", departureTime)
			} else {
				log.Printf("⏰ Departure Time: NOW (current traffic)")
			}
		} else {
			log.Printf("🚦 Traffic: DISABLED (theoretical optimal)")
		}

		// Get warehouse location
		warehouseLoc := services.GetWarehouseLocation()

		// Build HERE Waypoints Sequence API v8 request
		// Format: start=warehouse;lat,lng&destination1=name;lat,lng&end=warehouse;lat,lng
		apiURL := "https://wps.hereapi.com/v8/findsequence2"

		// Build query parameters
		params := url.Values{}
		params.Add("apiKey", HereAPIKey)

		// Configure traffic mode
		trafficMode := "traffic:disabled"
		if enableTraffic {
			trafficMode = "traffic:enabled"
		}
		params.Add("mode", fmt.Sprintf("fastest;car;%s", trafficMode))
		params.Add("improveFor", "time")

		// Add departure time if provided (for historical traffic patterns)
		if departureTime != "" && enableTraffic {
			params.Add("departure", departureTime) // HERE API uses "departure" not "departureTime"
			log.Printf("📅 Adding departure to HERE API: %s", departureTime)
		}
		params.Add("start", fmt.Sprintf("start-warehouse;%.6f,%.6f", warehouseLoc.Latitude, warehouseLoc.Longitude))
		params.Add("end", fmt.Sprintf("end-warehouse;%.6f,%.6f", warehouseLoc.Latitude, warehouseLoc.Longitude))
		log.Printf("📍 Warehouse: lat=%.6f, lng=%.6f", warehouseLoc.Latitude, warehouseLoc.Longitude)

		// Add all locations as destinations
		for i, loc := range req.Locations {
			destKey := fmt.Sprintf("destination%d", i+1)
			name := loc.Name
			if name == "" {
				name = fmt.Sprintf("location%d", i+1)
			}
			params.Add(destKey, fmt.Sprintf("%s;%.6f,%.6f", name, loc.Latitude, loc.Longitude))
			log.Printf("📍 Destination %d (%s): lat=%.6f, lng=%.6f", i+1, name, loc.Latitude, loc.Longitude)
		}

		// Make request to HERE API
		fullURL := fmt.Sprintf("%s?%s", apiURL, params.Encode())
		log.Printf("🌐 Calling HERE Waypoints Sequence API v8...")

		if enableTraffic {
			log.Printf("🔗 URL (traffic enabled): %s?mode=%s&improveFor=%s&departure=%s&start=%s&end=%s&...",
				apiURL,
				params.Get("mode"),
				params.Get("improveFor"),
				params.Get("departure"),
				params.Get("start"),
				params.Get("end"))
		} else {
			log.Printf("🔗 URL (no traffic): %s?mode=%s&improveFor=%s&start=%s&end=%s&...",
				apiURL,
				params.Get("mode"),
				params.Get("improveFor"),
				params.Get("start"),
				params.Get("end"))
		}

		resp, err := http.Get(fullURL)
		if err != nil {
			log.Printf("❌ HERE API error: %v", err)
			http.Error(w, "Failed to call HERE API", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			log.Printf("❌ HERE API returned status %d: %s", resp.StatusCode, string(body))
			http.Error(w, "HERE API request failed", http.StatusInternalServerError)
			return
		}

		// Parse HERE v8 response (separate struct from Mapbox)
		var hereResp struct {
			Results []struct {
				Waypoints []struct {
					ID                string  `json:"id"`
					Lat               float64 `json:"lat"`
					Lng               float64 `json:"lng"`
					Sequence          int     `json:"sequence"`
					EstimatedArrival  string  `json:"estimatedArrival,omitempty"`
					EstimatedDeparture string `json:"estimatedDeparture,omitempty"`
				} `json:"waypoints"`
				Distance      string `json:"distance"` // meters (string in v8)
				Time          string `json:"time"`     // seconds (string in v8)
				Interconnections []struct {
					FromWaypoint string  `json:"fromWaypoint"`
					ToWaypoint   string  `json:"toWaypoint"`
					Distance     float64 `json:"distance"` // meters (can be float in HERE)
					Time         float64 `json:"time"`     // seconds (can be float in HERE)
				} `json:"interconnections"`
			} `json:"results"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&hereResp); err != nil {
			log.Printf("❌ Failed to parse HERE response: %v", err)
			http.Error(w, "Failed to parse HERE response", http.StatusInternalServerError)
			return
		}

		if len(hereResp.Results) == 0 {
			log.Printf("❌ HERE optimization failed: no results")
			http.Error(w, "Route optimization failed", http.StatusInternalServerError)
			return
		}

		result := hereResp.Results[0]

		// Extract optimized order (excluding start and end which are warehouse)
		type OptimizedStop struct {
			Index     int     `json:"index"`
			Name      string  `json:"name"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Sequence  int     `json:"sequence"`
		}

		// Create a map of location names to indices
		nameToIndex := make(map[string]int)
		for i, loc := range req.Locations {
			name := loc.Name
			if name == "" {
				name = fmt.Sprintf("location%d", i+1)
			}
			nameToIndex[name] = i
		}

		optimizedStops := []OptimizedStop{}
		for _, waypoint := range result.Waypoints {
			// Skip start and end (warehouse)
			if waypoint.ID == "start-warehouse" || waypoint.ID == "end-warehouse" {
				continue
			}

			// Find the index using the waypoint ID (which is the location name)
			if idx, exists := nameToIndex[waypoint.ID]; exists {
				originalLoc := req.Locations[idx]
				name := originalLoc.Name
				if name == "" {
					name = fmt.Sprintf("Location %d", idx+1)
				}

				optimizedStops = append(optimizedStops, OptimizedStop{
					Index:     idx,
					Name:      name,
					Latitude:  originalLoc.Latitude,
					Longitude: originalLoc.Longitude,
					Sequence:  waypoint.Sequence,
				})
			}
		}

		// Calculate total distance and duration (v8 returns strings)
		var totalDistanceMeters, totalDurationSeconds float64
		fmt.Sscanf(result.Distance, "%f", &totalDistanceMeters)
		fmt.Sscanf(result.Time, "%f", &totalDurationSeconds)

		log.Printf("📊 HERE API Top-Level Fields:")
		log.Printf("   Distance (result.Distance): %s meters = %.2f km", result.Distance, totalDistanceMeters/1000.0)
		log.Printf("   Time (result.Time): %s seconds = %.2f minutes", result.Time, totalDurationSeconds/60.0)

		// Calculate sum of interconnections (actual route segments)
		var interconnectionDistanceSum float64
		var interconnectionTimeSum float64
		log.Printf("🔗 HERE API Interconnections (route segments):")
		for i, conn := range result.Interconnections {
			log.Printf("   Segment %d: %s → %s | Distance: %.0f m | Time: %.0f sec",
				i+1, conn.FromWaypoint, conn.ToWaypoint, conn.Distance, conn.Time)
			interconnectionDistanceSum += conn.Distance
			interconnectionTimeSum += conn.Time
		}
		log.Printf("📐 SUM of Interconnections:")
		log.Printf("   Total Distance: %.2f km (%.2f miles)", interconnectionDistanceSum/1000.0, (interconnectionDistanceSum/1000.0)*0.621371)
		log.Printf("   Total Time: %.2f minutes", interconnectionTimeSum/60.0)

		totalDistanceKm := totalDistanceMeters / 1000.0
		totalDurationHours := totalDurationSeconds / 3600.0

		// Add 15 minutes per location for service time
		serviceTimeHours := float64(len(req.Locations)) * (15.0 / 60.0)
		totalDurationHours += serviceTimeHours

		// Build traffic info message
		trafficInfo := "Disabled (theoretical optimal)"
		if enableTraffic {
			if departureTime != "" {
				trafficInfo = fmt.Sprintf("Enabled (historical traffic for %s)", departureTime)
			} else {
				trafficInfo = "Enabled (current live traffic)"
			}
		}

		response := struct {
			Success            bool            `json:"success"`
			Message            string          `json:"message"`
			TotalStops         int             `json:"total_stops"`
			TotalDistanceKm    float64         `json:"total_distance_km"`
			TotalDistanceMiles float64         `json:"total_distance_miles"`
			EstimatedDuration  string          `json:"estimated_duration"`
			DurationHours      float64         `json:"duration_hours"`
			OptimizedOrder     []OptimizedStop `json:"optimized_order"`
			ServiceDuration    string          `json:"service_duration_per_stop"`
			TrafficMode        string          `json:"traffic_mode"`
			DepartureTime      string          `json:"departure_time,omitempty"`
			Provider           string          `json:"provider"`
		}{
			Success:            true,
			Message:            "Route optimization completed successfully using HERE Maps",
			TotalStops:         len(req.Locations),
			TotalDistanceKm:    totalDistanceKm,
			TotalDistanceMiles: totalDistanceKm * 0.621371,
			EstimatedDuration:  fmt.Sprintf("%.1f hours (%.0f minutes)", totalDurationHours, totalDurationHours*60),
			DurationHours:      totalDurationHours,
			OptimizedOrder:     optimizedStops,
			ServiceDuration:    "15 minutes (900 seconds)",
			TrafficMode:        trafficInfo,
			DepartureTime:      departureTime,
			Provider:           "HERE Maps Waypoints Sequence API v8",
		}

		log.Printf("✅ HERE test optimization complete: %.2f km, %.2f hours, %d stops",
			response.TotalDistanceKm, response.DurationHours, len(optimizedStops))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// TestMapboxOptimization - Test endpoint for Mapbox Optimization API v1
func TestMapboxOptimization(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Same request structure as HERE test endpoint
		var req struct {
			Locations []struct {
				Name      string  `json:"name"`
				Latitude  float64 `json:"latitude"`
				Longitude float64 `json:"longitude"`
			} `json:"locations"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if len(req.Locations) == 0 {
			http.Error(w, "locations cannot be empty", http.StatusBadRequest)
			return
		}

		// Mapbox Optimization v1 supports up to 12 waypoints (including start/end)
		// Since we add warehouse at both ends, max locations = 12 - 2 = 10
		if len(req.Locations) > 10 {
			http.Error(w, "Cannot optimize more than 10 locations (Mapbox v1 limit: 12 waypoints including warehouse at start/end)", http.StatusBadRequest)
			return
		}

		log.Printf("🧪 Testing Mapbox route optimization for %d locations", len(req.Locations))

		// Get warehouse location
		warehouseLoc := services.GetWarehouseLocation()
		log.Printf("📍 Warehouse: lat=%.6f, lng=%.6f", warehouseLoc.Latitude, warehouseLoc.Longitude)

		// Build Mapbox Optimization API URL
		// Format: warehouse;location1;location2;...;warehouse
		coordinates := fmt.Sprintf("%.6f,%.6f", warehouseLoc.Longitude, warehouseLoc.Latitude)

		for i, loc := range req.Locations {
			coordinates += fmt.Sprintf(";%.6f,%.6f", loc.Longitude, loc.Latitude)
			log.Printf("📍 Destination %d (%s): lat=%.6f, lng=%.6f", i+1, loc.Name, loc.Latitude, loc.Longitude)
		}

		// Add warehouse at the end for explicit round trip
		coordinates += fmt.Sprintf(";%.6f,%.6f", warehouseLoc.Longitude, warehouseLoc.Latitude)

		mapboxToken := "pk.eyJ1IjoiYmlubHl5YWkiLCJhIjoiY21pNzN4bzlhMDVheTJpcHdqd2FtYjhpeSJ9.sQM8WHE2C9zWH0xG107xhw"
		mapboxURL := fmt.Sprintf(
			"https://api.mapbox.com/optimized-trips/v1/mapbox/driving/%s?source=first&destination=last&roundtrip=false&overview=full&geometries=geojson&access_token=%s",
			coordinates,
			mapboxToken,
		)

		log.Printf("🌐 Calling Mapbox Optimization API v1...")
		log.Printf("🔗 Coordinates: %s", coordinates)

		// Make request to Mapbox
		resp, err := http.Get(mapboxURL)
		if err != nil {
			log.Printf("❌ Mapbox API error: %v", err)
			http.Error(w, "Failed to call Mapbox API", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			log.Printf("❌ Mapbox API returned status %d: %s", resp.StatusCode, string(body))
			http.Error(w, "Mapbox API request failed", http.StatusInternalServerError)
			return
		}

		// Parse Mapbox response
		var mapboxResponse struct {
			Code      string `json:"code"`
			Waypoints []struct {
				WaypointIndex int `json:"waypoint_index"`
				TripsIndex    int `json:"trips_index"`
			} `json:"waypoints"`
			Trips []struct {
				Distance float64 `json:"distance"` // meters
				Duration float64 `json:"duration"` // seconds
			} `json:"trips"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&mapboxResponse); err != nil {
			log.Printf("❌ Failed to parse Mapbox response: %v", err)
			http.Error(w, "Failed to parse Mapbox response", http.StatusInternalServerError)
			return
		}

		if mapboxResponse.Code != "Ok" || len(mapboxResponse.Trips) == 0 {
			log.Printf("❌ Mapbox optimization failed: code=%s", mapboxResponse.Code)
			http.Error(w, "Route optimization failed", http.StatusInternalServerError)
			return
		}

		trip := mapboxResponse.Trips[0]

		// Extract optimized order (excluding warehouse at start and end)
		type OptimizedStop struct {
			Index     int     `json:"index"`
			Name      string  `json:"name"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Sequence  int     `json:"sequence"`
		}

		optimizedStops := []OptimizedStop{}
		for i, waypoint := range mapboxResponse.Waypoints {
			// Skip first (warehouse start) and last (warehouse end)
			if i == 0 || i == len(mapboxResponse.Waypoints)-1 {
				continue
			}

			// waypoint.WaypointIndex gives us the original index in coordinates string
			// Subtract 1 to get the index in req.Locations (since warehouse is at 0)
			originalIdx := waypoint.WaypointIndex - 1
			if originalIdx >= 0 && originalIdx < len(req.Locations) {
				loc := req.Locations[originalIdx]
				optimizedStops = append(optimizedStops, OptimizedStop{
					Index:     originalIdx,
					Name:      loc.Name,
					Latitude:  loc.Latitude,
					Longitude: loc.Longitude,
					Sequence:  i, // Position in optimized route
				})
			}
		}

		// Calculate totals
		totalDistanceKm := trip.Distance / 1000.0
		totalDurationHours := trip.Duration / 3600.0

		// Add 15 minutes per location for service time
		serviceTimeHours := float64(len(req.Locations)) * (15.0 / 60.0)
		totalDurationHours += serviceTimeHours

		response := struct {
			Success            bool            `json:"success"`
			Message            string          `json:"message"`
			TotalStops         int             `json:"total_stops"`
			TotalDistanceKm    float64         `json:"total_distance_km"`
			TotalDistanceMiles float64         `json:"total_distance_miles"`
			EstimatedDuration  string          `json:"estimated_duration"`
			DurationHours      float64         `json:"duration_hours"`
			OptimizedOrder     []OptimizedStop `json:"optimized_order"`
			ServiceDuration    string          `json:"service_duration_per_stop"`
			Provider           string          `json:"provider"`
		}{
			Success:            true,
			Message:            "Route optimization completed successfully using Mapbox",
			TotalStops:         len(req.Locations),
			TotalDistanceKm:    totalDistanceKm,
			TotalDistanceMiles: totalDistanceKm * 0.621371,
			EstimatedDuration:  fmt.Sprintf("%.1f hours (%.0f minutes)", totalDurationHours, totalDurationHours*60),
			DurationHours:      totalDurationHours,
			OptimizedOrder:     optimizedStops,
			ServiceDuration:    "15 minutes (900 seconds)",
			Provider:           "Mapbox Optimization API v1",
		}

		log.Printf("✅ Mapbox test optimization complete: %.2f km, %.2f hours, %d stops",
			response.TotalDistanceKm, response.DurationHours, len(optimizedStops))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// BinBatchGeocodeRequest represents the request body for batch geocoding bins
type BinBatchGeocodeRequest struct {
	AutoUpdate bool `json:"auto_update"` // If true, automatically update DB for changes <1km
}

// BatchGeocodeBins - Batch geocode all bins and compare with existing coordinates
func BatchGeocodeBins(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📍 Starting batch geocoding operation...")

		// Parse request
		var req BinBatchGeocodeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// Default to false if no body provided
			req.AutoUpdate = false
		}

		// Create geocoding service
		geocodingService := services.NewHEREGeocodingService(HereAPIKey)

		// Fetch all bins from database
		type Bin struct {
			ID            string         `db:"id"`
			BinNumber     int            `db:"bin_number"`
			CurrentStreet string         `db:"current_street"`
			City          string         `db:"city"`
			Zip           string         `db:"zip"`
			Latitude      sql.NullFloat64 `db:"latitude"`
			Longitude     sql.NullFloat64 `db:"longitude"`
		}

		var bins []Bin
		err := db.Select(&bins, `
			SELECT id, bin_number, current_street, city, zip, latitude, longitude
			FROM bins
			ORDER BY bin_number ASC
		`)
		if err != nil {
			log.Printf("❌ Failed to fetch bins: %v", err)
			http.Error(w, "Failed to fetch bins from database", http.StatusInternalServerError)
			return
		}

		log.Printf("   Found %d bins to geocode", len(bins))

		// Process each bin
		results := make([]services.GeocodeResult, 0, len(bins))
		updatedCount := 0
		flaggedCount := 0
		errorCount := 0

		for i, bin := range bins {
			log.Printf("   [%d/%d] Processing Bin #%d: %s, %s, %s",
				i+1, len(bins), bin.BinNumber, bin.CurrentStreet, bin.City, bin.Zip)

			// Get old coordinates (0 if NULL)
			oldLat := 0.0
			oldLng := 0.0
			if bin.Latitude.Valid {
				oldLat = bin.Latitude.Float64
			}
			if bin.Longitude.Valid {
				oldLng = bin.Longitude.Float64
			}

			result := services.GeocodeResult{
				BinID:        bin.ID,
				BinNumber:    bin.BinNumber,
				Address:      fmt.Sprintf("%s, %s, %s", bin.CurrentStreet, bin.City, bin.Zip),
				OldLatitude:  oldLat,
				OldLongitude: oldLng,
			}

			// Geocode the address
			newLat, newLng, err := geocodingService.GeocodeAddress(bin.CurrentStreet, bin.City, bin.Zip)
			if err != nil {
				log.Printf("      ❌ Geocoding failed: %v", err)
				result.GeocodeSuccess = false
				result.ErrorMessage = err.Error()
				errorCount++
				results = append(results, result)
				continue
			}

			result.NewLatitude = newLat
			result.NewLongitude = newLng
			result.GeocodeSuccess = true

			// Compare coordinates
			distance, needsReview := geocodingService.CompareCoordinates(
				oldLat, oldLng, newLat, newLng,
			)
			result.DistanceMoved = distance
			result.NeedsReview = needsReview

			if needsReview {
				log.Printf("      ⚠️  FLAGGED: Coordinates moved %.2f km (>1km threshold)", distance)
				flaggedCount++
			} else if oldLat != 0 && oldLng != 0 {
				log.Printf("      ✅ Minor change: %.2f km", distance)
			} else {
				log.Printf("      ✅ New coordinates set (no previous coords)")
			}

			// Auto-update if enabled (updates all bins including flagged ones)
			if req.AutoUpdate && result.GeocodeSuccess {
				_, err := db.Exec(`
					UPDATE bins
					SET latitude = $1, longitude = $2, updated_at = EXTRACT(EPOCH FROM NOW())::BIGINT
					WHERE id = $3
				`, newLat, newLng, bin.ID)

				if err != nil {
					log.Printf("      ❌ Failed to update database: %v", err)
					result.ErrorMessage = fmt.Sprintf("Update failed: %v", err)
				} else {
					updatedCount++
					if needsReview {
						log.Printf("      💾 Database updated (FLAGGED - moved %.2f km)", distance)
					} else {
						log.Printf("      💾 Database updated")
					}
				}
			}

			results = append(results, result)
		}

		log.Printf("✅ Batch geocoding complete!")
		log.Printf("   Total bins processed: %d", len(bins))
		log.Printf("   Successfully geocoded: %d", len(bins)-errorCount)
		log.Printf("   Failed geocoding: %d", errorCount)
		log.Printf("   Flagged for review (>1km): %d", flaggedCount)
		if req.AutoUpdate {
			log.Printf("   Database updated: %d bins", updatedCount)
		}

		// Build response
		response := struct {
			Success         bool                       `json:"success"`
			Message         string                     `json:"message"`
			TotalBins       int                        `json:"total_bins"`
			GeocodeSuccess  int                        `json:"geocode_success"`
			GeocodeFailed   int                        `json:"geocode_failed"`
			FlaggedForReview int                        `json:"flagged_for_review"`
			AutoUpdated     int                        `json:"auto_updated"`
			Results         []services.GeocodeResult   `json:"results"`
		}{
			Success:         true,
			Message:         "Batch geocoding completed",
			TotalBins:       len(bins),
			GeocodeSuccess:  len(bins) - errorCount,
			GeocodeFailed:   errorCount,
			FlaggedForReview: flaggedCount,
			AutoUpdated:     updatedCount,
			Results:         results,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// GoogleFleetCredentials - Constants for Google Fleet Routing API
const (
	GoogleAPIKey  = "" // Add your Google API key here
	GoogleProjectID = "" // Add your Google Cloud project ID here
)

// TestGoogleFleetOptimization - Test endpoint for Google Route Optimization API (Fleet Routing)
func TestGoogleFleetOptimization(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("🚛 Starting Google Fleet Routing optimization...")

		// Check credentials
		if GoogleAPIKey == "" || GoogleProjectID == "" {
			log.Printf("❌ Google Fleet API credentials not configured")
			http.Error(w, "Google Fleet API credentials not configured. Set GoogleAPIKey and GoogleProjectID constants.", http.StatusInternalServerError)
			return
		}

		// Parse request - same format as HERE/Mapbox test endpoints
		type LocationInput struct {
			Name      string  `json:"name"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		}

		var req struct {
			Locations       []LocationInput `json:"locations"`
			NumVehicles     int            `json:"num_vehicles"`     // Number of trucks (default: 1)
			ServiceDuration string         `json:"service_duration"` // Service time per bin (default: "900s" = 15 min)
			StartTime       string         `json:"start_time"`       // Shift start time (default: now)
			EndTime         string         `json:"end_time"`         // Shift end time (default: +8 hours)
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if len(req.Locations) == 0 {
			http.Error(w, "No locations provided", http.StatusBadRequest)
			return
		}

		// Defaults
		if req.NumVehicles == 0 {
			req.NumVehicles = 1
		}
		if req.ServiceDuration == "" {
			req.ServiceDuration = "900s" // 15 minutes per bin
		}
		if req.StartTime == "" {
			req.StartTime = time.Now().Format(time.RFC3339)
		}
		if req.EndTime == "" {
			endTime := time.Now().Add(8 * time.Hour)
			req.EndTime = endTime.Format(time.RFC3339)
		}

		log.Printf("   Bins to optimize: %d", len(req.Locations))
		log.Printf("   Number of vehicles: %d", req.NumVehicles)
		log.Printf("   Service duration per bin: %s", req.ServiceDuration)

		// Get warehouse location
		warehouse := services.GetWarehouseLocation()

		// Create Google Fleet service
		fleetService := services.NewGoogleFleetService(GoogleAPIKey, GoogleProjectID)

		// Build shipments (one per bin - delivery-only for waste collection)
		shipments := make([]services.Shipment, len(req.Locations))
		for i, loc := range req.Locations {
			shipments[i] = services.Shipment{
				Deliveries: []services.VisitRequest{
					{
						ArrivalWaypoint: services.Waypoint{
							Location: services.Location{
								LatLng: services.LatLng{
									Latitude:  loc.Latitude,
									Longitude: loc.Longitude,
								},
							},
						},
						Duration: req.ServiceDuration,
						Label:    loc.Name,
					},
				},
				Label: loc.Name,
			}
		}

		// Build vehicles (trucks)
		vehicles := make([]services.Vehicle, req.NumVehicles)
		for i := 0; i < req.NumVehicles; i++ {
			vehicles[i] = services.Vehicle{
				StartWaypoint: services.Waypoint{
					Location: services.Location{
						LatLng: services.LatLng{
							Latitude:  warehouse.Latitude,
							Longitude: warehouse.Longitude,
						},
					},
				},
				EndWaypoint: services.Waypoint{
					Location: services.Location{
						LatLng: services.LatLng{
							Latitude:  warehouse.Latitude,
							Longitude: warehouse.Longitude,
						},
					},
				},
				Label:            fmt.Sprintf("Truck %d", i+1),
				CostPerHour:      27.0,
				CostPerKilometer: 1.0,
			}
		}

		// Build optimization request
		optimizeReq := services.OptimizeToursRequest{
			Model: services.ShipmentModel{
				Shipments:       shipments,
				Vehicles:        vehicles,
				GlobalStartTime: req.StartTime,
				GlobalEndTime:   req.EndTime,
			},
			Timeout:             "30s",
			ConsiderRoadTraffic: false, // Set to true for traffic-aware routing
		}

		// Call Google Fleet API
		result, err := fleetService.OptimizeTours(optimizeReq)
		if err != nil {
			log.Printf("❌ Google Fleet optimization failed: %v", err)
			http.Error(w, fmt.Sprintf("Fleet optimization failed: %v", err), http.StatusInternalServerError)
			return
		}

		// Build response
		type OptimizedRoute struct {
			VehicleLabel       string   `json:"vehicle_label"`
			BinSequence        []string `json:"bin_sequence"`
			BinCount           int      `json:"bin_count"`
			TravelDistanceKm   float64  `json:"travel_distance_km"`
			TravelDistanceMiles float64 `json:"travel_distance_miles"`
			TravelDuration     string   `json:"travel_duration"`
			TotalDuration      string   `json:"total_duration"`
		}

		routes := make([]OptimizedRoute, len(result.Routes))
		for i, route := range result.Routes {
			binSequence := make([]string, len(route.Visits))
			for j, visit := range route.Visits {
				binSequence[j] = visit.ShipmentLabel
			}

			travelDistanceKm := route.Metrics.TravelDistanceMeters / 1000.0

			routes[i] = OptimizedRoute{
				VehicleLabel:       route.VehicleLabel,
				BinSequence:        binSequence,
				BinCount:           route.Metrics.PerformedShipmentCount,
				TravelDistanceKm:   travelDistanceKm,
				TravelDistanceMiles: travelDistanceKm * 0.621371,
				TravelDuration:     route.Metrics.TravelDuration,
				TotalDuration:      route.Metrics.TravelDuration, // Could add visit duration here
			}
		}

		response := struct {
			Success          bool             `json:"success"`
			Message          string           `json:"message"`
			Provider         string           `json:"provider"`
			TotalBins        int              `json:"total_bins"`
			VehiclesUsed     int              `json:"vehicles_used"`
			TotalCost        float64          `json:"total_cost"`
			CostBreakdown    map[string]float64 `json:"cost_breakdown"`
			Routes           []OptimizedRoute `json:"routes"`
		}{
			Success:       true,
			Message:       "Fleet routing optimization completed successfully",
			Provider:      "Google Route Optimization API (Fleet Routing)",
			TotalBins:     len(req.Locations),
			VehiclesUsed:  result.Metrics.UsedVehicleCount,
			TotalCost:     result.Metrics.TotalCost,
			CostBreakdown: result.Metrics.Costs,
			Routes:        routes,
		}

		log.Printf("✅ Google Fleet optimization complete: %d routes, %d bins distributed",
			len(routes), len(req.Locations))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}
