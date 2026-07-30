package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"ropacal-backend/internal/geo"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/orgdb"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/services/redis"
	"ropacal-backend/internal/services/roads"
	"ropacal-backend/internal/websocket"
	"ropacal-backend/pkg/utils"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// LocationUpdateRequest represents the location data sent by the driver
type LocationUpdateRequest struct {
	Latitude  float64  `json:"latitude"`
	Longitude float64  `json:"longitude"`
	Heading   *float64 `json:"heading"`
	Speed     *float64 `json:"speed"`
	Accuracy  *float64 `json:"accuracy"`
	ShiftID   *string  `json:"shift_id"`
	Timestamp int64    `json:"timestamp"`
}

// PostDriverLocation handles location updates from drivers
// Flow: Save to DB → OSRM snap → Publish to Centrifugo
func PostDriverLocation(root *sqlx.DB, centrifugoClient *centrifugo.Client, roadsClient *roads.OSRMClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		// Get authenticated user
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		// Only drivers can post location
		if userClaims.Role != "driver" {
			utils.RespondError(w, http.StatusForbidden, "Only drivers can post location")
			return
		}

		// Parse request body
		var req LocationUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ Invalid location request: %v", err)
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// Validate coordinates
		if req.Latitude < -90 || req.Latitude > 90 || req.Longitude < -180 || req.Longitude > 180 {
			log.Printf("❌ Invalid coordinates: lat=%.6f, lng=%.6f", req.Latitude, req.Longitude)
			utils.RespondError(w, http.StatusBadRequest, "Invalid coordinates")
			return
		}

		log.Printf("📍 Location update from driver %s: (%.6f, %.6f)", userClaims.UserID, req.Latitude, req.Longitude)

		// Default accuracy to 100m if not provided
		accuracyValue := 100.0
		if req.Accuracy != nil {
			accuracyValue = *req.Accuracy
		}

		// Step 1: Save ORIGINAL GPS to database (for audit/legal)
		query := `
			INSERT INTO driver_current_location (
				driver_id, latitude, longitude, heading, speed, accuracy, shift_id, timestamp, is_connected, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE, EXTRACT(EPOCH FROM NOW())::BIGINT)
			ON CONFLICT (driver_id)
			DO UPDATE SET
				latitude = EXCLUDED.latitude,
				longitude = EXCLUDED.longitude,
				heading = EXCLUDED.heading,
				speed = EXCLUDED.speed,
				accuracy = EXCLUDED.accuracy,
				shift_id = EXCLUDED.shift_id,
				timestamp = EXCLUDED.timestamp,
				is_connected = TRUE,
				updated_at = EXTRACT(EPOCH FROM NOW())::BIGINT
			RETURNING updated_at
		`

		var updatedAt int64
		err := db.QueryRow(
			query,
			userClaims.UserID,
			req.Latitude,  // Original GPS
			req.Longitude, // Original GPS
			req.Heading,
			req.Speed,
			req.Accuracy,
			req.ShiftID,
			req.Timestamp,
		).Scan(&updatedAt)

		if err != nil {
			log.Printf("❌ Error saving location to database: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to save location")
			return
		}

		log.Printf("✅ Location saved to database (original GPS for audit)")

		// Step 2: Snap to roads using OSRM (if accuracy > 15m)
		snappedLat := req.Latitude
		snappedLng := req.Longitude

		if roadsClient != nil {
			newLat, newLng, err := roadsClient.SnapToRoad(req.Latitude, req.Longitude, accuracyValue)
			if err == nil && (newLat != req.Latitude || newLng != req.Longitude) {
				snappedLat = newLat
				snappedLng = newLng
				log.Printf("🗺️  Snapped to road: (%.6f, %.6f) → (%.6f, %.6f)", req.Latitude, req.Longitude, snappedLat, snappedLng)
			}
		}

		// Step 3: Publish SNAPPED location to Centrifugo for managers
		if centrifugoClient != nil {
			// Convert pointer fields to values (use 0 if nil)
			heading := 0.0
			if req.Heading != nil {
				heading = *req.Heading
			}
			speed := 0.0
			if req.Speed != nil {
				speed = *req.Speed
			}
			accuracy := accuracyValue

			locationData := centrifugo.DriverLocation{
				Latitude:  snappedLat, // SNAPPED coordinates for display
				Longitude: snappedLng, // SNAPPED coordinates for display
				Heading:   heading,
				Speed:     speed,
				Accuracy:  accuracy,
				Timestamp: req.Timestamp,
			}

			err := centrifugoClient.PublishDriverLocation(r.Context(), userClaims.UserID, locationData)
			if err != nil {
				log.Printf("⚠️  Failed to publish to Centrifugo: %v", err)
				// Don't fail the request - location is already saved to DB
			} else {
				log.Printf("📤 Published snapped location to Centrifugo: driver:location:%s", userClaims.UserID)
			}
		}

		// Return success
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success":    true,
			"updated_at": updatedAt,
			"snapped": map[string]interface{}{
				"latitude":  snappedLat,
				"longitude": snappedLng,
			},
		})
	}
}

