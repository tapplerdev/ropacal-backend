package handlers

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"

	"github.com/jmoiron/sqlx"
	"ropacal-backend/internal/models"
)

// DriverShiftDetailResponse represents detailed shift information for a specific driver
type DriverShiftDetailResponse struct {
	DriverID          string                       `json:"driver_id"`
	DriverName        string                       `json:"driver_name"`
	ShiftID           string                       `json:"shift_id"`
	RouteID           *string                      `json:"route_id"`
	Status            string                       `json:"status"`
	StartTime         *int64                       `json:"start_time"`
	EndTime           *int64                       `json:"end_time"`
	TotalBins         int                          `json:"total_bins"`
	CompletedBins     int                          `json:"completed_bins"`
	TotalPauseSeconds int                          `json:"total_pause_seconds"`
	CreatedAt         int64                        `json:"created_at"`
	UpdatedAt         int64                        `json:"updated_at"`
	Bins              []models.ShiftBinWithDetails `json:"bins"`
}

// GetDriverShiftDetails returns detailed shift information for a specific driver (manager view)
func GetDriverShiftDetails(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		driverID := r.URL.Query().Get("driver_id")
		if driverID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "driver_id is required",
			})
			return
		}

		log.Printf("📋 GetDriverShiftDetails: Fetching shift details for driver: %s", driverID)

		// First, get the active shift for this driver
		shiftQuery := `
		SELECT
			s.id AS shift_id,
			s.driver_id,
			u.name AS driver_name,
			s.route_id,
			s.status,
			s.start_time,
			s.end_time,
			s.total_bins,
			s.completed_bins,
			s.total_pause_seconds,
			s.created_at,
			s.updated_at
		FROM shifts s
		INNER JOIN users u ON s.driver_id = u.id
		WHERE s.driver_id = $1
			AND s.status IN ('ready', 'active', 'paused')
		ORDER BY s.created_at DESC
		LIMIT 1
	`

		var detail DriverShiftDetailResponse
		err := db.QueryRow(shiftQuery, driverID).Scan(
			&detail.ShiftID,
			&detail.DriverID,
			&detail.DriverName,
			&detail.RouteID,
			&detail.Status,
			&detail.StartTime,
			&detail.EndTime,
			&detail.TotalBins,
			&detail.CompletedBins,
			&detail.TotalPauseSeconds,
			&detail.CreatedAt,
			&detail.UpdatedAt,
		)

		if err == sql.ErrNoRows {
			log.Printf("⚠️  No active shift found for driver: %s", driverID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "No active shift found for this driver",
			})
			return
		}

		if err != nil {
			log.Printf("❌ Database error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Failed to fetch shift details",
			})
			return
		}

		// Now get the bins for this shift
		binsQuery := `
		SELECT
			rb.id,
			rb.shift_id,
			rb.bin_id,
			rb.sequence_order,
			rb.is_completed,
			rb.completed_at,
			rb.updated_fill_percentage,
			rb.created_at,
			b.bin_number,
			b.current_street,
			b.city,
			b.zip,
			COALESCE(b.fill_percentage, 0) as fill_percentage,
			b.latitude,
			b.longitude
		FROM shift_bins rb
		INNER JOIN bins b ON rb.bin_id = b.id
		WHERE rb.shift_id = $1
		ORDER BY rb.sequence_order ASC
	`

		rows, err := db.Query(binsQuery, detail.ShiftID)
		if err != nil {
			log.Printf("❌ Error fetching bins: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Failed to fetch bins",
			})
			return
		}
		defer rows.Close()

		var bins []models.ShiftBinWithDetails
		for rows.Next() {
			var bin models.ShiftBinWithDetails
			err := rows.Scan(
				&bin.ID,
				&bin.ShiftID,
				&bin.BinID,
				&bin.SequenceOrder,
				&bin.IsCompleted,
				&bin.CompletedAt,
				&bin.UpdatedFillPercentage,
				&bin.CreatedAt,
				&bin.BinNumber,
				&bin.CurrentStreet,
				&bin.City,
				&bin.Zip,
				&bin.FillPercentage,
				&bin.Latitude,
				&bin.Longitude,
			)
			if err != nil {
				log.Printf("❌ Error scanning bin: %v", err)
				continue
			}
			bins = append(bins, bin)
		}

		detail.Bins = bins

		log.Printf("✅ Found shift with %d bins for driver: %s", len(bins), detail.DriverName)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    detail,
		})
	}
}
