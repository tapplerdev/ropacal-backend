package handlers

import (
	"encoding/json"
	"net/http"

	"ropacal-backend/pkg/utils"
)

// decodeJSON decodes a REQUIRED request body into dst. On failure it writes a
// 400 and returns false, so callers can write:
//
//	if !decodeJSON(w, r, &req) { return }
//
// Do not use this for optional bodies — an empty body yields io.EOF, which it
// treats as a 400.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
		return false
	}
	return true
}