type driverStartLocation struct {
	Latitude  float64
	Longitude float64
	Accuracy  float64
	Source    string // "redis" (live) or "durable" (driver_current_location fallback)
}

// Sentinel errors so callers map resolution outcomes to the right HTTP status:
// errNoDriverLocation / errDriverLocationStale are client conditions (4xx);
// any other returned error is a real infrastructure fault (5xx).
var (
	errNoDriverLocation    = errors.New("no driver location available")
	errDriverLocationStale = errors.New("driver location too stale to trust")
)

// maxFallbackLocationAge bounds how old the durable driver_current_location
// fallback may be. Redis' TTL is ~10 min; we allow a short tail past it.
const maxFallbackLocationAge = int64(15 * 60)

// isNullIsland reports whether a fix is the (0,0) sentinel that mobile GPS stacks
// emit for "no fix yet" — a valid coordinate on paper, but garbage as a route
// start point (it lands in the Atlantic off west Africa).
func isNullIsland(lat, lng float64) bool {
	const eps = 1e-6
	return lat > -eps && lat < eps && lng > -eps && lng < eps
}

// resolveDriverStartLocation returns the driver's current GPS fix for optimizing
// a shift's route. Primary source is Redis (the live cache fed by the Centrifugo
// publish-proxy AND the HTTP UpdateLocation path); on a Redis miss it falls back
// to the durable driver_current_location row (also written by UpdateLocation),
// guarded on freshness. A null-island (0,0) fix is rejected as "no GPS". Both
// StartShift and PreflightCheck call this so the start-gate and the start action
// can never disagree on what "located" means. Returns errNoDriverLocation /
// errDriverLocationStale for client conditions, or a wrapped error for real
// faults (the caller maps to 4xx vs 5xx).
func resolveDriverStartLocation(ctx context.Context, db *orgdb.DB, redisClient *redis.Client, driverID string) (driverStartLocation, error) {
	var loc driverStartLocation

	// Primary: Redis (live).
	if redisClient != nil {
		if raw, rErr := redisClient.GetDriverLocation(ctx, db.OrgID(), driverID); rErr == nil {
			var rl models.DriverLocation
			if jErr := json.Unmarshal([]byte(raw), &rl); jErr != nil {
				return loc, fmt.Errorf("parse redis location: %w", jErr)
			}
			loc = driverStartLocation{Latitude: rl.Latitude, Longitude: rl.Longitude, Source: "redis"}
			if rl.Accuracy != nil {
				loc.Accuracy = *rl.Accuracy
			}
			if isNullIsland(loc.Latitude, loc.Longitude) {
				return loc, errNoDriverLocation
			}
			return loc, nil
		}
	}

	// Fallback: the durable copy, freshness-guarded.
	var fb struct {
		Latitude  float64  `db:"latitude"`
		Longitude float64  `db:"longitude"`
		Accuracy  *float64 `db:"accuracy"`
		UpdatedAt int64    `db:"updated_at"`
	}
	if err := db.GetContext(ctx, &fb, `SELECT latitude, longitude, accuracy, updated_at FROM driver_current_location WHERE driver_id = $1`, driverID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return loc, errNoDriverLocation
		}
		return loc, fmt.Errorf("read driver_current_location: %w", err)
	}
	// age<0 guards against clock skew making a stale fix look fresh.
	if age := time.Now().Unix() - fb.UpdatedAt; age < 0 || age > maxFallbackLocationAge {
		return loc, errDriverLocationStale
	}
	loc = driverStartLocation{Latitude: fb.Latitude, Longitude: fb.Longitude, Source: "durable"}
	if fb.Accuracy != nil {
		loc.Accuracy = *fb.Accuracy
	}
	if isNullIsland(loc.Latitude, loc.Longitude) {
		return loc, errNoDriverLocation
	}
	return loc, nil
}

