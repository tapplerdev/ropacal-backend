package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/orgdb"
	"ropacal-backend/pkg/utils"
)

// ADDRESS geocoding for the dashboard — typeahead, lookup, and reverse.
//
// Distinct from geocode_search.go, which resolves AREAS (a city or district to
// aim the recommender at). This is the everyday surface: typing a bin's street
// address, dropping a pin on a map and getting the address back. It backs the
// shared HerePlacesAutocomplete component and every map-click handler.
//
// WHY THIS MOVED SERVER-SIDE. The browser called HERE directly, and that had
// the same defect the area picker had, in a much larger surface:
//
//   - NO country filter at all, so a Toronto tenant could be offered US
//     addresses with nothing to stop it.
//   - The proximity anchor DEFAULTED TO KANSAS CITY (39.0997,-94.5786) when the
//     caller passed no location — and of the components using the shared
//     autocomplete, exactly one passed one. So virtually every address field in
//     the app was ranking suggestions around Missouri.
//   - The key was NEXT_PUBLIC_HERE_API_KEY, readable by anyone with devtools.
//
// All three vanish by asking the server, which knows the caller's organization:
// its country becomes the filter, its warehouse becomes the anchor, and the key
// never leaves the backend. Switching a tenant between US and Canadian results
// is then one column — organizations.country — not a prop threaded through ten
// components.

// hereAddress is the address block HERE returns on both lookup and revgeocode.
type hereAddress struct {
	Label       string `json:"label"`
	HouseNumber string `json:"houseNumber"`
	Street      string `json:"street"`
	City        string `json:"city"`
	PostalCode  string `json:"postalCode"`
	State       string `json:"state"`
	StateCode   string `json:"stateCode"`
	CountryName string `json:"countryName"`
	CountryCode string `json:"countryCode"`
}

// addressResult is the wire shape the dashboard already expects. Kept identical
// to what the browser used to build itself, so the frontend swap is a change of
// TRANSPORT only — no component has to reinterpret a different payload.
type addressResult struct {
	Street  string `json:"street"`
	City    string `json:"city"`
	Zip     string `json:"zip"`
	State   string `json:"state,omitempty"`
	Country string `json:"country,omitempty"`
	// No omitempty: the dashboard types these as REQUIRED numbers and assigns
	// them straight into non-nullable state. A literal 0 coordinate must still
	// arrive as 0, not vanish from the payload.
	Latitude         float64 `json:"latitude"`
	Longitude        float64 `json:"longitude"`
	FormattedAddress string  `json:"formattedAddress"`
}

// toAddressResult flattens HERE's address block the same way the browser did.
func (a hereAddress) toAddressResult(lat, lng float64) addressResult {
	street := a.Street
	if a.HouseNumber != "" && a.Street != "" {
		street = a.HouseNumber + " " + a.Street
	}
	state := a.StateCode
	if state == "" {
		state = a.State
	}
	country := a.CountryCode
	if country == "" {
		country = a.CountryName
	}
	return addressResult{
		Street: strings.TrimSpace(street), City: a.City, Zip: a.PostalCode,
		State: state, Country: country,
		Latitude: lat, Longitude: lng, FormattedAddress: a.Label,
	}
}

// hereGet performs a HERE request and decodes it into out.
func hereGet(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 6 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HERE returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// requireHereKey writes the error response when the key is missing.
func requireHereKey(w http.ResponseWriter) bool {
	if HereAPIKey == "" {
		log.Printf("⚠️  [Geocode] HERE_API_KEY is not set — address geocoding disabled")
		utils.RespondError(w, http.StatusServiceUnavailable, "geocoding is not configured")
		return false
	}
	return true
}

// GET /api/geocode/autosuggest?q=&lat=&lng=
//
// Address typeahead. /autosuggest rather than /geocode because this one is
// typed character by character: measured on partial strings, autosuggest put
// the intended place first every time where /geocode returned nothing at all
// for "Mississ" or "Sacrame".
//
// lat/lng are OPTIONAL and override the warehouse anchor — a map-centred dialog
// can pass what the user is looking at. Unlike the old browser default, an
// absent location falls back to the organization's warehouse, not to Missouri.
func GeocodeAutosuggest(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if len(q) < 3 {
			utils.RespondJSON(w, http.StatusOK, map[string]any{"suggestions": []any{}})
			return
		}
		if !requireHereKey(w) {
			return
		}

		scope := scopeForOrg(orgdb.From(r))
		if lat, lng, ok := optionalLatLng(r); ok {
			scope.Lat, scope.Lng = lat, lng
		}

		qs := url.Values{}
		qs.Set("q", q)
		qs.Set("limit", "10")
		qs.Set("lang", "en")
		qs.Set("apiKey", HereAPIKey)
		if scope.Country != "" {
			qs.Set("in", "countryCode:"+scope.Country)
		}
		// autosuggest REQUIRES an anchor. The org's warehouse is the honest
		// default; the country centroid keeps a warehouse-less org on the right
		// continent instead of in the Gulf of Guinea.
		qs.Set("at", addressAnchor(scope))

		var parsed struct {
			Items []struct {
				ID         string      `json:"id"`
				Title      string      `json:"title"`
				ResultType string      `json:"resultType"`
				Address    hereAddress `json:"address"`
			} `json:"items"`
		}
		if err := hereGet(r.Context(), "https://autosuggest.search.hereapi.com/v1/autosuggest?"+qs.Encode(), &parsed); err != nil {
			log.Printf("⚠️  [Geocode] autosuggest %q failed: %v", q, err)
			utils.RespondError(w, http.StatusBadGateway, "geocoding service unavailable")
			return
		}

		type suggestion struct {
			ID         string            `json:"id"`
			Title      string            `json:"title"`
			ResultType string            `json:"resultType"`
			Address    map[string]string `json:"address,omitempty"`
		}
		out := make([]suggestion, 0, len(parsed.Items))
		for _, it := range parsed.Items {
			// Query rows (chainQuery/categoryQuery) carry an href to run another
			// search instead of a place; they cannot be looked up or selected.
			if it.ID == "" {
				continue
			}
			s := suggestion{ID: it.ID, Title: it.Title, ResultType: it.ResultType}
			if it.Address.Label != "" {
				s.Address = map[string]string{"label": it.Address.Label}
			}
			out = append(out, s)
		}
		utils.RespondJSON(w, http.StatusOK, map[string]any{"suggestions": out})
	}
}

