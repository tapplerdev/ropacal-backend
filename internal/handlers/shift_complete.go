package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"ropacal-backend/internal/geo"
	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/websocket"
	"ropacal-backend/pkg/utils"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

func CompleteTask(db *sqlx.DB, hub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[DIAGNOSTIC] ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("[DIAGNOSTIC] 📥 REQUEST: POST /api/driver/shift/complete-task")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("[DIAGNOSTIC]    User: %s (%s)", userClaims.Email, userClaims.UserID)

		// Parse request body
		var req struct {
			TaskID                string   `json:"task_id"`                           // ID of route_tasks record (identifies specific waypoint)
			BinID                 string   `json:"bin_id"`                            // DEPRECATED: Use task_id instead
			UpdatedFillPercentage *int     `json:"updated_fill_percentage,omitempty"` // Now optional
			PhotoUrl              *string  `json:"photo_url,omitempty"`               // "Before" photo — bin contents before collection
			AfterPhotoUrl         *string  `json:"after_photo_url,omitempty"`         // "After" photo — bin empty after collection
			PhotoLatitude         *float64 `json:"photo_latitude,omitempty"`          // EXIF GPS from before photo
			PhotoLongitude        *float64 `json:"photo_longitude,omitempty"`         // EXIF GPS from before photo
			AfterPhotoLatitude    *float64 `json:"after_photo_latitude,omitempty"`    // EXIF GPS from after photo
			AfterPhotoLongitude   *float64 `json:"after_photo_longitude,omitempty"`   // EXIF GPS from after photo
			MoveRequestID         *string  `json:"move_request_id,omitempty"`         // Links check to move request
			NewBinNumber          int      `json:"new_bin_number"`                    // REQUIRED: Driver-provided bin number for placements
			CompletionNotes       *string  `json:"completion_notes,omitempty"`        // Driver notes for service tasks

			// Incident reporting fields (all optional)
			HasIncident         bool    `json:"has_incident"`
			IncidentType        *string `json:"incident_type,omitempty"`
			IncidentPhotoUrl    *string `json:"incident_photo_url,omitempty"`
			IncidentDescription *string `json:"incident_description,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("[DIAGNOSTIC] ❌ Error decoding request body: %v", err)
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		log.Printf("[DIAGNOSTIC]    Task ID: %s (route task UUID)", req.TaskID)
		log.Printf("[DIAGNOSTIC]    Bin ID: %s (deprecated)", req.BinID)
		log.Printf("[DIAGNOSTIC] 🔍 FILL PERCENTAGE DEBUG:")
		if req.UpdatedFillPercentage != nil {
			log.Printf("[DIAGNOSTIC]    ✅ Received non-null fill_percentage from mobile app: %d%%", *req.UpdatedFillPercentage)
			log.Printf("[DIAGNOSTIC]    📊 This value WILL be written to database")
		} else {
			log.Printf("[DIAGNOSTIC]    ⚠️  Received NULL fill_percentage from mobile app")
			log.Printf("[DIAGNOSTIC]    📊 Database will store NULL (incident report or no assessment)")
		}
		if req.PhotoUrl != nil {
			log.Printf("[DIAGNOSTIC]    Photo URL: %s", *req.PhotoUrl)
		} else {
			log.Printf("[DIAGNOSTIC]    Photo URL: null (no photo)")
		}
		if req.HasIncident {
			log.Printf("[DIAGNOSTIC]    🚨 INCIDENT REPORTED: %s", *req.IncidentType)
		}

		// Note: Validation for photo/fill percentage is deferred until we know the stop type
		// Warehouse stops don't require photo or fill percentage

		// Validate fill percentage if provided
		if req.UpdatedFillPercentage != nil && (*req.UpdatedFillPercentage < 0 || *req.UpdatedFillPercentage > 100) {
			utils.RespondError(w, http.StatusBadRequest, "Fill percentage must be between 0 and 100")
			return
		}

		// Validate incident fields if incident is being reported
		if req.HasIncident {
			if req.IncidentType == nil {
				utils.RespondError(w, http.StatusBadRequest, "incident_type is required when reporting incident")
				return
			}
			// Validate incident type
			validTypes := map[string]bool{"vandalism": true, "landlord_complaint": true, "theft": true, "relocation_request": true, "missing": true, "damaged": true, "inaccessible": true}
			if !validTypes[*req.IncidentType] {
				utils.RespondError(w, http.StatusBadRequest, "Invalid incident_type")
				return
			}
			// At least photo OR description required for incidents
			if req.IncidentPhotoUrl == nil && req.IncidentDescription == nil {
				utils.RespondError(w, http.StatusBadRequest, "Either incident photo or description is required")
				return
			}
		}

		// Get current active shift
		var shift models.Shift
		err := db.Get(&shift, `SELECT * FROM shifts WHERE driver_id = $1 AND status = 'active' ORDER BY created_at DESC LIMIT 1`, userClaims.UserID)
		if err != nil {
			utils.RespondError(w, http.StatusBadRequest, "No active shift")
			return
		}

		// Mark task as completed (route_tasks ONLY - unified task system)
		now := time.Now().Unix()

		log.Printf("[DIAGNOSTIC] 🔍 Finding task in route_tasks table...")
		log.Printf("[DIAGNOSTIC]    Shift ID: %s", shift.ID)
		log.Printf("[DIAGNOSTIC]    Bin ID: %s", req.BinID)

		// Find the next incomplete task in this shift
		var taskID string
		var taskType string

		if req.BinID != "" {
			// Collection/Pickup/Dropoff task - has bin_id
			log.Printf("[DIAGNOSTIC] 🔍 Looking for task with bin_id...")
			err = db.QueryRow(`
				SELECT id, task_type
				FROM route_tasks
				WHERE shift_id = $1
				  AND bin_id = $2
				  AND is_completed = 0
				ORDER BY sequence_order ASC
				LIMIT 1
			`, shift.ID, req.BinID).Scan(&taskID, &taskType)
		} else {
			// Warehouse or Placement task - use task_id from request
			log.Printf("[DIAGNOSTIC] 🔍 Looking for task by ID: %s", req.TaskID)
			err = db.QueryRow(`
				SELECT id, task_type
				FROM route_tasks
				WHERE shift_id = $1
				  AND id = $2
				  AND is_completed = 0
				LIMIT 1
			`, shift.ID, req.TaskID).Scan(&taskID, &taskType)
		}

		if err == sql.ErrNoRows {
			log.Printf("[DIAGNOSTIC] ⚠️  Task not found in route or already completed")
			utils.RespondError(w, http.StatusBadRequest, "Task not found in route or already completed")
			return
		}
		if err != nil {
			log.Printf("❌ Error querying route_tasks: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to find task")
			return
		}

		log.Printf("[DIAGNOSTIC] ✅ Found task: ID=%s, Type=%s", taskID, taskType)

		// Validate: at least photo OR fill percentage required (unless incident, warehouse, or service)
		if !req.HasIncident && taskType != "warehouse_stop" && taskType != "service" && req.PhotoUrl == nil && req.UpdatedFillPercentage == nil {
			log.Printf("[DIAGNOSTIC] ⚠️  Validation failed: task_type=%s, photo=%v, fill=%v",
				taskType, req.PhotoUrl != nil, req.UpdatedFillPercentage != nil)
			utils.RespondError(w, http.StatusBadRequest, "At least photo or fill percentage is required")
			return
		}

		// Service task validation: check photo if photo_required
		if taskType == "service" {
			var photoRequired bool
			db.QueryRow(`SELECT photo_required FROM route_tasks WHERE id = $1`, taskID).Scan(&photoRequired)
			if photoRequired && req.PhotoUrl == nil {
				log.Printf("[DIAGNOSTIC] ⚠️  Service task requires photo but none provided")
				utils.RespondError(w, http.StatusBadRequest, "Photo is required for this service task")
				return
			}
		}
		// Track newly created bin ID for placement tasks (used later for check record)
		var placementBinID *string

		// ── Pre-tx reads: detect a move + resolve its finalization inputs ─────────
		// The geocode/config reads must never be held open across the transaction.
		var moveRequest models.BinMoveRequest
		moveErr := db.Get(&moveRequest, `
			SELECT * FROM bin_move_requests
			WHERE bin_id = $1
			AND assigned_shift_id = $2
			AND status IN ('assigned', 'in_progress')
		`, req.BinID, shift.ID)
		if moveErr != nil {
			// Fallback: look up move_request_id from the route_task itself
			var moveReqID *string
			db.QueryRow(`SELECT move_request_id FROM route_tasks WHERE id = $1`, taskID).Scan(&moveReqID)
			if moveReqID != nil {
				moveErr = db.Get(&moveRequest, `
					SELECT * FROM bin_move_requests
					WHERE id = $1 AND status IN ('assigned', 'in_progress')
				`, *moveReqID)
			}
		}
		isMoveDropoff := moveErr == nil && taskType == "dropoff"
		var moveFin moveFinalization
		if isMoveDropoff {
			// Leg-order guard: a dropoff must not complete (and finalize the move,
			// relocating the bin in the system) while its paired pickup is still
			// live and incomplete — the bin isn't on the truck yet. Mirrors the
			// RemoveTasks dropoff guard. Skipped/auto-completed pickups
			// (is_completed=1) and consciously removed ones (is_deleted) pass.
			var incompletePickups int
			if gErr := db.Get(&incompletePickups, `
				SELECT COUNT(*) FROM route_tasks
				WHERE shift_id = $1 AND move_request_id = $2
				  AND task_type = 'pickup' AND is_deleted = false AND is_completed = 0
			`, shift.ID, moveRequest.ID); gErr != nil {
				log.Printf("❌ Error checking paired pickup: %v", gErr)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to verify move pickup")
				return
			}
			if incompletePickups > 0 {
				log.Printf("🚫 Dropoff blocked: move %s has an incomplete pickup", moveRequest.ID)
				utils.RespondError(w, http.StatusBadRequest, "Complete the pickup first — the bin has not been picked up yet")
				return
			}
			moveFin = prepareMoveCompletion(db, moveRequest)
		}

		// ── ONE transaction: task completion + every branch write ────────────────
		// (Previously pool writes with manual "uncomplete" compensations — a crash
		// between the completion and its branch writes tore the state.)
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ Error starting completion transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to complete task")
			return
		}
		defer tx.Rollback()

		// Complete via the domain — the single completion write (photos + EXIF GPS).
		rowsAffected, err := itinerary.Complete(tx, taskID, now, itinerary.Completion{
			FillPercentage: req.UpdatedFillPercentage,
			Notes:          req.CompletionNotes,
			PhotoURL:       req.PhotoUrl,
			AfterPhotoURL:  req.AfterPhotoUrl,
			PhotoLat:       req.PhotoLatitude,
			PhotoLng:       req.PhotoLongitude,
			AfterPhotoLat:  req.AfterPhotoLatitude,
			AfterPhotoLng:  req.AfterPhotoLongitude,
		})
		if err != nil {
			log.Printf("❌ Error marking task as completed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to complete task")
			return
		}
		log.Printf("[DIAGNOSTIC] ✅ Task marked as completed in route_tasks table")
		if rowsAffected == 0 {
			log.Printf("[DIAGNOSTIC] ⚠️  Update affected 0 rows")
			utils.RespondError(w, http.StatusBadRequest, "Failed to update task")
			return
		}

		if moveErr == nil {
			// This is a MOVE REQUEST bin!
			log.Printf("[DIAGNOSTIC] 🚚 Detected move request: %s (type: %s)", moveRequest.ID, moveRequest.MoveType)
			log.Printf("[DIAGNOSTIC] Task type: %s", taskType)

			if isMoveDropoff {
				// Finalize the move ATOMICALLY with the dropoff's completion. A
				// failure now fails the request and rolls back the completion —
				// this used to be swallowed ("don't fail — just log"), leaving a
				// completed dropoff with a move that never finalized.
				log.Printf("[DIAGNOSTIC] This is the DROPOFF - finalizing move request")
				if err = applyMoveCompletion(tx, moveRequest, moveFin, now); err != nil {
					log.Printf("[DIAGNOSTIC] ❌ Error finalizing move request: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to finalize move request")
					return
				}
			} else {
				log.Printf("[DIAGNOSTIC] This is the PICKUP - move request remains in_progress")
			}
		} else if taskType == "placement" {
			// PLACEMENT task - either create new bin (potential_location) or redeploy warehouse bin
			log.Printf("[DIAGNOSTIC] 📍 Detected PLACEMENT task - determining source...")

			// Get placement details from route_tasks table
			var potentialLocationID *string
			var placementSource *string
			var taskBinID *string
			err = db.QueryRow(`
				SELECT potential_location_id, placement_source, bin_id
				FROM route_tasks
				WHERE id = $1
			`, taskID).Scan(&potentialLocationID, &placementSource, &taskBinID)
			if err != nil {
				log.Printf("[DIAGNOSTIC] ❌ Error fetching placement details: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to retrieve placement details")
				return
			}

			src := "potential_location"
			if placementSource != nil {
				src = *placementSource
			}
			log.Printf("[DIAGNOSTIC]    placement_source: %s", src)

			if src == "warehouse" {
				// ── WAREHOUSE REDEPLOYMENT (LEGACY) ────────────────────────────────────
				// DEPRECATED: new shifts no longer create placement tasks with
				// placement_source='warehouse' — the shift builder now mints a real
				// redeployment move request with a pickup/dropoff pair, completed via
				// the move branch above. This branch remains only for in-flight shifts
				// created before the consolidation; remove once none exist.
				// The bin already exists (in_storage). Update its location + status → active.
				log.Printf("[DIAGNOSTIC] 🏭 Warehouse redeployment path (legacy placement task)")

				if taskBinID == nil || *taskBinID == "" {
					log.Printf("[DIAGNOSTIC] ❌ No bin_id on warehouse placement task")
					utils.RespondError(w, http.StatusBadRequest, "bin_id is required for warehouse deployment tasks")
					return
				}

				// Use task coordinates + address as the deployment destination
				var destLat, destLon float64
				var destAddr *string
				db.QueryRow(`SELECT latitude, longitude, address FROM route_tasks WHERE id = $1`, taskID).Scan(&destLat, &destLon, &destAddr)
				addrVal := ""
				if destAddr != nil {
					addrVal = *destAddr
				}

				_, err = tx.Exec(`
					UPDATE bins
					SET status = 'active',
					    current_street = $1,
					    latitude = $2,
					    longitude = $3,
					    last_checked_at = $4,
					    updated_at = $4
					WHERE id = $5
				`, addrVal, destLat, destLon, now, *taskBinID)
				if err != nil {
					log.Printf("[DIAGNOSTIC] ❌ Error redeploying warehouse bin: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to redeploy bin")
					return
				}
				log.Printf("[DIAGNOSTIC] ✅ Bin %s redeployed to active at (%f, %f)", *taskBinID, destLat, destLon)

				// Broadcast bin_redeployed event to managers
				if hub != nil {
					hub.BroadcastToRole("manager", websocket.Message{
						UserID: "",
						Data: map[string]interface{}{
							"type":     "bin_redeployed",
							"bin_id":   *taskBinID,
							"address":  addrVal,
							"status":   "active",
							"shift_id": shift.ID,
						},
					})
					log.Printf("[DIAGNOSTIC] 📡 Broadcast bin_redeployed event to managers")
				}

				// Publish bin_redeployed via Centrifugo
				if centrifugoClient != nil {
					if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "bin_redeployed", map[string]interface{}{
						"bin_id":   *taskBinID,
						"address":  addrVal,
						"status":   "active",
						"shift_id": shift.ID,
					}); pubErr != nil {
						log.Printf("[DIAGNOSTIC] ⚠️  Failed to publish bin_redeployed to Centrifugo: %v", pubErr)
					}
				}
			} else {
				// ── POTENTIAL LOCATION PLACEMENT ──────────────────────────────────────
				// Create a brand new bin from a potential location
				if potentialLocationID == nil {
					log.Printf("[DIAGNOSTIC] ❌ Missing potential_location_id for potential_location placement")
					utils.RespondError(w, http.StatusInternalServerError, "Failed to retrieve placement location")
					return
				}

				log.Printf("[DIAGNOSTIC]    Potential Location ID: %s", *potentialLocationID)

				// Fetch potential location details
				var potentialLocation models.PotentialLocation
				err = db.Get(&potentialLocation, "SELECT * FROM potential_locations WHERE id = $1", *potentialLocationID)
				if err != nil {
					log.Printf("[DIAGNOSTIC] ❌ Error fetching potential location: %v", err)
				} else {
					log.Printf("[DIAGNOSTIC]    Location: %s, %s %s", potentialLocation.Street, potentialLocation.City, potentialLocation.Zip)

					// Create new bin with pre-assigned bin_number
					newBinID := uuid.New().String()
					binInsertQuery := `
						INSERT INTO bins (
							id, bin_number, current_street, city, zip,
							latitude, longitude, status, fill_percentage,
							last_checked_at, created_by_user_id, placement_photo_url, created_at, updated_at
						) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
					`

					// Use driver-provided bin number (required)
					actualBinNumber := req.NewBinNumber
					log.Printf("[DIAGNOSTIC] Using driver-provided bin number: %d", actualBinNumber)
					if actualBinNumber == 0 {
						log.Printf("[DIAGNOSTIC] ❌ Driver did not provide a bin number")
						utils.RespondError(w, http.StatusBadRequest, "Bin number is required for placement tasks")
						return
					}

					// Check if this bin number is already taken — allow duplicates for now
					var binAlreadyExists bool
					db.QueryRow(`SELECT EXISTS(SELECT 1 FROM bins WHERE bin_number = $1)`, actualBinNumber).Scan(&binAlreadyExists)
					if binAlreadyExists {
						log.Printf("[DIAGNOSTIC] ⚠️ Bin #%d already exists — allowing duplicate", actualBinNumber)
					}

					_, err = tx.Exec(
						binInsertQuery,
						newBinID,
						actualBinNumber,
						potentialLocation.Street,
						potentialLocation.City,
						potentialLocation.Zip,
						potentialLocation.Latitude,
						potentialLocation.Longitude,
						"active",
						0, // New bins start at 0% fill
						now,
						userClaims.UserID,
						req.PhotoUrl, // Placement photo from driver
						now,
						now,
					)

					if err != nil {
						log.Printf("[DIAGNOSTIC] ❌ Error creating bin: %v", err)
						utils.RespondError(w, http.StatusInternalServerError, "Failed to create bin, please try again")
						return
					} else {
						log.Printf("[DIAGNOSTIC] ✅ Created new Bin #%d (ID: %s)", actualBinNumber, newBinID)

						// Save the bin ID for check record insertion later
						placementBinID = &newBinID

						// Update potential_location record (mark as converted via shift)
						_, err = tx.Exec(`
							UPDATE potential_locations
							SET converted_to_bin_id = $1,
								converted_at = $2,
								converted_by_user_id = $3,
								converted_via_shift_id = $4,
								updated_at = $5
							WHERE id = $6
						`, newBinID, now, userClaims.UserID, shift.ID, now, *potentialLocationID)

						if err != nil {
							log.Printf("[DIAGNOSTIC] ❌ Error updating potential_location: %v", err)
						} else {
							log.Printf("[DIAGNOSTIC] ✅ Potential location marked as converted (via shift %s)", shift.ID)

							// Broadcast WebSocket event to managers
							if hub != nil {
								binCreatedMsg := websocket.Message{
									UserID: "",
									Data: map[string]interface{}{
										"type":       "bin_created",
										"bin_id":     newBinID,
										"bin_number": actualBinNumber,
										"street":     potentialLocation.Street,
										"city":       potentialLocation.City,
										"zip":        potentialLocation.Zip,
										"status":     "active",
										"created_by": "driver_placement",
										"shift_id":   shift.ID,
									},
								}
								hub.BroadcastToRole("manager", binCreatedMsg)
								log.Printf("[DIAGNOSTIC] 📡 Broadcast bin_created event to managers")
							}

							// Publish bin_created via Centrifugo
							if centrifugoClient != nil {
								if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "bin_created", map[string]interface{}{
									"bin_id":     newBinID,
									"bin_number": actualBinNumber,
									"street":     potentialLocation.Street,
									"city":       potentialLocation.City,
									"zip":        potentialLocation.Zip,
									"status":     "active",
									"created_by": "driver_placement",
									"shift_id":   shift.ID,
								}); pubErr != nil {
									log.Printf("[DIAGNOSTIC] ⚠️  Failed to publish bin_created to Centrifugo: %v", pubErr)
								}
							}

							// Also publish potential_location_converted via Centrifugo so the
							// manager dashboard removes this location from the selector in real-time
							if centrifugoClient != nil {
								plData := map[string]interface{}{
									"location_id": *potentialLocationID,
									"bin_id":      newBinID,
									"bin_number":  actualBinNumber,
									"street":      potentialLocation.Street,
									"city":        potentialLocation.City,
									"zip":         potentialLocation.Zip,
									"shift_id":    shift.ID,
								}
								if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "potential_location_converted", plData); pubErr != nil {
									log.Printf("[DIAGNOSTIC] Failed to publish potential_location_converted via Centrifugo: %v", pubErr)
								} else {
									log.Printf("[DIAGNOSTIC] Published potential_location_converted via Centrifugo (location_id: %s)", *potentialLocationID)
								}
							}
						}
					}
				}
			}
		} else {
			// Regular bin check - update fill percentage and last_checked_at
			if req.UpdatedFillPercentage != nil {
				log.Printf("[DIAGNOSTIC] 📝 Updating bin fill percentage and last_checked_at in bins table...")
				binUpdateQuery := `UPDATE bins
								   SET fill_percentage = $1,
								       last_checked_at = $2,
								       updated_at = $2
								   WHERE id = $3`

				_, err = tx.Exec(binUpdateQuery, *req.UpdatedFillPercentage, now, req.BinID)
				if err != nil {
					// In-tx now: a failed statement aborts the transaction, so this
					// can no longer be swallowed — roll back the completion with it.
					log.Printf("[DIAGNOSTIC] ❌ Error updating bin fill percentage: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to update bin")
					return
				} else {
					log.Printf("[DIAGNOSTIC] ✅ Bin fill percentage updated to %d%% and last_checked_at set to %d", *req.UpdatedFillPercentage, now)
				}
			} else {
				// Even without fill percentage, update last_checked_at
				log.Printf("[DIAGNOSTIC] 📝 Updating last_checked_at (no fill percentage due to incident)...")
				_, err = tx.Exec(`UPDATE bins SET last_checked_at = $1, updated_at = $1 WHERE id = $2`, now, req.BinID)
				if err != nil {
					log.Printf("[DIAGNOSTIC] ❌ Error updating last_checked_at: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to update bin")
					return
				} else {
					log.Printf("[DIAGNOSTIC] ✅ last_checked_at set to %d", now)
				}
			}
		}

		// Insert check record into checks table (only for bin-related stops, not warehouse)
		var checkID *int

		// Determine which bin ID to use for check record
		binIDForCheck := req.BinID
		if placementBinID != nil {
			// For placement tasks, use the newly created bin ID
			binIDForCheck = *placementBinID
			log.Printf("[DIAGNOSTIC] 📦 Using newly created bin ID for placement check record: %s", binIDForCheck)
		}

		if binIDForCheck != "" && taskType != "warehouse_stop" && taskType != "service" {
			log.Printf("[DIAGNOSTIC] 📝 Inserting check record into checks table...")
			log.Printf("[DIAGNOSTIC] 💾 CHECKS TABLE INSERT - fill_percentage value:")
			if req.UpdatedFillPercentage != nil {
				log.Printf("[DIAGNOSTIC]    Inserting fill_percentage: %d%%", *req.UpdatedFillPercentage)
			} else {
				log.Printf("[DIAGNOSTIC]    Inserting fill_percentage: NULL")
			}

			// The address snapshot reads the bins row as of THIS point in the tx:
			// collections capture the bin's current spot; a move dropoff captures
			// the destination (applyMoveCompletion already relocated the bin
			// above) — i.e. wherever the driver physically stood for the check.
			checkQuery := `INSERT INTO checks (bin_id, checked_from, fill_percentage, checked_on, checked_by, photo_url, move_request_id, shift_id,
											   bin_address_snapshot, bin_latitude_snapshot, bin_longitude_snapshot)
						   VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
								   (SELECT CONCAT_WS(', ', NULLIF(current_street,''), NULLIF(city,''), NULLIF(zip,'')) FROM bins WHERE id = $1),
								   (SELECT latitude FROM bins WHERE id = $1),
								   (SELECT longitude FROM bins WHERE id = $1))
						   RETURNING id`

			var returnedID int
			err = tx.QueryRow(checkQuery, binIDForCheck, "shift", req.UpdatedFillPercentage, now, userClaims.UserID, req.PhotoUrl, req.MoveRequestID, shift.ID).Scan(&returnedID)
			if err != nil {
				// In-tx now: a failed statement aborts the transaction — fatal.
				log.Printf("[DIAGNOSTIC] ❌ Error inserting check record: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to record check")
				return
			} else {
				checkID = &returnedID
				if req.PhotoUrl != nil {
					log.Printf("[DIAGNOSTIC] ✅ Check record inserted with photo_url (ID: %d)", returnedID)
				} else {
					log.Printf("[DIAGNOSTIC] ✅ Check record inserted without photo (ID: %d)", returnedID)
				}

				// Auto-resolve any pending check recommendations for this bin
				autoResolveCheckRecommendation(db, binIDForCheck, userClaims.UserID, now)
			}
		} else {
			log.Printf("[DIAGNOSTIC] ⏭️  Skipping check record insert (warehouse stop or no bin_id)")
		}

		// Recompute the shift's counts ON THE SAME TX — a pool-based recompute here
		// would not see the uncommitted completion and would persist stale counts.
		if rerr := itinerary.RecomputeShiftCounts(tx, shift.ID, now); rerr != nil {
			log.Printf("❌ Failed to recompute shift counts: %v", rerr)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to update shift counts")
			return
		}

		// ── COMMIT: task completion + branch writes + checks + counts, atomically ──
		if err = tx.Commit(); err != nil {
			log.Printf("❌ Error committing completion: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to complete task")
			return
		}

		// ── Post-commit, best-effort side effects ────────────────────────────────
		if isMoveDropoff {
			moveCompletionSideEffects(db, hub, centrifugoClient, moveRequest, now, userClaims.UserID)
		}

		// Create incident if reported
		var createdIncidentID *string
		if req.HasIncident && checkID != nil {
			log.Printf("[DIAGNOSTIC] 🚨 Creating incident report for %s...", *req.IncidentType)

			// Get bin details for zone creation
			var bin models.Bin
			err = db.Get(&bin, "SELECT * FROM bins WHERE id = $1", binIDForCheck)
			if err != nil {
				log.Printf("[DIAGNOSTIC] ❌ Error fetching bin details: %v", err)
			} else {
				log.Printf("[DIAGNOSTIC]    Bin found: %s, %s", bin.CurrentStreet, bin.City)
				log.Printf("[DIAGNOSTIC]    Latitude: %v, Longitude: %v", bin.Latitude, bin.Longitude)
			}

			// relocation_request is an operational note, not a location safety signal — skip zone creation
			if err == nil && bin.Latitude != nil && bin.Longitude != nil && *req.IncidentType != "relocation_request" {
				binIDCopy := binIDForCheck
				shiftIDCopy := shift.ID
				zoneName := fmt.Sprintf("%s - %s", bin.CurrentStreet, bin.City)
				driverShiftSource := "driver_shift"
				incidentID, incidentErr := createZoneAndIncident(
					db,
					centrifugoClient,
					*bin.Latitude, *bin.Longitude,
					zoneName,
					*req.IncidentType,
					&binIDCopy,
					userClaims.UserID,
					req.IncidentDescription,
					req.IncidentPhotoUrl,
					&shiftIDCopy,
					checkID,
					nil, nil,
					false,
					now,
					&driverShiftSource, // source
					nil,                // moveRequestID
				)
				if incidentErr != nil {
					log.Printf("[DIAGNOSTIC] ❌ Error creating zone/incident: %v", incidentErr)
				} else {
					createdIncidentID = &incidentID
					log.Printf("[DIAGNOSTIC] ✅ Incident created (ID: %s) and linked to check ID %d", incidentID, *checkID)
				}
			} else if err != nil {
				log.Printf("[DIAGNOSTIC] ⚠️  Could not create incident: failed to fetch bin")
			} else {
				log.Printf("[DIAGNOSTIC] ⚠️  Could not create incident: bin has no coordinates (lat: %v, lng: %v)", bin.Latitude, bin.Longitude)
			}

			// If the incident type is "missing", flip the bin status and broadcast in real-time
			if *req.IncidentType == "missing" {
				if _, updateErr := db.Exec(
					`UPDATE bins SET status = 'missing', updated_at = $1 WHERE id = $2`,
					now, binIDForCheck,
				); updateErr != nil {
					log.Printf("[DIAGNOSTIC] ⚠️  Failed to mark bin %s as missing: %v", binIDForCheck, updateErr)
				} else {
					log.Printf("[DIAGNOSTIC] 🔍 Bin %s marked as missing", binIDForCheck)
					if centrifugoClient != nil {
						var updatedBin models.Bin
						if fetchErr := db.Get(&updatedBin, "SELECT * FROM bins WHERE id = $1", binIDForCheck); fetchErr == nil {
							if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "bin_updated", updatedBin); pubErr != nil {
								log.Printf("[DIAGNOSTIC] ⚠️  Centrifugo bin_updated publish failed: %v", pubErr)
							}
						}
					}
				}
			}
		}

		// Get updated shift
		db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shift.ID)

		// Get updated bins list
		log.Printf("🔄 Fetching shift tasks via getShiftTasksWithDetails...")
		bins, err := getShiftTasksWithDetails(db, shift.ID)
		if err != nil {
			log.Printf("❌ Error fetching route bins: %v", err)
			bins = []models.ShiftBinWithDetails{}
		}
		log.Printf("✅ Fetched %d tasks", len(bins))

		// Calculate LOGICAL bin counts (treating pickup+dropoff as 1)
		logicalTotal, logicalCompleted := calculateLogicalBinCounts(bins)
		log.Printf("[DIAGNOSTIC] 🔢 Logical counts: %d completed / %d total (Physical: %d/%d)",
			logicalCompleted, logicalTotal, shift.CompletedBins, shift.TotalBins)

		// Broadcast WebSocket update with bins
		completeTaskUpdateData := map[string]interface{}{
			"id":             shift.ID,
			"status":         shift.Status,
			"completed_bins": logicalCompleted,
			"total_bins":     logicalTotal,
			"tasks":          bins,
		}
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": completeTaskUpdateData,
		})

		// Publish shift_update via Centrifugo
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishShiftUpdate(r.Context(), shift.ID, map[string]interface{}{
				"type": "shift_update",
				"data": completeTaskUpdateData,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish shift_update to Centrifugo: %v", pubErr)
			}
		}

		log.Printf("[DIAGNOSTIC] ✅ Bin completed: %d/%d (logical)", logicalCompleted, logicalTotal)

		completionPercentage := 0.0
		if logicalTotal > 0 {
			completionPercentage = float64(logicalCompleted) / float64(logicalTotal) * 100
		}

		response := models.CompleteBinResponse{
			CompletedBins:        logicalCompleted,
			TotalBins:            logicalTotal,
			CompletionPercentage: completionPercentage,
			CheckID:              checkID,
			IncidentID:           createdIncidentID,
		}

		log.Printf("[DIAGNOSTIC] 📤 RESPONSE: 200 OK")
		log.Printf("[DIAGNOSTIC]    Completed: %d/%d (%.1f%%) [LOGICAL COUNTS]", logicalCompleted, logicalTotal, completionPercentage)
		log.Printf("[DIAGNOSTIC]    Photo uploaded: %v", req.PhotoUrl != nil)
		if checkID != nil {
			log.Printf("[DIAGNOSTIC]    Check ID: %d (available for incident linking)", *checkID)
		}
		if createdIncidentID != nil {
			log.Printf("[DIAGNOSTIC]    Incident ID: %s (incident successfully created)", *createdIncidentID)
		}
		log.Printf("[DIAGNOSTIC] ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    response,
		})
	}
}

// ReoptimizeActiveShift re-optimizes an active shift's remaining tasks using current traffic
// skipGates: if true, skips time-based gates (for manager-initiated changes)

func calculateZoneDistance(lat1, lon1, lat2, lon2 float64) float64 {
	return geo.HaversineMeters(lat1, lon1, lat2, lon2)
}

// moveFinalization carries the pre-resolved inputs a move completion needs:
// destination status, warehouse config, and the reverse-geocoded destination.
// Resolved BEFORE the completion tx (the HERE geocode is a network call that
// must never be held open across a transaction).
type moveFinalization struct {
	isStore, isRelocation      bool
	storeStatus                string
	warehouse                  models.WarehouseLocation
	warehouseOK                bool
	relStreet, relCity, relZip string
}

// prepareMoveCompletion resolves everything applyMoveCompletion will write —
// config reads + reverse geocoding — on the pool, pre-tx.
func prepareMoveCompletion(db *sqlx.DB, moveRequest models.BinMoveRequest) moveFinalization {
	log.Printf("[MOVE] 🚚 Preparing move request completion (type: %s)", moveRequest.MoveType)

	isStore := moveRequest.MoveType == "store" || moveRequest.MoveType == "pickup_only"
	isRelocation := moveRequest.MoveType == "relocation" || moveRequest.MoveType == "redeployment"

	// ── Resolve external/read-only inputs BEFORE the tx ──────────────────────────
	// (No HERE network call or config read is held open across the transaction.)

	// store/pickup_only: destination status + warehouse coordinates
	var storeStatus string
	var warehouse models.WarehouseLocation
	var warehouseOK bool
	if isStore {
		storeStatus = "in_storage" // default
		if moveRequest.DisposalAction != nil && *moveRequest.DisposalAction == "retire" {
			storeStatus = "retired"
		}
		var warehouseJSON []byte
		if whErr := db.QueryRow(`SELECT value FROM config WHERE key = 'warehouse_location'`).Scan(&warehouseJSON); whErr == nil {
			if json.Unmarshal(warehouseJSON, &warehouse) == nil {
				warehouseOK = true
			}
		}
		log.Printf("[MOVE]    → store move: status=%s, warehouseConfig=%v", storeStatus, warehouseOK)
	}

	// relocation/redeployment: reverse-geocode the destination (network → must be pre-tx)
	relStreet, relCity, relZip := "", "", ""
	if isRelocation {
		if moveRequest.NewAddress != nil {
			relStreet = *moveRequest.NewAddress
		}
		if moveRequest.NewLatitude != nil && moveRequest.NewLongitude != nil {
			geocoder := services.NewHEREGeocodingService(HereAPIKey)
			if s, c, z, rgErr := geocoder.ReverseGeocode(*moveRequest.NewLatitude, *moveRequest.NewLongitude); rgErr != nil {
				log.Printf("[MOVE] ⚠️  Reverse geocode failed, using move request address: %v", rgErr)
			} else {
				relStreet, relCity, relZip = s, c, z
				log.Printf("[MOVE] ✅ Reverse geocoded: %s, %s %s", relStreet, relCity, relZip)
			}
		}
	}

	return moveFinalization{
		isStore: isStore, isRelocation: isRelocation, storeStatus: storeStatus,
		warehouse: warehouse, warehouseOK: warehouseOK,
		relStreet: relStreet, relCity: relCity, relZip: relZip,
	}
}

// applyMoveCompletion is the move finalization's atomic core — mark the move
// request completed and move the bin — running on the CALLER's transaction,
// atomic with the dropoff task's own completion (closing the historical torn
// write: a completed dropoff whose move never finalized, error swallowed).
func applyMoveCompletion(ext sqlx.Ext, moveRequest models.BinMoveRequest, fin moveFinalization, now int64) error {
	if _, err := ext.Exec(`
		UPDATE bin_move_requests
		SET status = 'completed', completed_at = $1, updated_at = $1
		WHERE id = $2
	`, now, moveRequest.ID); err != nil {
		return fmt.Errorf("failed to complete move request: %w", err)
	}

	if fin.isStore {
		if fin.warehouseOK {
			if _, err := ext.Exec(`
				UPDATE bins
				SET status = $1, latitude = $2, longitude = $3, current_street = $4, city = '', zip = '',
				    fill_percentage = 0, last_checked_at = NULL, updated_at = $5
				WHERE id = $6
			`, fin.storeStatus, fin.warehouse.Latitude, fin.warehouse.Longitude, fin.warehouse.Address, now, moveRequest.BinID); err != nil {
				return fmt.Errorf("failed to update bin status: %w", err)
			}
		} else {
			if _, err := ext.Exec(`UPDATE bins SET status = $1, fill_percentage = 0, last_checked_at = NULL, updated_at = $2 WHERE id = $3`,
				fin.storeStatus, now, moveRequest.BinID); err != nil {
				return fmt.Errorf("failed to update bin status: %w", err)
			}
		}
	} else if fin.isRelocation {
		if _, err := ext.Exec(`
			UPDATE bins
			SET latitude = $1, longitude = $2, current_street = $3, city = $4, zip = $5,
			    status = 'active', updated_at = $6
			WHERE id = $7
		`, moveRequest.NewLatitude, moveRequest.NewLongitude, fin.relStreet, fin.relCity, fin.relZip, now, moveRequest.BinID); err != nil {
			return fmt.Errorf("failed to relocate bin: %w", err)
		}
	}

	return nil
}

// moveCompletionSideEffects runs the best-effort post-commit tail: history,
// dashboard broadcasts, potential-location conversion, the moves record.
func moveCompletionSideEffects(db *sqlx.DB, hub *websocket.Hub, centrifugoClient *centrifugo.Client, moveRequest models.BinMoveRequest, now int64, driverID string) {
	log.Printf("[MOVE] ✅ Move request %s completed + bin %s updated (atomic with task completion)", moveRequest.ID, moveRequest.BinID)
	isRelocation := moveRequest.MoveType == "relocation" || moveRequest.MoveType == "redeployment"

	// History + conversion attribution: the actor is the DRIVER who completed the task.
	// moveRequest.AssignedUserID is NULL once a move is assigned to a shift (assign-to-shift
	// assigns to the shift, not a user — see #31), so prefer the authenticated driver and
	// only fall back to AssignedUserID for a directly user-assigned move.
	completedBy := driverID
	if completedBy == "" && moveRequest.AssignedUserID != nil {
		completedBy = *moveRequest.AssignedUserID
	}
	if completedBy != "" {
		var driverName string
		if dErr := db.Get(&driverName, `SELECT name FROM users WHERE id = $1`, completedBy); dErr != nil {
			driverName = "Unknown Driver"
		}
		if lErr := moverequest.LogCompleted(db, moveRequest.ID, completedBy, driverName, "driver", moveRequest.Status); lErr != nil {
			log.Printf("Warning: Failed to log move request completion: %v", lErr)
		}
	} else {
		log.Printf("Warning: Move request completed without a resolvable actor ID")
	}

	// Broadcast move request completion to dashboard
	moveCompletedData := map[string]interface{}{
		"move_request_id": moveRequest.ID,
		"bin_id":          moveRequest.BinID,
		"new_status":      "completed",
		"completed_at":    now,
	}
	hub.BroadcastToRole("admin", map[string]interface{}{"type": "move_request_status_updated", "data": moveCompletedData})
	hub.BroadcastToRole("manager", map[string]interface{}{"type": "move_request_status_updated", "data": moveCompletedData})
	if centrifugoClient != nil {
		if pubErr := centrifugoClient.PublishCompanyEvent(context.Background(), "move_request_status_updated", moveCompletedData); pubErr != nil {
			log.Printf("⚠️  Failed to publish move_request_status_updated to Centrifugo: %v", pubErr)
		}
	}

	if isRelocation {
		// If this was a redeployment/relocation to a potential location, mark it converted
		if moveRequest.SourcePotentialLocationID != nil {
			var shiftID *string
			if moveRequest.AssignedShiftID != nil {
				shiftID = moveRequest.AssignedShiftID
			}
			if _, plErr := db.Exec(`
				UPDATE potential_locations
				SET converted_to_bin_id = $1, converted_at = $2, converted_by_user_id = $3, converted_via_shift_id = $4, updated_at = $2
				WHERE id = $5
			`, moveRequest.BinID, now, completedBy, shiftID, *moveRequest.SourcePotentialLocationID); plErr != nil {
				log.Printf("[MOVE] ⚠️  Error updating potential location: %v", plErr)
			} else {
				plConvertedData := map[string]interface{}{
					"potential_location_id": *moveRequest.SourcePotentialLocationID,
					"bin_id":                moveRequest.BinID,
					"shift_id":              shiftID,
					"converted_at":          now,
				}
				hub.BroadcastToRole("manager", map[string]interface{}{"type": "potential_location_converted", "data": plConvertedData})
				if centrifugoClient != nil {
					if pubErr := centrifugoClient.PublishCompanyEvent(context.Background(), "potential_location_converted", plConvertedData); pubErr != nil {
						log.Printf("⚠️  Failed to publish potential_location_converted to Centrifugo: %v", pubErr)
					}
				}
			}
		}

		// Record the move in the moves table (best-effort — the move is already committed)
		var fromStreet, fromCity, fromZip, toStreet, toCity, toZip *string
		parseAddr := func(addr string) (*string, *string, *string) {
			parts := strings.Split(addr, ", ")
			if len(parts) < 2 {
				return nil, nil, nil
			}
			street := parts[0]
			cityZipParts := strings.Split(strings.TrimSpace(parts[1]), " ")
			if len(cityZipParts) < 2 {
				return &street, nil, nil
			}
			city := strings.Join(cityZipParts[:len(cityZipParts)-1], " ")
			zip := cityZipParts[len(cityZipParts)-1]
			return &street, &city, &zip
		}
		if moveRequest.OriginalAddress != "" {
			fromStreet, fromCity, fromZip = parseAddr(moveRequest.OriginalAddress)
		}
		newAddr := ""
		if moveRequest.NewAddress != nil {
			newAddr = *moveRequest.NewAddress
			toStreet, toCity, toZip = parseAddr(newAddr)
		}
		if _, mvErr := db.Exec(`
			INSERT INTO moves (
				bin_id, moved_from, moved_to, moved_on,
				move_type, from_street, from_city, from_zip,
				to_street, to_city, to_zip,
				move_request_id, shift_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`, moveRequest.BinID, moveRequest.OriginalAddress, newAddr, now, "shift",
			fromStreet, fromCity, fromZip, toStreet, toCity, toZip,
			moveRequest.ID, moveRequest.AssignedShiftID); mvErr != nil {
			log.Printf("[MOVE] ⚠️  Failed to record move: %v", mvErr)
		}
		log.Printf("[MOVE] ✅ Bin relocated to %s", newAddr)
	}

}

// CancelShift cancels a specific shift
// PUT /api/manager/shifts/:id/cancel
