package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ropacal-backend/internal/models"
	"ropacal-backend/internal/services/optimization"
	"ropacal-backend/pkg/utils"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// previewLocation is an anchor point in the preview response (warehouse / start).
type previewLocation struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Address   string  `json:"address"`
	Source    string  `json:"source,omitempty"` // "warehouse" | "custom"
}

// previewStop is one ordered stop in the previewed route. warehouse_stop rows are
// synthesized from the optimizer's end/warehouse-pickup stops, exactly as the real
// start-shift persistence does — so the manager sees warehouse loads interleaved
// where the driver will actually make them.
type previewStop struct {
	SequenceOrder  int     `json:"sequence_order"`
	Type           string  `json:"type"` // collection|placement|pickup|dropoff|service|warehouse_stop
	Latitude       float64 `json:"latitude"`
	Longitude      float64 `json:"longitude"`
	Address        string  `json:"address"`
	BinNumber      *int    `json:"bin_number,omitempty"`
	NewBinNumber   *int    `json:"new_bin_number,omitempty"`
	FillPercentage *int    `json:"fill_percentage,omitempty"`
	Label          *string `json:"label,omitempty"`
}

// shiftPreviewResponse is the dry-run optimized route for a not-yet-started shift.
type shiftPreviewResponse struct {
	ShiftID                string          `json:"shift_id"`
	OptimizerUsed          string          `json:"optimizer_used"`
	TotalDistanceKm        float64         `json:"total_distance_km"`
	TotalDistanceMiles     float64         `json:"total_distance_miles"`
	TotalDurationSeconds   int             `json:"total_duration_seconds"`
	TotalDurationFormatted string          `json:"total_duration_formatted"`
	EstimatedCompletion    string          `json:"estimated_completion"`
	StartLocation          previewLocation `json:"start_location"`
	Warehouse              previewLocation `json:"warehouse"`
	Stops                  []previewStop   `json:"stops"`
	StopCount              int             `json:"stop_count"`
	Capacity               int             `json:"capacity"` // truck bin capacity used for this run
}