// GET /api/geocode/lookup?id=  — resolve a suggestion to a full address.
func GeocodeLookup(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			utils.RespondError(w, http.StatusBadRequest, "id is required")
			return
		}
		if !requireHereKey(w) {
			return
		}

		qs := url.Values{}
		qs.Set("id", id)
		qs.Set("apiKey", HereAPIKey)

		var parsed struct {
			Address  hereAddress `json:"address"`
			Position struct {
				Lat float64 `json:"lat"`
				Lng float64 `json:"lng"`
			} `json:"position"`
		}
		if err := hereGet(r.Context(), "https://lookup.search.hereapi.com/v1/lookup?"+qs.Encode(), &parsed); err != nil {
			log.Printf("⚠️  [Geocode] lookup %q failed: %v", id, err)
			utils.RespondError(w, http.StatusBadGateway, "geocoding service unavailable")
			return
		}
		if parsed.Address.Label == "" {
			utils.RespondError(w, http.StatusNotFound, "no address for that id")
			return
		}
		utils.RespondJSON(w, http.StatusOK,
			parsed.Address.toAddressResult(parsed.Position.Lat, parsed.Position.Lng))
	}
}

// GET /api/geocode/reverse?lat=&lng=  — coordinates to an address.
//
// Used by every drop-a-pin-on-the-map flow. No country scoping: the caller
// already has a specific point, and filtering by country could only make a
// legitimate answer disappear.
func GeocodeReverse(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lat, lng, ok := optionalLatLng(r)
		if !ok {
			utils.RespondError(w, http.StatusBadRequest, "lat and lng are required")
			return
		}
		if !requireHereKey(w) {
			return
		}

		qs := url.Values{}
		qs.Set("at", fmt.Sprintf("%.6f,%.6f", lat, lng))
		qs.Set("limit", "1")
		qs.Set("apiKey", HereAPIKey)

		var parsed struct {
			Items []struct {
				Address hereAddress `json:"address"`
			} `json:"items"`
		}
		if err := hereGet(r.Context(), "https://revgeocode.search.hereapi.com/v1/revgeocode?"+qs.Encode(), &parsed); err != nil {
			log.Printf("⚠️  [Geocode] reverse %.5f,%.5f failed: %v", lat, lng, err)
			utils.RespondError(w, http.StatusBadGateway, "geocoding service unavailable")
			return
		}
		if len(parsed.Items) == 0 {
			utils.RespondError(w, http.StatusNotFound, "no address at those coordinates")
			return
		}
		utils.RespondJSON(w, http.StatusOK, parsed.Items[0].Address.toAddressResult(lat, lng))
	}
}

// optionalLatLng reads lat/lng query params, reporting whether BOTH parsed. A
// half-supplied pair is treated as absent rather than as 0,0 — the Gulf of
// Guinea is never what a caller meant.
func optionalLatLng(r *http.Request) (float64, float64, bool) {
	latS, lngS := r.URL.Query().Get("lat"), r.URL.Query().Get("lng")
	if latS == "" || lngS == "" {
		return 0, 0, false
	}
	lat, err1 := strconv.ParseFloat(latS, 64)
	lng, err2 := strconv.ParseFloat(lngS, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return 0, 0, false
	}
	return lat, lng, true
}

// addressAnchor is the required `at=` for autosuggest: the warehouse, else the
// organization's country centroid.
func addressAnchor(s geocodeScope) string {
	if s.Lat != 0 || s.Lng != 0 {
		return fmt.Sprintf("%.6f,%.6f", s.Lat, s.Lng)
	}
	switch s.Country {
	case "CAN":
		return "56.130400,-106.346800"
	case "GBR":
		return "54.702400,-3.276600"
	case "AUS":
		return "-25.274400,133.775100"
	case "MEX":
		return "23.634500,-102.552800"
	default:
		return "39.828200,-98.579500" // geographic centre of the contiguous US
	}
}
