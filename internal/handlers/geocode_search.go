package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/orgdb"
	"ropacal-backend/pkg/utils"
)

// GET /api/geocode/search?q=... — area autocomplete for the dashboard picker.
//
// TWO PROBLEMS THIS SOLVES, which is why it is an endpoint rather than a
// one-line change in the browser.
//
//  1. COUNTRY. The dashboard called HERE directly with `q=<text>, USA` and
//     `in=countryCode:USA`. A Canadian tenant typing "Brampton" got "Brampton
//     Twp, MI, United States" — Brampton, Ontario was excluded by construction.
//     The correct country is a property of the ORGANIZATION, which the browser
//     has no authoritative way to know; the server does.
//
//  2. THE API KEY. That call used NEXT_PUBLIC_HERE_API_KEY, which by definition
//     ships inside the JavaScript bundle. Anyone opening devtools on the
//     dashboard could read it and spend the HERE quota. Proxying through here
//     keeps the key server-side, where HERE_API_KEY already lives.
//
// Results are scoped by the caller's org country AND ranked by distance from
// its warehouse, so "Brentwood" still resolves to Brentwood CA for a Bay Area
// tenant while "Windsor" resolves to Windsor ON for a Toronto one.

type geocodeSearchResult struct {
	Label string      `json:"label"`
	Lat   float64     `json:"lat"`
	Lng   float64     `json:"lng"`
	BBox  *[4]float64 `json:"bbox,omitempty"`
	Type  string      `json:"type"`
}

// GeocodeSearch proxies HERE's geocode endpoint with organization scoping.
func GeocodeSearch(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if len(q) < 2 {
			// Match the picker's own minimum so a keystroke never costs a HERE call.
			utils.RespondJSON(w, http.StatusOK, map[string]any{"results": []geocodeSearchResult{}})
			return
		}
		if HereAPIKey == "" {
			log.Printf("⚠️  [Geocode] HERE_API_KEY is not set — area search disabled")
			utils.RespondError(w, http.StatusServiceUnavailable, "geocoding is not configured")
			return
		}

		db := orgdb.From(r)
		scope := scopeForOrg(db)

		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, hereGeocodeURL(q, 6, scope), nil)
		if err != nil {
			utils.RespondError(w, http.StatusInternalServerError, "could not build geocode request")
			return
		}
		client := &http.Client{Timeout: 6 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("⚠️  [Geocode] HERE request failed for %q: %v", q, err)
			utils.RespondError(w, http.StatusBadGateway, "geocoding service unavailable")
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			utils.RespondError(w, http.StatusBadGateway, "could not read geocoding response")
			return
		}

		var parsed struct {
			Items []struct {
				Title      string `json:"title"`
				ResultType string `json:"resultType"`
				// HERE v7: districts arrive as resultType=locality with
				// localityType=district; counties and states as
				// administrativeArea with administrativeAreaType.
				LocalityType           string `json:"localityType"`
				AdministrativeAreaType string `json:"administrativeAreaType"`
				Position               struct {
					Lat float64 `json:"lat"`
					Lng float64 `json:"lng"`
				} `json:"position"`
				MapView *struct {
					West  float64 `json:"west"`
					South float64 `json:"south"`
					East  float64 `json:"east"`
					North float64 `json:"north"`
				} `json:"mapView"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			log.Printf("⚠️  [Geocode] unparseable HERE response for %q: %v", q, err)
			utils.RespondError(w, http.StatusBadGateway, "could not parse geocoding response")
			return
		}

		out := make([]geocodeSearchResult, 0, len(parsed.Items))
		seen := make(map[string]bool, len(parsed.Items))
		for _, it := range parsed.Items {
			if it.Title == "" || seen[it.Title] {
				continue
			}
			// Filtering lives here rather than in the browser so the rule exists
			// ONCE. The dashboard previously reimplemented these same checks
			// against raw HERE fields, which meant the two could drift.
			//
			// A specific address is a legitimate target — the recommender sweeps
			// a radius around the point — so streets and house numbers stay.
			switch it.ResultType {
			case "locality", "administrativeArea", "street", "houseNumber", "address", "intersection":
			default:
				continue
			}
			// States and countries are not placement targets; postal codes are
			// not areas a human picks on a map.
			if it.AdministrativeAreaType == "state" || it.AdministrativeAreaType == "country" {
				continue
			}
			if it.LocalityType == "postalCode" {
				continue
			}
			if it.Position.Lat == 0 && it.Position.Lng == 0 {
				continue
			}
			seen[it.Title] = true
			// Prefer the most specific type available, matching what the picker
			// displays: localityType ("district") beats resultType ("locality").
			typ := it.LocalityType
			if typ == "" {
				typ = it.AdministrativeAreaType
			}
			if typ == "" {
				typ = it.ResultType
			}
			res := geocodeSearchResult{
				Label: it.Title, Lat: it.Position.Lat, Lng: it.Position.Lng,
				Type: typ,
			}
			if mv := it.MapView; mv != nil {
				res.BBox = &[4]float64{mv.West, mv.South, mv.East, mv.North}
			}
			out = append(out, res)
		}

		log.Printf("🔎 [Geocode] %q → %d results (country=%s, biased to %.4f,%.4f)",
			q, len(out), scope.Country, scope.Lat, scope.Lng)
		utils.RespondJSON(w, http.StatusOK, map[string]any{"results": out})
	}
}
