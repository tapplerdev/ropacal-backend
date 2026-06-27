package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// errORToolsParse marks an OR-Tools failure that occurred while parsing the
// response body (as opposed to making the request). Callers that previously
// emitted distinct HTTP error messages for the two cases use errors.Is to
// preserve that distinction.
var errORToolsParse = errors.New("ortools parse error")

// errOSRMRequest marks an OSRM failure that occurred while making the HTTP
// request (as opposed to parsing the response / a non-Ok code). Callers that
// previously emitted distinct HTTP error messages use errors.Is to distinguish.
var errOSRMRequest = errors.New("osrm request error")

// Shared helpers for the route-optimization handlers (smart_routes.go and
// route_reoptimize.go). These extract logic that was duplicated near-verbatim
// across both handlers: warehouse-config lookup, the OSRM Table API call +
// int-matrix conversion, the OR-Tools CVRP request/response, and the
// directional ("South"/"Central"/"North"/"Area %d") route naming.

// defaultWarehouseLat / defaultWarehouseLng are the hardcoded fallback used when
// the config table has no usable warehouse_location row.
const (
	defaultWarehouseLat = 37.6368013
	defaultWarehouseLng = -122.1269379
)

// fetchWarehouseLocation reads the warehouse_location config row and returns its
// latitude/longitude, falling back to the hardcoded default when the row is
// missing, unparseable, or has a zero latitude (matching the prior behavior of
// both handlers).
func fetchWarehouseLocation(db *sqlx.DB) (lat, lng float64) {
	lat, lng = defaultWarehouseLat, defaultWarehouseLng
	var whJSON []byte
	if err := db.QueryRow(`SELECT value FROM config WHERE key = 'warehouse_location'`).Scan(&whJSON); err == nil {
		var wh struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		}
		if json.Unmarshal(whJSON, &wh) == nil && wh.Latitude != 0 {
			lat, lng = wh.Latitude, wh.Longitude
		}
	}
	return lat, lng
}

// osrmServerURL returns the configured OSRM server URL or the public default.
func osrmServerURL() string {
	url := os.Getenv("OSRM_SERVER_URL")
	if url == "" {
		url = "http://router.project-osrm.org"
	}
	return url
}

// ortoolsServiceURL returns the configured OR-Tools service URL or the local default.
func ortoolsServiceURL() string {
	url := os.Getenv("ORTOOLS_SERVICE_URL")
	if url == "" {
		url = "http://localhost:8000"
	}
	return url
}

// fetchOSRMMatrices builds the OSRM coordinate string (index 0 = warehouse,
// indices 1..N = bins), calls the OSRM Table API, and converts the float
// duration/distance matrices to int matrices. coords[i] must be "lon,lat".
// On any failure it returns an error; callers map that to their own HTTP error.
func fetchOSRMMatrices(coords []string) (distMatrix, durMatrix [][]int, err error) {
	n := len(coords)
	tableURL := fmt.Sprintf("%s/table/v1/driving/%s?annotations=duration,distance", osrmServerURL(), strings.Join(coords, ";"))

	osrmClient := &http.Client{Timeout: 30 * time.Second}
	osrmResp, err := osrmClient.Get(tableURL)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errOSRMRequest, err)
	}
	defer osrmResp.Body.Close()

	osrmBody, _ := io.ReadAll(osrmResp.Body)
	var osrmData struct {
		Code      string      `json:"code"`
		Durations [][]float64 `json:"durations"`
		Distances [][]float64 `json:"distances"`
	}
	if e := json.Unmarshal(osrmBody, &osrmData); e != nil || osrmData.Code != "Ok" {
		if e != nil {
			return nil, nil, fmt.Errorf("OSRM response parse error: %w", e)
		}
		return nil, nil, fmt.Errorf("OSRM returned code %q", osrmData.Code)
	}

	distMatrix = make([][]int, n)
	durMatrix = make([][]int, n)
	for i := 0; i < n; i++ {
		distMatrix[i] = make([]int, n)
		durMatrix[i] = make([]int, n)
		for j := 0; j < n; j++ {
			distMatrix[i][j] = int(osrmData.Distances[i][j])
			durMatrix[i][j] = int(osrmData.Durations[i][j])
		}
	}
	return distMatrix, durMatrix, nil
}

// ortoolsLocation is the location payload shape sent to the OR-Tools service.
type ortoolsLocation struct {
	ID   string  `json:"id"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
	Name string  `json:"name"`
}

// ortoolsResult is the parsed OR-Tools CVRP response shared by both handlers.
type ortoolsResult struct {
	Routes []struct {
		VehicleID     int   `json:"vehicle_id"`
		StopIndices   []int `json:"stop_indices"`
		TotalDistance int   `json:"total_distance"`
		TotalDuration int   `json:"total_duration"`
	} `json:"routes"`
	Unassigned      []int `json:"unassigned"`
	Feasible        bool  `json:"feasible"`
	SolverRuntimeMs int   `json:"solver_runtime_ms"`
}

// callORTools builds the OR-Tools CVRP request map, POSTs it to the optimizer
// service, and parses the response. On any failure it returns an error.
func callORTools(locations []ortoolsLocation, distMatrix, durMatrix [][]int, numVehicles, vehicleCapacity int) (ortoolsResult, error) {
	var result ortoolsResult

	ortoolsReq := map[string]interface{}{
		"locations":           locations,
		"distance_matrix":     distMatrix,
		"duration_matrix":     durMatrix,
		"num_vehicles":        numVehicles,
		"vehicle_capacity":    vehicleCapacity,
		"depot_index":         0,
		"max_runtime_seconds": 30,
	}

	reqBody, _ := json.Marshal(ortoolsReq)
	ortoolsClient := &http.Client{Timeout: 60 * time.Second}
	resp, err := ortoolsClient.Post(ortoolsServiceURL()+"/generate-templates", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return result, fmt.Errorf("OR-Tools request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &result); err != nil {
		return result, fmt.Errorf("%w: %v (body: %s)", errORToolsParse, err, string(body))
	}
	return result, nil
}

// directionalSuffixes returns the directional labels for n routes that share a
// dominant city, ordered from lowest to highest latitude rank: South/North for
// 2 routes, South/Central/North for 3, and "Area 1".."Area n" for more than 3.
// For n <= 1 it returns nil (no suffix needed).
func directionalSuffixes(n int) []string {
	switch {
	case n <= 1:
		return nil
	case n == 2:
		return []string{"South", "North"}
	case n == 3:
		return []string{"South", "Central", "North"}
	default:
		labels := make([]string, 0, n)
		for i := 0; i < n; i++ {
			labels = append(labels, fmt.Sprintf("Area %d", i+1))
		}
		return labels
	}
}
