package handlers

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"

	"github.com/jmoiron/sqlx"
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

// GetActiveDrivers returns all drivers with active shifts (ready, active, or paused)
func GetActiveDrivers(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Println("📋 GetActiveDrivers: Fetching all active drivers...")

		query := `
			SELECT
				s.id AS shift_id,
				s.driver_id,
				u.name AS driver_name,
				s.route_id,
				s.status,
				s.start_time,
				s.total_bins,
				s.completed_bins,
				s.updated_at,
				dl.latitude,
				dl.longitude
			FROM shifts s
			INNER JOIN users u ON s.driver_id = u.id
			LEFT JOIN (
				-- Get the most recent location for each driver
				SELECT DISTINCT ON (driver_id)
					driver_id, latitude, longitude
				FROM driver_locations
				ORDER BY driver_id, timestamp DESC
			) dl ON s.driver_id = dl.driver_id
			WHERE s.status IN ('ready', 'active', 'paused')
			ORDER BY s.updated_at DESC
		`

		rows, err := db.Query(query)
		if err != nil {
			log.Printf("❌ Database error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Failed to fetch active drivers",
			})
			return
		}
		defer rows.Close()

		var activeDrivers []ActiveDriverResponse

		for rows.Next() {
			var driver ActiveDriverResponse
			var latitude, longitude sql.NullFloat64

			err := rows.Scan(
				&driver.ShiftID,
				&driver.DriverID,
				&driver.DriverName,
				&driver.RouteID,
				&driver.Status,
				&driver.StartTime,
				&driver.TotalBins,
				&driver.CompletedBins,
				&driver.UpdatedAt,
				&latitude,
				&longitude,
			)
			if err != nil {
				log.Printf("❌ Row scan error: %v", err)
				continue
			}

			// Add location if available
			if latitude.Valid && longitude.Valid {
				driver.CurrentLocation = &DriverLocation{
					Latitude:  latitude.Float64,
					Longitude: longitude.Float64,
				}
			}

			activeDrivers = append(activeDrivers, driver)
		}

		if err = rows.Err(); err != nil {
			log.Printf("❌ Rows iteration error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Failed to process active drivers",
			})
			return
		}

		log.Printf("✅ Found %d active driver(s)", len(activeDrivers))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    activeDrivers,
		})
	}
}
