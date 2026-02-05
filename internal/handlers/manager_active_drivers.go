package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"ropacal-backend/internal/services/redis"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// DriverLocation represents the driver's current GPS location
type DriverLocation struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// ActiveDriverResponse represents an active driver with their current shift
type ActiveDriverResponse struct {
	DriverID        string          `json:"driver_id"`
	DriverName      string          `json:"driver_name"`
	ShiftID         string          `json:"shift_id"`
	RouteID         *string         `json:"route_id"`
	Status          string          `json:"status"`
	StartTime       *int64          `json:"start_time"`
	TotalBins       int             `json:"total_bins"`
	CompletedBins   int             `json:"completed_bins"`
	CurrentLocation *DriverLocation `json:"current_location,omitempty"`
	UpdatedAt       int64           `json:"updated_at"`
}

// AllDriverResponse represents a driver with their current status (used by GetAllDrivers)
type AllDriverResponse struct {
	DriverID        string          `json:"driver_id"`
	DriverName      string          `json:"driver_name"`
	Email           string          `json:"email"`
	ShiftID         *string         `json:"shift_id,omitempty"`
	RouteID         *string         `json:"route_id,omitempty"`
	Status          string          `json:"status"` // 'active', 'paused', 'ready', 'inactive'
	StartTime       *int64          `json:"start_time,omitempty"`
	TotalBins       int             `json:"total_bins"`
	CompletedBins   int             `json:"completed_bins"`
	CurrentLocation *DriverLocation `json:"current_location,omitempty"`
	UpdatedAt       *int64          `json:"updated_at,omitempty"`
}

// LocationData represents GPS data from Redis
type LocationData struct {
	Latitude  float64  `json:"latitude"`
	Longitude float64  `json:"longitude"`
	Heading   *float64 `json:"heading,omitempty"`
	Speed     *float64 `json:"speed,omitempty"`
	Accuracy  float64  `json:"accuracy"`
	ShiftID   *string  `json:"shift_id,omitempty"`
	Timestamp int64    `json:"timestamp"`
}

// GetActiveDrivers returns all drivers publishing location data
// Includes shift info if they're on an active shift
func GetActiveDrivers(db *sqlx.DB, redisClient *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Println("📋 GetActiveDrivers: Fetching drivers from Redis...")

		ctx := context.Background()

		// 1. Get all driver locations from Redis (source of truth for current location)
		locations, err := redisClient.GetAllDriverLocations(ctx)
		if err != nil {
			log.Printf("❌ Redis error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Failed to fetch driver locations",
			})
			return
		}

		if len(locations) == 0 {
			log.Println("📋 No active drivers found (no location data in Redis)")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"data":    []ActiveDriverResponse{},
			})
			return
		}

		log.Printf("📍 Found %d drivers with location data in Redis", len(locations))

		// 2. Get driver IDs for database query
		driverIDs := make([]string, 0, len(locations))
		for driverID := range locations {
			driverIDs = append(driverIDs, driverID)
		}

		// 3. Query database for driver metadata + shift info
		query := `
			SELECT
				u.id AS driver_id,
				u.name AS driver_name,
				s.id AS shift_id,
				s.route_id,
				s.status,
				s.start_time,
				s.total_bins,
				s.completed_bins,
				s.updated_at
			FROM users u
			LEFT JOIN shifts s ON u.id = s.driver_id
				AND s.status IN ('ready', 'active', 'paused')
			WHERE u.id = ANY($1)
			ORDER BY s.updated_at DESC NULLS LAST
		`

		rows, err := db.Query(query, pq.Array(driverIDs))
		if err != nil {
			log.Printf("❌ Database error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Failed to fetch driver info",
			})
			return
		}
		defer rows.Close()

		var activeDrivers []ActiveDriverResponse

		for rows.Next() {
			var driver ActiveDriverResponse
			var shiftID, routeID, status sql.NullString
			var startTime, updatedAt sql.NullInt64
			var totalBins, completedBins sql.NullInt32

			err := rows.Scan(
				&driver.DriverID,
				&driver.DriverName,
				&shiftID,
				&routeID,
				&status,
				&startTime,
				&totalBins,
				&completedBins,
				&updatedAt,
			)
			if err != nil {
				log.Printf("❌ Row scan error: %v", err)
				continue
			}

			// 4. Get location from Redis
			if locationJSON, ok := locations[driver.DriverID]; ok {
				var location LocationData
				if err := json.Unmarshal([]byte(locationJSON), &location); err != nil {
					log.Printf("⚠️ Failed to parse location for driver %s: %v", driver.DriverID, err)
					continue
				}

				driver.CurrentLocation = &DriverLocation{
					Latitude:  location.Latitude,
					Longitude: location.Longitude,
				}

				// Calculate location age
				locationAge := time.Now().Unix() - (location.Timestamp / 1000)
				driver.UpdatedAt = location.Timestamp / 1000

				log.Printf("   📍 Driver %s: lat=%.6f, lng=%.6f, age=%ds",
					driver.DriverName, location.Latitude, location.Longitude, locationAge)
			}

			// 5. Set shift info if available
			if shiftID.Valid {
				driver.ShiftID = shiftID.String
				if routeID.Valid {
					driver.RouteID = &routeID.String
				}
				driver.Status = status.String
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
				// Driver has no active shift (background tracking)
				driver.Status = "inactive"
				driver.TotalBins = 0
				driver.CompletedBins = 0
			}

			activeDrivers = append(activeDrivers, driver)
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

		log.Printf("✅ Returning %d drivers (%d on shift, %d logged in)",
			len(activeDrivers),
			countDriversOnShift(activeDrivers),
			len(activeDrivers)-countDriversOnShift(activeDrivers))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    activeDrivers,
		})
	}
}

// Helper function to count drivers on active shifts
func countDriversOnShift(drivers []ActiveDriverResponse) int {
	count := 0
	for _, d := range drivers {
		if d.ShiftID != "" {
			count++
		}
	}
	return count
}