// PreviewShiftOptimization runs the SAME optimizer the driver's start-shift runs,
// read-only, so a manager can preview a scheduled shift's route "as if the driver
// started it" — including the generated warehouse stops and the total time/distance.
// Nothing is persisted.
//
// The one intentional difference from a real start: a not-yet-started shift has no
// live driver GPS, so the vehicle is anchored at the WAREHOUSE (or the shift's custom
// start, if set). When the driver actually starts from their real location the first/
// last leg can shift slightly — the dashboard surfaces this caveat.
//
// GET/POST /api/manager/shifts/{shiftId}/optimize-preview
func PreviewShiftOptimization(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shiftID := chi.URLParam(r, "shiftId")
		if shiftID == "" {
			utils.RespondError(w, http.StatusBadRequest, "Missing shift id")
			return
		}

		var shift models.Shift
		if err := db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shiftID); err != nil {
			utils.RespondError(w, http.StatusNotFound, "Shift not found")
			return
		}

		// Only preview shifts that haven't started. Once a shift is active/paused/
		// ended/cancelled its route is already persisted — the drawer's live task
		// list is the source of truth, not a dry-run.
		switch shift.Status {
		case models.ShiftStatusActive, models.ShiftStatusPaused,
			models.ShiftStatusEnded, models.ShiftStatusCancelled:
			utils.RespondError(w, http.StatusConflict,
				"Shift already started; its route is live — use the task list, not a preview")
			return
		}

		// Warehouse guard (mirrors StartShift): standard shifts must have a warehouse.
		if shift.ShiftType != "custom" && (shift.WarehouseLatitude == nil || shift.WarehouseLongitude == nil) {
			utils.RespondError(w, http.StatusBadRequest, "Warehouse location not configured")
			return
		}

		// Optional request-body override so a manager can preview "what if the truck
		// holds N bins" without changing the shift. Empty body is fine (io.EOF ignored).
		var overrides struct {
			Capacity *int `json:"capacity"`
		}
		_ = json.NewDecoder(r.Body).Decode(&overrides)

		capacity := 4
		if shift.TruckBinCapacity != nil {
			capacity = *shift.TruckBinCapacity
		}
		if overrides.Capacity != nil && *overrides.Capacity > 0 {
			capacity = *overrides.Capacity
			if capacity > 100 {
				capacity = 100 // sane ceiling
			}
		}

		warehouseLat, warehouseLon := 0.0, 0.0
		warehouseAddr := ""
		if shift.WarehouseLatitude != nil {
			warehouseLat = *shift.WarehouseLatitude
		}
		if shift.WarehouseLongitude != nil {
			warehouseLon = *shift.WarehouseLongitude
		}
		if shift.WarehouseAddress != nil {
			warehouseAddr = *shift.WarehouseAddress
		}

		// No live GPS for a scheduled shift → anchor the "driver" at the warehouse.
		// A custom start (if set on the shift) still overrides this inside the core,
		// exactly as it does for the real driver start.
		req, routePtr, err := buildAndOptimizeShiftRoute(
			db, shiftID, capacity,
			warehouseLat, warehouseLon, // driver start = warehouse (no GPS)
			warehouseLat, warehouseLon, warehouseAddr,
			false, // binsPreloaded: preview assumes a cold start
			shift.StartLatitude, shift.StartLongitude, shift.StartAddress,
			shift.EndLatitude, shift.EndLongitude, shift.EndAddress,
		)
		if err != nil {
			utils.RespondError(w, http.StatusInternalServerError,
				fmt.Sprintf("Route optimization failed: %v", err))
			return
		}

		// Start-location source + coords for the response.
		startLoc := previewLocation{Latitude: warehouseLat, Longitude: warehouseLon, Address: warehouseAddr, Source: "warehouse"}
		if shift.StartLatitude != nil && shift.StartLongitude != nil {
			startLoc = previewLocation{Latitude: *shift.StartLatitude, Longitude: *shift.StartLongitude, Source: "custom"}
			if shift.StartAddress != nil {
				startLoc.Address = *shift.StartAddress
			}
		}

		resp := shiftPreviewResponse{
			ShiftID:       shiftID,
			OptimizerUsed: optimization.NewOptimizer().Name(),
			StartLocation: startLoc,
			Warehouse:     previewLocation{Latitude: warehouseLat, Longitude: warehouseLon, Address: warehouseAddr},
			Stops:         []previewStop{},
			Capacity:      capacity,
		}

		// No tasks → empty (but valid) preview.
		if req == nil || routePtr == nil {
			utils.RespondJSON(w, http.StatusOK, resp)
			return
		}
		route := *routePtr

		// Build lookup maps from the SAME request the persistence half reads, so the
		// preview's stop classification matches start-shift exactly (no drift).
		placementNewBin := make(map[string]int) // placement.ID → new bin #
		for _, p := range req.Placements {
			placementNewBin[p.ID] = p.NewBinNumber
		}
		moveBin := make(map[string]int) // moveReq.ID → bin #
		for _, m := range req.MoveRequests {
			moveBin[m.ID] = m.BinNumber
		}
		collBin := make(map[string]int) // collection.ID → bin #
		collFill := make(map[string]int)
		for _, c := range req.Collections {
			collBin[c.ID] = c.BinNumber
			collFill[c.ID] = c.FillPercentage
		}
		svcLabel := make(map[string]string) // service.ID → label
		for _, s := range req.ServiceTasks {
			svcLabel[s.ID] = s.Label
		}

		seq := 1
		for _, stop := range route.Stops {
			if stop.Type == optimization.StopTypeStart {
				continue // the start anchor isn't a task
			}

			ps := previewStop{
				SequenceOrder: seq,
				Latitude:      stop.Latitude,
				Longitude:     stop.Longitude,
				Address:       stop.Address,
			}

			switch stop.Type {
			case optimization.StopTypeEnd:
				ps.Type = "warehouse_stop"

			case optimization.StopTypePickup:
				if stop.PlacementID != "" {
					// Placement pickup at the warehouse (load the new bin) → a warehouse stop.
					ps.Type = "warehouse_stop"
				} else if stop.MoveRequestID != "" {
					ps.Type = "pickup"
					id := strings.TrimPrefix(stop.MoveRequestID, "move-")
					if n, ok := moveBin[id]; ok {
						ps.BinNumber = intPtr(n)
					}
				}

			case optimization.StopTypeDropoff, optimization.StopTypePlacement:
				if stop.PlacementID != "" {
					ps.Type = "placement"
					id := strings.TrimPrefix(stop.PlacementID, "placement-")
					if n, ok := placementNewBin[id]; ok {
						ps.NewBinNumber = intPtr(n)
					}
				} else if stop.MoveRequestID != "" {
					ps.Type = "dropoff"
					id := strings.TrimPrefix(stop.MoveRequestID, "move-")
					if n, ok := moveBin[id]; ok {
						ps.BinNumber = intPtr(n)
					}
				}

			case optimization.StopTypeCollection, "service":
				// Service tasks and collections both arrive here; the CollectionID
				// prefix disambiguates ("service-…" vs "collection-…"), same rule as
				// the persistence switch.
				if strings.HasPrefix(stop.CollectionID, "service-") {
					id := strings.TrimPrefix(stop.CollectionID, "service-")
					if lbl, ok := svcLabel[id]; ok {
						ps.Type = "service"
						l := lbl
						ps.Label = &l
						break
					}
				}
				ps.Type = "collection"
				id := strings.TrimPrefix(stop.CollectionID, "collection-")
				if n, ok := collBin[id]; ok {
					ps.BinNumber = intPtr(n)
				}
				if f, ok := collFill[id]; ok {
					ps.FillPercentage = intPtr(f)
				}
			}

			if ps.Type == "" {
				// Unknown stop type — surface it rather than drop it silently.
				ps.Type = string(stop.Type)
			}
			resp.Stops = append(resp.Stops, ps)
			seq++
		}

		// Totals — same math as the start-shift metadata (shift_optimization.go).
		resp.TotalDistanceKm = route.TotalDistance / 1000.0
		resp.TotalDistanceMiles = route.TotalDistance / 1609.34
		resp.TotalDurationSeconds = route.TotalDuration
		resp.TotalDurationFormatted = formatDuration(route.TotalDuration)
		resp.EstimatedCompletion = time.Now().Add(time.Duration(route.TotalDuration) * time.Second).Format(time.RFC3339)
		resp.StopCount = len(resp.Stops)

		utils.RespondJSON(w, http.StatusOK, resp)
	}
}

// formatDuration renders seconds as "Xh Ym" (or "Ym") — identical to the start-shift
// optimization-metadata formatter.
func formatDuration(totalSeconds int) string {
	hours := totalSeconds / 3600
	mins := (totalSeconds % 3600) / 60
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}
