package handlers

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"sort"

	"ropacal-backend/internal/models"

	"github.com/jmoiron/sqlx"
)

type RouteRequest struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Limit     int     `json:"limit"`
}

type BinWithDistance struct {
	Bin      models.Bin
	Distance float64
}

// Calculate distance between two coordinates using Haversine formula
func calculateDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371 // km

	dLat := (lat2 - lat1) * math.Pi / 180.0
	dLon := (lon2 - lon1) * math.Pi / 180.0

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180.0)*math.Cos(lat2*math.Pi/180.0)*
			math.Sin(dLon/2)*math.Sin(dLon/2)

	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return earthRadius * c
}

// OptimizeRoute returns an optimized route based on location and bin fill levels
func OptimizeRoute(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req RouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Default limit
		if req.Limit <= 0 {
			req.Limit = 5
		}

		log.Printf("🚗 Optimizing route from (%.6f, %.6f) with limit %d", req.Latitude, req.Longitude, req.Limit)

		// Get all bins with high fill percentage (>70%)
		var bins []models.Bin
		query := `
			SELECT * FROM bins
			WHERE fill_percentage > 70
			AND latitude IS NOT NULL
			AND longitude IS NOT NULL
			AND status = 'Active'
		`
		if err := db.Select(&bins, query); err != nil {
			log.Printf("Error fetching bins: %v", err)
			http.Error(w, "Failed to fetch bins", http.StatusInternalServerError)
			return
		}

		log.Printf("Found %d high-fill bins", len(bins))

		if len(bins) == 0 {
			// Return empty array if no bins found
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]models.BinResponse{})
			return
		}

		// Calculate distances and sort
		binsWithDistance := make([]BinWithDistance, len(bins))
		for i, bin := range bins {
			distance := calculateDistance(
				req.Latitude,
				req.Longitude,
				*bin.Latitude,
				*bin.Longitude,
			)
			binsWithDistance[i] = BinWithDistance{
				Bin:      bin,
				Distance: distance,
			}
		}

		// Sort by distance (nearest first)
		sort.Slice(binsWithDistance, func(i, j int) bool {
			return binsWithDistance[i].Distance < binsWithDistance[j].Distance
		})

		// Take top N bins
		resultCount := req.Limit
		if len(binsWithDistance) < resultCount {
			resultCount = len(binsWithDistance)
		}

		// Convert to response format
		result := make([]models.BinResponse, resultCount)
		for i := 0; i < resultCount; i++ {
			result[i] = binsWithDistance[i].Bin.ToBinResponse()
			log.Printf("  Stop %d: Bin #%d (%.2f km away, %d%% full)",
				i+1,
				binsWithDistance[i].Bin.BinNumber,
				binsWithDistance[i].Distance,
				*binsWithDistance[i].Bin.FillPercentage,
			)
		}

		log.Printf("✓ Route optimized: %d bins selected", len(result))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}
