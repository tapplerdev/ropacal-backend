package services

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
)

// GeocodingService handles geocoding and reverse geocoding using Google Maps API
type GeocodingService struct {
	apiKey string
	client *http.Client
}

// Coordinates represents latitude and longitude
type Coordinates struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// Address represents a full address
type Address struct {
	FormattedAddress string      `json:"formatted_address"`
	Coordinates      Coordinates `json:"coordinates"`
}

// GoogleGeocodeResponse represents the Google Maps Geocoding API response
type GoogleGeocodeResponse struct {
	Results []struct {
		FormattedAddress string `json:"formatted_address"`
		Geometry         struct {
			Location Coordinates `json:"location"`
		} `json:"geometry"`
	} `json:"results"`
	Status string `json:"status"`
}

// NewGeocodingService creates a new geocoding service
func NewGeocodingService() (*GeocodingService, error) {
	apiKey := os.Getenv("GOOGLE_MAPS_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("GOOGLE_MAPS_API_KEY environment variable is required")
	}

	return &GeocodingService{
		apiKey: apiKey,
		client: &http.Client{},
	}, nil
}

// ReverseGeocode converts coordinates to an address
func (s *GeocodingService) ReverseGeocode(lat, lng float64) (*Address, error) {
	baseURL := "https://maps.googleapis.com/maps/api/geocode/json"

	params := url.Values{}
	params.Add("latlng", fmt.Sprintf("%f,%f", lat, lng))
	params.Add("key", s.apiKey)

	fullURL := fmt.Sprintf("%s?%s", baseURL, params.Encode())

	resp, err := s.client.Get(fullURL)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status code %d", resp.StatusCode)
	}

	var result GoogleGeocodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if result.Status != "OK" {
		return nil, fmt.Errorf("geocoding API returned status: %s", result.Status)
	}

	if len(result.Results) == 0 {
		return nil, fmt.Errorf("no results found")
	}

	firstResult := result.Results[0]
	return &Address{
		FormattedAddress: firstResult.FormattedAddress,
		Coordinates: Coordinates{
			Lat: lat,
			Lng: lng,
		},
	}, nil
}

// Geocode converts an address string to coordinates
func (s *GeocodingService) Geocode(address string) (*Address, error) {
	baseURL := "https://maps.googleapis.com/maps/api/geocode/json"

	params := url.Values{}
	params.Add("address", address)
	params.Add("key", s.apiKey)

	fullURL := fmt.Sprintf("%s?%s", baseURL, params.Encode())

	resp, err := s.client.Get(fullURL)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status code %d", resp.StatusCode)
	}

	var result GoogleGeocodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if result.Status != "OK" {
		return nil, fmt.Errorf("geocoding API returned status: %s", result.Status)
	}

	if len(result.Results) == 0 {
		return nil, fmt.Errorf("no results found for address: %s", address)
	}

	firstResult := result.Results[0]
	return &Address{
		FormattedAddress: firstResult.FormattedAddress,
		Coordinates:      firstResult.Geometry.Location,
	}, nil
}
