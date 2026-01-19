package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"ropacal-backend/internal/services"
)

// ReverseGeocodeRequest represents a request to reverse geocode coordinates
type ReverseGeocodeRequest struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// GeocodeRequest represents a request to geocode an address
type GeocodeRequest struct {
	Address string `json:"address"`
}

// BatchGeocodeRequest represents a batch request to geocode multiple addresses
type BatchGeocodeRequest struct {
	Addresses []GeocodeRequest `json:"addresses"`
}

// BatchGeocodeResponse represents a batch response with multiple coordinates
type BatchGeocodeResponse struct {
	Addresses []services.Address `json:"addresses"`
	Errors    []string           `json:"errors,omitempty"`
}

// BatchReverseGeocodeRequest represents a batch request to reverse geocode multiple coordinates
type BatchReverseGeocodeRequest struct {
	Coordinates []services.Coordinates `json:"coordinates"`
}

// BatchReverseGeocodeResponse represents a batch response with multiple addresses
type BatchReverseGeocodeResponse struct {
	Addresses []services.Address `json:"addresses"`
	Errors    []string           `json:"errors,omitempty"`
}

// ReverseGeocode handles POST /api/geocoding/reverse
func ReverseGeocode() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ReverseGeocodeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		geocodingService, err := services.NewGeocodingService()
		if err != nil {
			log.Printf("Failed to create geocoding service: %v", err)
			http.Error(w, "Geocoding service unavailable", http.StatusInternalServerError)
			return
		}

		address, err := geocodingService.ReverseGeocode(req.Lat, req.Lng)
		if err != nil {
			log.Printf("Reverse geocoding failed: %v", err)
			http.Error(w, fmt.Sprintf("Failed to reverse geocode: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(address)
	}
}

// BatchReverseGeocode handles POST /api/geocoding/reverse/batch
func BatchReverseGeocode() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req BatchReverseGeocodeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if len(req.Coordinates) == 0 {
			http.Error(w, "No coordinates provided", http.StatusBadRequest)
			return
		}

		geocodingService, err := services.NewGeocodingService()
		if err != nil {
			log.Printf("Failed to create geocoding service: %v", err)
			http.Error(w, "Geocoding service unavailable", http.StatusInternalServerError)
			return
		}

		response := BatchReverseGeocodeResponse{
			Addresses: make([]services.Address, 0, len(req.Coordinates)),
			Errors:    make([]string, 0),
		}

		for i, coord := range req.Coordinates {
			address, err := geocodingService.ReverseGeocode(coord.Lat, coord.Lng)
			if err != nil {
				log.Printf("Failed to reverse geocode coordinate %d (%.6f, %.6f): %v", i, coord.Lat, coord.Lng, err)
				response.Errors = append(response.Errors, fmt.Sprintf("Index %d: %v", i, err))
				// Add empty address placeholder to maintain array alignment
				response.Addresses = append(response.Addresses, services.Address{
					FormattedAddress: "",
					Coordinates:      coord,
				})
				continue
			}
			response.Addresses = append(response.Addresses, *address)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// Geocode handles POST /api/geocoding/forward
func Geocode() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req GeocodeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if req.Address == "" {
			http.Error(w, "Address is required", http.StatusBadRequest)
			return
		}

		geocodingService, err := services.NewGeocodingService()
		if err != nil {
			log.Printf("Failed to create geocoding service: %v", err)
			http.Error(w, "Geocoding service unavailable", http.StatusInternalServerError)
			return
		}

		address, err := geocodingService.Geocode(req.Address)
		if err != nil {
			log.Printf("Geocoding failed for address '%s': %v", req.Address, err)
			http.Error(w, fmt.Sprintf("Failed to geocode: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(address)
	}
}

// BatchGeocode handles POST /api/geocoding/forward/batch
func BatchGeocode() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req BatchGeocodeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if len(req.Addresses) == 0 {
			http.Error(w, "No addresses provided", http.StatusBadRequest)
			return
		}

		geocodingService, err := services.NewGeocodingService()
		if err != nil {
			log.Printf("Failed to create geocoding service: %v", err)
			http.Error(w, "Geocoding service unavailable", http.StatusInternalServerError)
			return
		}

		response := BatchGeocodeResponse{
			Addresses: make([]services.Address, 0, len(req.Addresses)),
			Errors:    make([]string, 0),
		}

		for i, addrReq := range req.Addresses {
			if addrReq.Address == "" {
				log.Printf("Skipping empty address at index %d", i)
				response.Errors = append(response.Errors, fmt.Sprintf("Index %d: empty address", i))
				response.Addresses = append(response.Addresses, services.Address{})
				continue
			}

			address, err := geocodingService.Geocode(addrReq.Address)
			if err != nil {
				log.Printf("Failed to geocode address %d ('%s'): %v", i, addrReq.Address, err)
				response.Errors = append(response.Errors, fmt.Sprintf("Index %d: %v", i, err))
				// Add empty address placeholder to maintain array alignment
				response.Addresses = append(response.Addresses, services.Address{
					FormattedAddress: addrReq.Address,
				})
				continue
			}
			response.Addresses = append(response.Addresses, *address)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}
