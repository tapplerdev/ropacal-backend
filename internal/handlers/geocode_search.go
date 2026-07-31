package handlers

import (
	"context"
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

// candidateLimit is how many rows we ask HERE for. Larger than what the picker
// shows, because rankGeocodeResults can only reorder what it was given.
const candidateLimit = 20

// resultLimit is how many survive to the dropdown.
const resultLimit = 8

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

		out, err := fetchGeocodeAreas(r.Context(), q, scope)
		if err != nil {
			log.Printf("⚠️  [Geocode] %q: %v", q, err)
			utils.RespondError(w, http.StatusBadGateway, "geocoding service unavailable")
			return
		}

		// The proximity anchor normally helps, but it can bury a distant city so
		// thoroughly that nothing usable comes back — "Windsor" for a Toronto org
		// returned a single POI near Toronto, which the area filter then dropped.
		// One unanchored retry recovers it (Windsor ON, Windsor QC) and costs a
		// second HERE call only in the case that would otherwise show "no results".
		if len(out) == 0 && (scope.Lat != 0 || scope.Lng != 0) {
			log.Printf("🔁 [Geocode] %q found nothing near the warehouse — retrying unanchored", q)
			if retry, rerr := fetchGeocodeAreas(r.Context(), q, scope.unbiased()); rerr == nil {
				out = retry
			}
		}

		// Ranked against the REAL scope, including on the unanchored retry: the
		// anchor was only dropped from the vendor query, not from what we mean by
		// "nearest".
		rankGeocodeResults(out, scope)
		if len(out) > resultLimit {
			out = out[:resultLimit]
		}

		log.Printf("🔎 [Geocode] %q → %d results (country=%s, biased to %.4f,%.4f)",
			q, len(out), scope.Country, scope.Lat, scope.Lng)
		utils.RespondJSON(w, http.StatusOK, map[string]any{"results": out})
	}
}

// fetchGeocodeAreas calls HERE once and returns the rows that are actually
// pickable areas. Split out so the unanchored retry runs the identical
// filtering — an earlier version of this logic lived in the browser, which is
// how the two copies drifted in the first place.
func fetchGeocodeAreas(ctx context.Context, q string, s geocodeScope) ([]geocodeSearchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hereGeocodeURL(q, candidateLimit, s), nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 6 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
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
		return nil, err
	}

	out := make([]geocodeSearchResult, 0, len(parsed.Items))
	seen := make(map[string]bool, len(parsed.Items))
	for _, it := range parsed.Items {
		if it.Title == "" || seen[it.Title] {
			continue
		}
		// A specific address is a legitimate target — the recommender sweeps a
		// radius around the point — so streets and house numbers stay. POIs
		// (resultType=place) do not: "the Windsor pub" is not an area.
		switch it.ResultType {
		case "locality", "administrativeArea", "street", "houseNumber", "address", "intersection":
		default:
			continue
		}
		// States and countries are not placement targets; postal codes are not
		// areas a human picks on a map.
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
	return out, nil
}
