package handlers

import (
	"net/http"
	"strconv"

	"ropacal-backend/internal/geo"
	"ropacal-backend/pkg/utils"
)

// boundaryStore holds the compiled-in city boundaries (TIGER CA places). Set
// once at startup by SetBoundaryStore; read-only thereafter, so the recommender
// and the HTTP handler share it without locking. nil until loaded (or if the
// asset fails to parse) — every reader treats nil as "no polygon, fall back".
var boundaryStore *geo.BoundaryStore

// SetBoundaryStore wires the loaded boundary index in from main.
func SetBoundaryStore(s *geo.BoundaryStore) { boundaryStore = s }

// BoundaryCount reports how many city boundaries are loaded (0 if the asset
// failed to load) — surfaced on /health for live deploy verification.
func BoundaryCount() int {
	if boundaryStore == nil {
		return 0
	}
	return boundaryStore.Count()
}

// lookupBoundary resolves a picked area to its true city polygon, or nil.
// Central helper so the endpoint and the recommender apply identical rules.
func lookupBoundary(name, typ string, lat, lng float64) *geo.Boundary {
	if boundaryStore == nil {
		return nil
	}
	return boundaryStore.Lookup(name, typ, lat, lng)
}

// GetAreaBoundary serves one city's true legal outline (GeoJSON) so the map can
// draw the real shape instead of the search rectangle. Districts, counties, and
// unknown names return found=false — the client keeps its bbox+hex fallback.
//
//	GET /api/areas/boundary?name=San+Jose&type=city&lat=37.33&lng=-121.89
func GetAreaBoundary() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			utils.RespondError(w, http.StatusBadRequest, "name is required")
			return
		}
		typ := r.URL.Query().Get("type")
		// Coordinates are REQUIRED, not optional: they arm the geographic
		// sanity check in Lookup that stops a bare `?name=Brentwood` from
		// resolving to a same-named city 400 mi from where the user picked.
		// Without them (and with no/unknown type) both guards would be off.
		latStr, lngStr := r.URL.Query().Get("lat"), r.URL.Query().Get("lng")
		lat, errLat := strconv.ParseFloat(latStr, 64)
		lng, errLng := strconv.ParseFloat(lngStr, 64)
		if latStr == "" || lngStr == "" || errLat != nil || errLng != nil {
			utils.RespondError(w, http.StatusBadRequest, "lat and lng are required")
			return
		}

		b := lookupBoundary(name, typ, lat, lng)
		if b == nil {
			utils.JSON(w, http.StatusOK, map[string]any{"found": false})
			return
		}
		utils.JSON(w, http.StatusOK, map[string]any{
			"found":    true,
			"name":     b.Name,
			"namelsad": b.NameLSAD,
			"type":     b.Type,
			"bbox":     b.BBox,
			"geometry": b.GeometryJSON(),
		})
	}
}