// StartShift starts an assigned shift

func CheckShiftDriverProximity(root *sqlx.DB, redisClient *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		shiftID := chi.URLParam(r, "id")
		if shiftID == "" {
			http.Error(w, "Shift ID is required", http.StatusBadRequest)
			return
		}

		log.Printf("🔍 [PROXIMITY CHECK] Checking driver proximity for shift: %s", shiftID)

		// Get shift details
		var shift struct {
			ID       string `db:"id"`
			DriverID string `db:"driver_id"`
			Status   string `db:"status"`
		}
		err := db.Get(&shift, `SELECT id, driver_id, status FROM shifts WHERE id = $1`, shiftID)
		if err == sql.ErrNoRows {
			http.Error(w, "Shift not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("❌ Error fetching shift: %v", err)
			http.Error(w, "Failed to fetch shift", http.StatusInternalServerError)
			return
		}

		// Only check proximity for active shifts
		if shift.Status != "active" {
			log.Printf("⚠️  [PROXIMITY CHECK] Shift status is '%s', not active - skipping proximity check", shift.Status)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"is_nearby": false,
			})
			return
		}

		// Get driver's current location from Redis
		if redisClient == nil {
			log.Printf("❌ [PROXIMITY CHECK] Redis client not available")
			http.Error(w, "Location service unavailable", http.StatusInternalServerError)
			return
		}

		ctx := context.Background()
		locationJSON, err := redisClient.GetDriverLocation(ctx, db.OrgID(), shift.DriverID)
		if err != nil {
			log.Printf("⚠️  [PROXIMITY CHECK] No location for driver %s: %v", shift.DriverID, err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"is_nearby": false,
			})
			return
		}

		// Parse location
		var driverLoc struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Timestamp int64   `json:"timestamp"`
		}
		if err := json.Unmarshal([]byte(locationJSON), &driverLoc); err != nil {
			log.Printf("❌ [PROXIMITY CHECK] Failed to parse location for driver %s: %v", shift.DriverID, err)
			http.Error(w, "Failed to parse location", http.StatusInternalServerError)
			return
		}

		// Get current task (first uncompleted task for this shift)
		var currentTask struct {
			ID        string  `db:"id"`
			Address   *string `db:"address"`
			BinNumber *int    `db:"bin_number"`
			Latitude  float64 `db:"latitude"`
			Longitude float64 `db:"longitude"`
		}
		err = db.Get(&currentTask, `
			SELECT id, address, bin_number, latitude, longitude
			FROM route_tasks
			WHERE shift_id = $1 AND is_completed = 0
			ORDER BY sequence_order ASC
			LIMIT 1
		`, shiftID)

		if err == sql.ErrNoRows {
			log.Printf("⚠️  [PROXIMITY CHECK] No current task for shift %s", shiftID)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"is_nearby": false,
			})
			return
		}
		if err != nil {
			log.Printf("❌ [PROXIMITY CHECK] Error fetching current task: %v", err)
			http.Error(w, "Failed to fetch current task", http.StatusInternalServerError)
			return
		}

		// Get driver name
		var driverName string
		err = db.Get(&driverName, `SELECT name FROM users WHERE id = $1`, shift.DriverID)
		if err != nil {
			driverName = "Unknown Driver"
		}

		// Calculate distance using haversine formula
		distanceKm := geo.HaversineKm(
			driverLoc.Latitude, driverLoc.Longitude,
			currentTask.Latitude, currentTask.Longitude,
		)
		distanceMiles := distanceKm * 0.621371 // Convert to miles

		// Calculate location age
		now := time.Now().Unix()
		locationAge := now - (driverLoc.Timestamp / 1000) // Convert ms to seconds

		// Determine if driver is nearby (within 5 miles and location is fresh)
		isNearby := distanceMiles <= 5.0 && locationAge < 300 // 5 minutes

		log.Printf("📍 [PROXIMITY CHECK] Driver %s: %.2f miles from current task (nearby=%v, age=%ds)",
			driverName, distanceMiles, isNearby, locationAge)

		// Build response
		response := map[string]interface{}{
			"is_nearby":               isNearby,
			"driver_distance_miles":   distanceMiles,
			"location_age_seconds":    locationAge,
			"current_task_id":         currentTask.ID,
			"current_task_address":    currentTask.Address,
			"current_task_bin_number": currentTask.BinNumber,
			"driver_name":             driverName,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// calculateLogicalBinCounts groups move request pickup+dropoff as one logical bin
// Returns (logicalTotal, logicalCompleted)

func RegisterFCMToken(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		// Parse request body
		var req struct {
			Token      string `json:"token"`
			DeviceType string `json:"device_type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// Validate device type
		if req.DeviceType != "ios" && req.DeviceType != "android" {
			utils.RespondError(w, http.StatusBadRequest, "Invalid device_type (must be 'ios' or 'android')")
			return
		}

		// Insert or update token
		now := time.Now().Unix()
		query := `INSERT INTO fcm_tokens (user_id, token, device_type, created_at, updated_at)
				  VALUES ($1, $2, $3, $4, $5)
				  ON CONFLICT(token) DO UPDATE SET
					  user_id = excluded.user_id,
					  device_type = excluded.device_type,
					  updated_at = excluded.updated_at`

		_, err := db.Exec(query, userClaims.UserID, req.Token, req.DeviceType, now, now)
		if err != nil {
			log.Printf("❌ Error registering FCM token: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to register FCM token")
			return
		}

		// Clean up stale tokens for this user (keep only the current one)
		result, cleanupErr := db.Exec(
			`DELETE FROM fcm_tokens WHERE user_id = $1 AND token != $2`,
			userClaims.UserID, req.Token,
		)
		if cleanupErr != nil {
			log.Printf("⚠️  Failed to clean up old FCM tokens: %v", cleanupErr)
		} else if rows, _ := result.RowsAffected(); rows > 0 {
			log.Printf("🧹 Cleaned up %d stale FCM token(s) for %s", rows, userClaims.Email)
		}

		log.Printf("📱 FCM token registered: %s (%s)", userClaims.Email, req.DeviceType)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": "FCM token registered successfully",
		})
	}
}

// ClearAllShifts deletes all shifts from the database (for testing purposes)

func UpdateLocation(root *sqlx.DB, hub *websocket.Hub, redisClient *redis.Client, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		// Only drivers can post location
		if userClaims.Role != "driver" {
			utils.RespondError(w, http.StatusForbidden, "Only drivers can post location")
			return
		}

		var req struct {
			Latitude  float64  `json:"latitude"`
			Longitude float64  `json:"longitude"`
			Heading   *float64 `json:"heading"`
			Speed     *float64 `json:"speed"`
			Accuracy  *float64 `json:"accuracy"`
			ShiftID   *string  `json:"shift_id"`
			Timestamp int64    `json:"timestamp"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// Validate coordinates
		if req.Latitude < -90 || req.Latitude > 90 || req.Longitude < -180 || req.Longitude > 180 {
			log.Printf("❌ Invalid coordinates: lat=%.6f, lng=%.6f", req.Latitude, req.Longitude)
			utils.RespondError(w, http.StatusBadRequest, "Invalid coordinates")
			return
		}

		log.Printf("📍 Location update from driver %s: (%.6f, %.6f)", userClaims.UserID, req.Latitude, req.Longitude)

		// Default accuracy to 100m if not provided
		accuracyValue := 100.0
		if req.Accuracy != nil {
			accuracyValue = *req.Accuracy
		}

		// Step 1: Snap to roads using OSRM (if hub has roadsClient and accuracy > 15m)
		snappedLat := req.Latitude
		snappedLng := req.Longitude

		if hub != nil && hub.GetRoadsClient() != nil {
			roadsClient := hub.GetRoadsClient()
			newLat, newLng, err := roadsClient.SnapToRoad(req.Latitude, req.Longitude, accuracyValue)
			if err == nil && (newLat != req.Latitude || newLng != req.Longitude) {
				snappedLat = newLat
				snappedLng = newLng
				log.Printf("🗺️  Snapped to road: (%.6f, %.6f) → (%.6f, %.6f)", req.Latitude, req.Longitude, snappedLat, snappedLng)
			}
		}

		// Step 2: Update driver_current_location (UPSERT for shift start validation)
		currentLocationQuery := `
			INSERT INTO driver_current_location (
				driver_id, latitude, longitude, heading, speed, accuracy, shift_id, timestamp, is_connected, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE, EXTRACT(EPOCH FROM NOW())::BIGINT)
			ON CONFLICT (driver_id)
			DO UPDATE SET
				latitude = EXCLUDED.latitude,
				longitude = EXCLUDED.longitude,
				heading = EXCLUDED.heading,
				speed = EXCLUDED.speed,
				accuracy = EXCLUDED.accuracy,
				shift_id = EXCLUDED.shift_id,
				timestamp = EXCLUDED.timestamp,
				is_connected = TRUE,
				updated_at = EXTRACT(EPOCH FROM NOW())::BIGINT
			RETURNING updated_at
		`

		var updatedAt int64
		err := db.QueryRow(
			currentLocationQuery,
			userClaims.UserID,
			req.Latitude,  // Store ORIGINAL GPS for audit
			req.Longitude, // Store ORIGINAL GPS for audit
			req.Heading,
			req.Speed,
			req.Accuracy,
			req.ShiftID,
			req.Timestamp,
		).Scan(&updatedAt)

		if err != nil {
			log.Printf("❌ Error updating driver_current_location: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to save location")
			return
		}

		log.Printf("✅ Updated driver_current_location (original GPS for audit)")

		// Step 2.5: Save ORIGINAL GPS to Redis so the HTTP path is a true peer of
		// the Centrifugo publish-proxy path. Redis is the live source of truth that
		// preflight, StartShift, and the dashboard read — without this, a location
		// arriving via HTTP (e.g. when Centrifugo is down) reaches Postgres but
		// stays invisible to the live system for up to 30s (next batch flush).
		// Same JSON shape the proxy writes (see LocationData in centrifugo_location_proxy.go).
		if redisClient != nil {
			locData := LocationData{
				Latitude:  req.Latitude,
				Longitude: req.Longitude,
				Accuracy:  accuracyValue,
				Heading:   getFloat64Value(req.Heading),
				Speed:     getFloat64Value(req.Speed),
				ShiftID:   req.ShiftID,
				Timestamp: req.Timestamp,
			}
			if locationJSON, mErr := json.Marshal(locData); mErr == nil {
				if rErr := redisClient.SaveDriverLocation(r.Context(), db.OrgID(), userClaims.UserID, string(locationJSON)); rErr != nil {
					log.Printf("⚠️  Failed to save location to Redis: %v", rErr)
				}
			}
		}

		// Step 3: Insert into driver_locations (historical log - optional)
		// Note: This table may not exist in your schema, commenting out for now
		// historicalQuery := `
		// 	INSERT INTO driver_locations (
		// 		driver_id, latitude, longitude, heading, speed, accuracy, shift_id, timestamp
		// 	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		// `
		// _, _ = db.Exec(historicalQuery, userClaims.UserID, req.Latitude, req.Longitude, req.Heading, req.Speed, req.Accuracy, req.ShiftID, req.Timestamp)

		// Step 4: Publish SNAPPED location to Centrifugo for managers
		if centrifugoClient != nil {
			// Convert pointer fields to values (use 0 if nil)
			heading := 0.0
			if req.Heading != nil {
				heading = *req.Heading
			}
			speed := 0.0
			if req.Speed != nil {
				speed = *req.Speed
			}
			accuracy := accuracyValue

			locationData := centrifugo.DriverLocation{
				Latitude:  snappedLat, // SNAPPED coordinates for display
				Longitude: snappedLng, // SNAPPED coordinates for display
				Heading:   heading,
				Speed:     speed,
				Accuracy:  accuracy,
				Timestamp: req.Timestamp,
			}

			err := centrifugoClient.PublishDriverLocation(r.Context(), userClaims.UserID, locationData)
			if err != nil {
				log.Printf("⚠️  Failed to publish to Centrifugo: %v", err)
				// Don't fail the request - location is already saved to DB
			} else {
				log.Printf("📤 Published snapped location to Centrifugo: driver:location:%s", userClaims.UserID)
			}
		}

		// Step 5: Broadcast SNAPPED location to managers via legacy WebSocket
		locationUpdate := map[string]interface{}{
			"type": "driver_location_update",
			"data": map[string]interface{}{
				"driver_id":  userClaims.UserID,
				"latitude":   snappedLat, // SNAPPED for display
				"longitude":  snappedLng, // SNAPPED for display
				"heading":    req.Heading,
				"speed":      req.Speed,
				"accuracy":   req.Accuracy,
				"shift_id":   req.ShiftID,
				"timestamp":  req.Timestamp,
				"updated_at": updatedAt,
			},
		}

		hub.BroadcastToRole("admin", locationUpdate)
		// log.Printf("📤 Broadcasted snapped location to managers via WebSocket")

		// Return success response
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success":    true,
			"updated_at": updatedAt,
			"snapped": map[string]interface{}{
				"latitude":  snappedLat,
				"longitude": snappedLng,
			},
		})
	}
}

