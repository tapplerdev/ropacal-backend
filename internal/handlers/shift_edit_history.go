package handlers

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/orgdb"
	"ropacal-backend/pkg/utils"
)

// GetShiftEditHistory returns a shift's edit timeline, oldest first: created,
// tasks added and removed, driver reassigned — who did it, and why.
//
// GET /api/manager/shifts/{shiftId}/edit-history
//
// A shift with no events reads as an empty list, not a 404. Shifts created
// before the timeline existed have none, and under RLS another organization's
// shift is indistinguishable from a missing one anyway.
func GetShiftEditHistory(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		shiftID := chi.URLParam(r, "shiftId")

		edits, err := itinerary.ListEdits(db, shiftID)
		if err != nil {
			log.Printf("❌ [ShiftEditHistory] shift %s: %v", shiftID, err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch shift edit history")
			return
		}
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    edits,
		})
	}
}
