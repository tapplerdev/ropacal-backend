package services

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
)

// HEREGeocodeResponse represents the response from HERE Geocoding API
type HEREGeocodeResponse struct {
	Items []struct {
		Position struct {
			Lat float64 `json:"lat"`
			Lng float64 `json:"lng"`
		} `json:"position"`
		Address struct {
			Label string `json:"label"`
		} `json:"address"`
		Scoring struct {
			QueryScore float64 `json:"queryScore"`
		} `json:"scoring"`
	} `json:"items"`
}

// GeocodeResult represents the result of geocoding a single address
type GeocodeResult struct {
	BinID          string  `json:"bin_id"`
	BinNumber      int     `json:"bin_number"`
	Address        string  `json:"address"`
	OldLatitude    float64 `json:"old_latitude"`
	OldLongitude   float64 `json:"old_longitude"`
	NewLatitude    float64 `json:"new_latitude"`
	NewLongitude   float64 `json:"new_longitude"`
	DistanceMoved  float64 `json:"distance_moved_km"`
	NeedsReview    bool    `json:"needs_review"`
	GeocodeSuccess bool    `json:"geocode_success"`
	ErrorMessage   string  `json:"error_message,omitempty"`
}

// HEREGeocodingService handles geocoding operations using HERE Maps API
type HEREGeocodingService struct {
	apiKey string
}

// NewHEREGeocodingService creates a new HERE geocoding service
func NewHEREGeocodingService(apiKey string) *HEREGeocodingService {
	return &HEREGeocodingService{
		apiKey: apiKey,
	}
}

// GeocodeAddress geocodes a single address using HERE Geocoding API
func (gs *HEREGeocodingService) GeocodeAddress(street, city, zip string) (float64, float64, error) {
	// Build the full address query
	query := fmt.Sprintf("%s, %s, %s", street, city, zip)

	// Build URL
	baseURL := "https://geocode.search.hereapi.com/v1/geocode"
	params := url.Values{}
	params.Add("q", query)
	params.Add("apiKey", gs.apiKey)

	fullURL := fmt.Sprintf("%s?%s", baseURL, params.Encode())

	log.Printf("🌍 Geocoding: %s", query)

	// Make request
	resp, err := http.Get(fullURL)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to make geocoding request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, 0, fmt.Errorf("geocoding API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to read response body: %w", err)
	}

	var result HEREGeocodeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, 0, fmt.Errorf("failed to parse geocoding response: %w", err)
	}

	// Check if we got results
	if len(result.Items) == 0 {
		return 0, 0, fmt.Errorf("no geocoding results found for address: %s", query)
	}

	// Return the first (best) result
	lat := result.Items[0].Position.Lat
	lng := result.Items[0].Position.Lng

	log.Printf("   ✅ Found: %.6f, %.6f (confidence: %.2f)", lat, lng, result.Items[0].Scoring.QueryScore)

	return lat, lng, nil
}

// CompareCoordinates compares old and new coordinates and returns distance moved
func (gs *HEREGeocodingService) CompareCoordinates(oldLat, oldLng, newLat, newLng float64) (float64, bool) {
	// If old coordinates are zero/missing, don't flag for review
	if oldLat == 0 && oldLng == 0 {
		return 0, false
	}

	// Calculate distance using haversine
	distance := haversineDistance(oldLat, oldLng, newLat, newLng)

	// Flag for review if moved more than 1km
	needsReview := distance > 1.0

	return distance, needsReview
}