// getFloat64Value safely extracts value from pointer or returns 0
func getFloat64Value(val *float64) float64 {
	if val == nil {
		return 0
	}
	return *val
}

// GetAllDrivers returns all drivers regardless of shift status
// Drivers with active shifts will show their current shift info
// Drivers without active shifts will show status as 'inactive'
// Location data comes from Redis (real-time current position)
// GET /api/manager/drivers
func GetAllDrivers(root *sqlx.DB, redisClient *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		log.Println("📋 GetAllDrivers: Fetching all drivers...")

		ctx := context.Background()

		// 1. Get all driver locations from Redis
		var locations map[string]string
		if redisClient != nil {
			var err error
			locations, err = redisClient.GetOrgDriverLocations(ctx, db.OrgID())
			if err != nil {
				log.Printf("⚠️ Redis error (non-fatal): %v", err)
				locations = make(map[string]string)
			} else {
				log.Printf("📍 Found %d drivers with location data in Redis", len(locations))
			}
		} else {
			locations = make(map[string]string)
		}

		// 2. Query database for all drivers
		query := `
			SELECT
				u.id AS driver_id,
				u.name AS driver_name,
				u.email,
				s.id AS shift_id,
				s.route_id,
				s.status AS shift_status,
				s.start_time,
				s.total_bins,
				s.completed_bins,
				s.updated_at
			FROM users u
			LEFT JOIN shifts s ON u.id = s.driver_id AND s.status IN ('ready', 'active', 'paused')
			WHERE u.role = 'driver'
			ORDER BY
				CASE
					WHEN s.status IS NOT NULL THEN 0  -- Active drivers first
					ELSE 1                             -- Idle drivers last
				END,
				s.updated_at DESC NULLS LAST,
				u.name ASC
		`

		rows, err := db.Query(query)
		if err != nil {
			log.Printf("❌ Database error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Failed to fetch drivers",
			})
			return
		}
		defer rows.Close()

		var allDrivers []AllDriverResponse

		for rows.Next() {
			var driver AllDriverResponse
			var shiftID, routeID, shiftStatus sql.NullString
			var startTime, updatedAt sql.NullInt64
			var totalBins, completedBins sql.NullInt32

			err := rows.Scan(
				&driver.DriverID,
				&driver.DriverName,
				&driver.Email,
				&shiftID,
				&routeID,
				&shiftStatus,
				&startTime,
				&totalBins,
				&completedBins,
				&updatedAt,
			)
			if err != nil {
				log.Printf("❌ Row scan error: %v", err)
				continue
			}

			// 3. Get location from Redis if available
			if locationJSON, ok := locations[driver.DriverID]; ok {
				var location LocationData
				if err := json.Unmarshal([]byte(locationJSON), &location); err == nil {
					driver.CurrentLocation = &DriverLocation{
						Latitude:  location.Latitude,
						Longitude: location.Longitude,
					}

					// Calculate location age
					locationAge := time.Now().Unix() - (location.Timestamp / 1000)
					t := location.Timestamp / 1000
					driver.UpdatedAt = &t

					log.Printf("   📍 %s: has location (age=%ds)", driver.DriverName, locationAge)
				}
			}

			// 4. Set shift-related fields if driver has an active shift
			if shiftID.Valid {
				driver.ShiftID = &shiftID.String
				driver.Status = shiftStatus.String

				if routeID.Valid {
					driver.RouteID = &routeID.String
				}
				if startTime.Valid {
					t := startTime.Int64
					driver.StartTime = &t
				}
				if totalBins.Valid {
					driver.TotalBins = int(totalBins.Int32)
				}
				if completedBins.Valid {
					driver.CompletedBins = int(completedBins.Int32)
				}
			} else {
				// No active shift - driver is inactive
				driver.Status = "inactive"
				driver.TotalBins = 0
				driver.CompletedBins = 0
			}

			allDrivers = append(allDrivers, driver)
		}

		if err = rows.Err(); err != nil {
			log.Printf("❌ Rows iteration error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Failed to process drivers",
			})
			return
		}

		log.Printf("✅ Found %d driver(s)", len(allDrivers))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    allDrivers,
		})
	}
}

// Helper Functions for Incident Reporting and No-Go Zones

// calculateZoneDistance calculates the distance in meters between two coordinates (Haversine).
