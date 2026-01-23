package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ropacal-backend/internal/models"
	"ropacal-backend/internal/websocket"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

func GetBins(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Auto-uncheck bins older than 3 days
		threeDaysAgo := time.Now().Add(-3 * 24 * time.Hour).Unix()
		_, err := db.Exec(`
			UPDATE bins
			SET checked = 0
			WHERE checked = 1 AND last_checked IS NOT NULL AND last_checked < $1
		`, threeDaysAgo)
		if err != nil {
			http.Error(w, "Failed to update bins", http.StatusInternalServerError)
			return
		}

		// Get all bins
		var bins []models.Bin
		err = db.Select(&bins, `
			SELECT id, bin_number, current_street, city, zip,
			       last_moved, last_checked, status, fill_percentage,
			       checked, move_requested, latitude, longitude,
			       created_at, updated_at
			FROM bins
			ORDER BY bin_number ASC
		`)
		if err != nil {
			http.Error(w, "Failed to fetch bins", http.StatusInternalServerError)
			return
		}

		// Convert to response format
		responses := make([]models.BinResponse, len(bins))
		for i, bin := range bins {
			responses[i] = bin.ToBinResponse()
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}

func CreateBin(db *sqlx.DB, wsHub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req models.CreateBinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate required fields
		if req.CurrentStreet == "" || req.City == "" || req.Zip == "" || req.Status == "" {
			http.Error(w, "Missing required fields (current_street, city, zip, status)", http.StatusBadRequest)
			return
		}

		// Auto-assign bin_number if not provided
		var binNumber int
		if req.BinNumber != nil && *req.BinNumber > 0 {
			// Use provided bin number (for manual override or migration)
			binNumber = *req.BinNumber
			log.Printf("[CREATE-BIN] Using provided bin_number: %d", binNumber)
		} else {
			// Auto-assign based on highest existing bin_number (including retired bins)
			// This ensures continuity: if bins are 54, 55, 56, 57, next will be 58
			var maxBinNumber sql.NullInt64
			err := db.Get(&maxBinNumber, "SELECT MAX(bin_number) FROM bins")
			if err != nil {
				log.Printf("❌ [CREATE-BIN] Failed to get max bin_number: %v", err)
				http.Error(w, "Failed to generate bin number", http.StatusInternalServerError)
				return
			}

			if maxBinNumber.Valid {
				binNumber = int(maxBinNumber.Int64) + 1
			} else {
				// No bins exist yet, start at 1
				binNumber = 1
			}
			log.Printf("[CREATE-BIN] Auto-assigned bin_number: %d (max existing: %v)", binNumber, maxBinNumber)
		}

		// Generate UUID for new bin
		id := uuid.New().String()
		now := time.Now().Unix()

		// Get user ID from context (if authenticated)
		userID, _ := r.Context().Value("user_id").(string)

		// Default fill_percentage to 0 if not provided
		fillPercentage := 0
		if req.FillPercentage != nil {
			fillPercentage = *req.FillPercentage
		}

		// Insert bin
		_, err := db.Exec(`
			INSERT INTO bins (
				id, bin_number, current_street, city, zip, status,
				fill_percentage, checked, move_requested, latitude, longitude,
				created_by_user_id, created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		`,
			id, binNumber, req.CurrentStreet, req.City, req.Zip, req.Status,
			fillPercentage, 0, 0, req.Latitude, req.Longitude,
			&userID, now, now,
		)

		if err != nil {
			// Check if bin_number already exists
			if strings.Contains(err.Error(), "duplicate key") {
				log.Printf("❌ [CREATE-BIN] Bin number %d already exists", binNumber)
				http.Error(w, "Bin number already exists", http.StatusConflict)
				return
			}
			log.Printf("❌ [CREATE-BIN] Database insert failed: %v", err)
			http.Error(w, "Failed to create bin", http.StatusInternalServerError)
			return
		}

		// Fetch created bin
		var created models.Bin
		err = db.Get(&created, "SELECT * FROM bins WHERE id = $1", id)
		if err != nil {
			log.Printf("❌ [CREATE-BIN] Failed to fetch created bin: %v", err)
			http.Error(w, "Failed to fetch created bin", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [CREATE-BIN] Created bin #%d (ID: %s) at %s, %s", binNumber, id, req.CurrentStreet, req.City)

		// Broadcast to all managers
		wsHub.BroadcastToRole("admin", map[string]interface{}{
			"type": "bin_created",
			"data": created.ToBinResponse(),
		})
		log.Printf("📤 [CREATE-BIN] WebSocket event broadcasted to managers")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(created.ToBinResponse())
	}
}

func UpdateBin(db *sqlx.DB, wsHub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		// Extract user from JWT context (for checked_by tracking)
		var userID *string
		if claims, ok := r.Context().Value("userClaims").(map[string]interface{}); ok {
			if uid, ok := claims["user_id"].(string); ok {
				userID = &uid
			}
		}

		var req models.UpdateBinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Get existing bin
		var existing models.Bin
		err := db.Get(&existing, "SELECT * FROM bins WHERE id = $1", id)
		if err == sql.ErrNoRows {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		wasChecked := existing.Checked
		becomingChecked := req.Checked && !wasChecked

		// Determine check time
		now := time.Now()
		if req.CheckedOnIso != nil {
			if parsed, err := time.Parse(time.RFC3339, *req.CheckedOnIso); err == nil {
				now = parsed
			}
		}

		// Clamp fill percentage
		if req.FillPercentage != nil {
			val := *req.FillPercentage
			if val < 0 {
				val = 0
			}
			if val > 100 {
				val = 100
			}
			req.FillPercentage = &val
		}

		// Check if address changed
		addrChanged := strings.TrimSpace(req.CurrentStreet) != existing.CurrentStreet ||
			strings.TrimSpace(req.City) != existing.City ||
			strings.TrimSpace(req.Zip) != existing.Zip

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			http.Error(w, "Failed to begin transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		// Build update query
		query := `
			UPDATE bins
			SET current_street = $1, city = $2, zip = $3, status = $4,
			    checked = $5, fill_percentage = $6, move_requested = $7`

		args := []interface{}{
			req.CurrentStreet, req.City, req.Zip, req.Status,
			req.Checked, req.FillPercentage, req.MoveRequested,
		}

		paramCount := 7
		if becomingChecked {
			paramCount++
			query += `, last_checked = $` + fmt.Sprintf("%d", paramCount)
			args = append(args, now.Unix())
		}

		if addrChanged {
			query += `, latitude = NULL, longitude = NULL`
		}

		paramCount++
		query += `, updated_at = $` + fmt.Sprintf("%d", paramCount) + ` WHERE id = $` + fmt.Sprintf("%d", paramCount+1)
		args = append(args, time.Now().Unix(), id)

		_, err = tx.Exec(query, args...)
		if err != nil {
			http.Error(w, "Failed to update bin", http.StatusInternalServerError)
			return
		}

		// If becoming checked, insert check record
		if becomingChecked {
			checkedFrom := ""
			if req.CheckedFrom != nil && strings.TrimSpace(*req.CheckedFrom) != "" {
				checkedFrom = *req.CheckedFrom
			} else {
				checkedFrom = req.CurrentStreet + ", " + req.City + " " + req.Zip
			}

			fillForCheck := 0
			if req.FillPercentage != nil {
				fillForCheck = *req.FillPercentage
			}

			// Include checked_by (authenticated user) and photo_url if provided
			_, err = tx.Exec(`
				INSERT INTO checks (bin_id, checked_from, fill_percentage, checked_on, checked_by, photo_url)
				VALUES ($1, $2, $3, $4, $5, $6)
			`, id, checkedFrom, fillForCheck, now.Unix(), userID, req.PhotoUrl)
			if err != nil {
				http.Error(w, "Failed to create check record", http.StatusInternalServerError)
				return
			}
		}

		// Commit transaction
		if err := tx.Commit(); err != nil {
			http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
			return
		}

		// Fetch updated bin
		var updated models.Bin
		err = db.Get(&updated, "SELECT * FROM bins WHERE id = $1", id)
		if err != nil {
			http.Error(w, "Failed to fetch updated bin", http.StatusInternalServerError)
			return
		}

		// Broadcast to all managers
		wsHub.BroadcastToRole("admin", map[string]interface{}{
			"type": "bin_updated",
			"data": updated.ToBinResponse(),
		})
		log.Printf("📤 [UPDATE-BIN] WebSocket event broadcasted to managers")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(updated.ToBinResponse())
	}
}

func DeleteBin(db *sqlx.DB, wsHub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		result, err := db.Exec("DELETE FROM bins WHERE id = $1", id)
		if err != nil {
			http.Error(w, "Failed to delete", http.StatusInternalServerError)
			return
		}

		rows, err := result.RowsAffected()
		if err != nil || rows == 0 {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		// Broadcast to all managers
		wsHub.BroadcastToRole("admin", map[string]interface{}{
			"type": "bin_deleted",
			"data": map[string]interface{}{
				"bin_id": id,
			},
		})
		log.Printf("📤 [DELETE-BIN] WebSocket event broadcasted to managers")

		w.WriteHeader(http.StatusNoContent)
	}
}

// LoadRealBins clears test data and loads real production bins (admin only)
func LoadRealBins(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("🗑️  REQUEST: POST /api/admin/bins/load-real")

		// Step 1: Delete all non-Dallas bins
		deleteResult, err := db.Exec("DELETE FROM bins WHERE city != 'Dallas'")
		if err != nil {
			fmt.Printf("❌ Error deleting test bins: %v\n", err)
			http.Error(w, "Failed to delete test bins", http.StatusInternalServerError)
			return
		}

		deletedRows, _ := deleteResult.RowsAffected()
		fmt.Printf("✅ Deleted %d test bins\n", deletedRows)

		// Step 2: Insert real bins data
		// Using the migration SQL directly embedded in code
		migrationSQL := `
INSERT INTO bins (id, bin_number, current_street, city, zip, last_moved, last_checked, status, fill_percentage, checked, move_requested, latitude, longitude, created_at, updated_at) VALUES
('c96c3c41-fdbd-4777-86eb-326edba84309', 1, '143 E El Camino Real', 'Mountain View', '94040', NULL, 1723403460, 'Missing', 40, 0, 0, 37.37858, -122.071589, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('14a67be5-9b31-4acf-bf48-4aacb39d3130', 2, '1101 W El Camino Real', 'Sunnyvale', '94087', NULL, 1729984568, 'Active', 25, 0, 0, 37.37386, -122.05294, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('8f4f7f05-c61f-4e20-9bc4-6db3f4defd59', 3, '615 Coleman Ave', 'San Jose', '95110', NULL, 1732068105, 'Active', 100, 0, 0, 37.340408, -121.908161, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('2c7d4b00-c070-4515-b91d-da85ec6b53b7', 4, '2400 Charleston Rd', 'Mountain View', '94043', NULL, 1732414957, 'Active', 40, 0, 0, 37.42182, -122.09657, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('826043b0-07a1-455c-ad5e-f7f8b6198262', 5, '1060 E El Camino Real', 'Sunnyvale', '94087', 1729983361, 1732420652, 'Active', 30, 0, 0, NULL, NULL, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('8e35d3f5-ff41-454b-84e8-224677e740c9', 6, '2161 Monterey Rd', 'San Jose', '95125', NULL, 1723475220, 'Missing', 90, 0, 0, 37.30441, -121.86563, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('d26930d5-394f-4d2b-9b33-759881f791b7', 7, '5055 Almaden Expy', 'San Jose', '95118', NULL, 1732424057, 'Active', 10, 0, 0, 37.25727, -121.8765, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('ebf3cc8a-0c2c-409b-92fb-96fda4b4c2e3', 8, '1933 W El Camino Real', 'Mountain View', '94040', NULL, 1732418838, 'Active', 40, 0, 0, 37.393042, -122.097551, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('153afdd6-fb6c-4ce1-80cc-8ba59302d4db', 9, '5524 Monterey Rd', 'San Jose', '95138', NULL, 1730042536, 'Missing', 100, 0, 0, 37.25637, -121.79907, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('48b39523-183e-49ed-9df5-c0d7352cc29f', 10, '3635 El Camino Real', 'Santa Clara', '95051', NULL, 1732421011, 'Active', 5, 0, 0, 37.352291, -121.988535, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('48f7c1c0-9fab-4b74-bf03-fc8a86156afc', 11, '199 E Middlefield Rd Ste 200', 'Mountain View', '94043', NULL, 1732414062, 'Active', 30, 0, 0, 37.397222, -122.062096, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('0f0ef655-77e2-4738-8976-d04841b41c8c', 12, '1660 Winchester Blvd', 'Campbell', '95008', NULL, NULL, 'Missing', 0, 0, 0, 37.293058, -121.949259, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('f1649781-bcde-4412-a313-a49801de73ed', 13, '1305 S Winchester Blvd', 'San Jose', '95117', NULL, NULL, 'Missing', 0, 0, 0, 37.300899, -121.951779, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('ad92c60c-4b10-4c51-ae47-155b0def8baa', 14, '4644 Meridian Ave', 'San Jose', '95124', NULL, 1732423438, 'Active', 100, 0, 0, 37.256866, -121.897573, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('46f118f7-ac2f-436f-a658-f17ccb0b775c', 15, '1130 Branham Ln', 'San Jose', '95118', NULL, 1722433440, 'Missing', 15, 0, 0, 37.26202, -121.878139, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('2548deaf-8e80-49f6-a2ac-92aaffb45032', 16, '1721 E Bayshore Rd', 'Palo Alto', '94303', NULL, 1732409894, 'Active', 100, 0, 0, 37.460225, -122.137732, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('4c19a8b2-2ffa-47e4-9965-3cc0e4583cdb', 17, '2720 El Camino Real', 'Santa Clara', '95051', NULL, 1732421559, 'Active', 20, 0, 0, 37.352196, -121.975935, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('e30d6984-1ac5-4cce-bc8d-4840ab849197', 18, '1691 The Alameda San Jose, CA  95126 United States', 'San Jose', '95126', 1729448553, 1728108195, 'Missing', 30, 0, 0, 37.337079, -121.919615, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('35ee2834-8b1c-406f-a720-974ecd833dc1', 19, '1041 El Monte Ave', 'Mountain View', '94040', NULL, 1732418844, 'Active', 100, 0, 0, 37.390051, -122.094742, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('fa541645-13fa-479c-93d2-5b8d7cc958ec', 20, '2510 W El Camino Real Suite 2', 'Mountain View', '94040', NULL, 1732416763, 'Active', 25, 0, 0, 37.40003, -122.110255, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('bfa30cb2-14ee-4aa0-871c-7fa78fddbe6a', 21, 'Mountain View Shopping Center 121 E El Camino Real Mountain View, CA  94040 United States', 'Mountain View', '94040', 1729120112, 1732052767, 'Missing', 70, 0, 0, NULL, NULL, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('1959f69c-0d7e-4f09-a184-17fd47aa3b63', 22, '1757 W San Carlos St San Jose, CA  95128 United States', 'San Jose', '95128', 1728114850, 1732422663, 'Active', 40, 0, 0, NULL, NULL, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('baf638d9-a0b9-40bc-a232-4b17ccc6e828', 23, '1425 Lafayette St Santa Clara, CA  95050 United States', 'Santa Clara', '95050', 1726533799, 1732059576, 'Active', 40, 0, 0, 37.35466, -121.94572, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('e3f52b60-8164-4adb-9c1c-f908719d3d3a', 24, '3904 Middlefield Rd', 'Palo Alto', '94303', NULL, 1728508791, 'Missing', 20, 0, 0, 37.419241, -122.110524, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('fcbd018f-bfd2-4615-a5fa-06fa96fc0fbf', 25, '2811 Middlefield Rd', 'Palo Alto', '94306', NULL, 1732411205, 'Active', 80, 0, 0, 37.43288, -122.127406, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('c207b5fe-880b-414b-b1fe-02283b5aee36', 26, '887 E El Camino Real Sunnyvale, CA  94087 United States', 'Sunnyvale', '94087', 1725868335, 1732420107, 'Active', 25, 0, 0, 37.354035, -122.014985, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('c552735a-354d-45fe-8637-484f2daa1341', 27, '525 El Camino Real', 'Menlo Park', '94025', NULL, NULL, 'Active', 0, 0, 0, 37.452067, -122.178904, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('6da99eed-a351-4396-80a2-447b1d703eb5', 28, '200 Woodside Plaza', 'Redwood City', '94061', NULL, 1723388760, 'Missing', 100, 0, 0, 37.456535, -122.229593, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('75401257-1c80-4d37-ba9e-6ee484b41487', 29, '5269 Prospect Rd San Jose, CA  95129 United States', 'San Jose', '95129', 1726528434, 1726503609, 'Missing', 0, 0, 1, 37.292949, -121.994242, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('f4906f01-453b-4419-ba4f-6857c6a5ae49', 30, '2495 Lafayette St', 'Santa Clara', '95050', NULL, 1732063444, 'Active', 30, 0, 0, 37.36573, -121.94979, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('74da2fad-2e1e-44ce-896d-2437edb83b1d', 31, '590 Showers Dr', 'Mountain View', '94040', NULL, 1732416751, 'Active', 25, 0, 0, 37.402102, -122.110739, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('1a989b38-f538-4bd9-9444-dcdef185f8c6', 32, '20 Woodside Plaza', 'Redwood City', '94061', NULL, 1723995720, 'Missing', 100, 0, 0, 37.457956, -122.22879, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('efac394f-307a-4b1b-a0ab-21cd8a54d0ae', 33, '1349 Coleman Ave Santa Clara, CA  95050 United States', 'Santa Clara', '95050', 1729456542, 1730656400, 'Active', 20, 0, 0, 37.356773, -121.935764, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('b14813e2-48a1-4075-87ba-654fb362ada3', 34, '2485 El Camino Real', 'Redwood City', '94063', NULL, 1722351600, 'Missing', 50, 0, 0, 37.475639, -122.217094, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('5438c040-1b57-4087-8651-5e1a85fec954', 35, '1920 Camden Ave San Jose, CA  95124 United States', 'San Jose', '95124', 1730768552, 1732422258, 'Active', 2, 0, 0, NULL, NULL, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('3f9d27a3-bf21-4c7a-987a-5048969afaa4', 36, '2407 El Camino Real Redwood City, CA  94063 United States', 'Redwood City', '94063', 1725860739, 1732410071, 'Active', 100, 0, 0, 37.485323, -122.229117, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('6e7c94b9-e894-4a4c-b20b-d72b19226c5e', 37, '1884 S Norfolk St', 'San Mateo', '94403', NULL, 1732406522, 'Active', 100, 0, 0, 37.554401, -122.29191, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('0a3d7c94-17fd-4951-b11c-698630255f4d', 38, '516 El Camino Real', 'Belmont', '94002', NULL, 1732408347, 'Active', 80, 0, 0, 37.52528, -122.282498, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('da4d6093-eff1-41b4-bd2c-95e97a653122', 39, '1119 Industrial Rd Ste F', 'San Carlos', '94070', NULL, 1727367714, 'Missing', 30, 0, 1, 37.50393, -122.246335, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('5d6c53f6-a03d-4ac2-846c-1fe7382f5e12', 40, '2220 Bridgepointe Pkwy', 'San Mateo', '94404', NULL, 1732407068, 'Active', 100, 0, 0, 37.558595, -122.283297, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('2b92ba74-cd05-426f-8c2c-56094fd60512', 41, '640 Concar Dr', 'San Mateo', '94402', NULL, 1732405547, 'Active', 40, 0, 0, 37.553598, -122.304754, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('64ff1a7c-e994-4544-8188-d3e3ac87aa92', 42, '3904 Middlefield Rd', 'Palo Alto', '94303', 1728582476, 1732415730, 'Active', 40, 0, 0, NULL, NULL, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('35b5527b-25f8-4373-8255-fe1ab0eac396', 43, '2021 The Alameda', 'San Jose', '95126', NULL, 1729183170, 'Active', 5, 0, 0, 37.342805, -121.927995, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('f7aac47e-7479-458d-a717-b792963f9a4f', 44, '4960 Almaden Expy San Jose, CA  95118 United States', 'San Jose', '95118', 1727318409, 1731213193, 'Active', 50, 0, 0, 37.2605, -121.874759, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT);
`

		_, err = db.Exec(migrationSQL)
		if err != nil {
			fmt.Printf("❌ Error inserting bins: %v\n", err)
			http.Error(w, "Failed to load bins", http.StatusInternalServerError)
			return
		}

		fmt.Println("✅ Inserted 44 real bins")

		// Query summary
		var summary struct {
			TotalBins         int `db:"total_bins"`
			ActiveBins        int `db:"active_bins"`
			MissingBins       int `db:"missing_bins"`
			BinsWithoutCoords int `db:"bins_without_coords"`
		}

		err = db.Get(&summary, `
			SELECT
				COUNT(*) AS total_bins,
				COUNT(CASE WHEN status = 'Active' THEN 1 END) AS active_bins,
				COUNT(CASE WHEN status = 'Missing' THEN 1 END) AS missing_bins,
				COUNT(CASE WHEN latitude IS NULL OR longitude IS NULL THEN 1 END) AS bins_without_coords
			FROM bins
			WHERE city != 'Dallas'
		`)

		if err != nil {
			fmt.Printf("⚠️  Error querying summary: %v\n", err)
			// Still return success since insert worked
		} else {
			fmt.Printf("📊 Summary: %d total, %d active, %d missing, %d without coords\n",
				summary.TotalBins, summary.ActiveBins, summary.MissingBins, summary.BinsWithoutCoords)
		}

		// Return success response
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":             true,
			"message":             "Successfully loaded real bins",
			"deleted_test_bins":   deletedRows,
			"loaded_bins":         44,
			"total_bins":          summary.TotalBins,
			"active_bins":         summary.ActiveBins,
			"missing_bins":        summary.MissingBins,
			"bins_without_coords": summary.BinsWithoutCoords,
		})
	}
}

// FixBinStatus lowercases all bin status values for Flutter compatibility
func FixBinStatus(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("🔧 REQUEST: POST /api/admin/bins/fix-status")

		// Update all bin statuses to lowercase
		result, err := db.Exec("UPDATE bins SET status = LOWER(status)")
		if err != nil {
			fmt.Printf("❌ Error updating status: %v\n", err)
			http.Error(w, "Failed to update bin statuses", http.StatusInternalServerError)
			return
		}

		rowsAffected, _ := result.RowsAffected()
		fmt.Printf("✅ Updated %d bin statuses to lowercase\n", rowsAffected)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":       true,
			"message":       "Fixed bin status casing",
			"rows_affected": rowsAffected,
		})
	}
}
