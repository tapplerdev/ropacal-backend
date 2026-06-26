package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

type CityOpportunity struct {
	City             string   `json:"city"`
	BinCount         int      `json:"bin_count"`
	AvgFillRate      float64  `json:"avg_fill_rate"`
	Population       int      `json:"population"`
	MedianIncome     int      `json:"median_income"`
	OpportunityScore float64  `json:"opportunity_score"`
	OpportunityLabel string   `json:"opportunity_label"` // "high", "moderate", "low"
	Reasoning        string   `json:"reasoning"`
	RecommendedBins  int      `json:"recommended_bins"`
	TopCorridors     []string `json:"top_corridors"`
	CenterLat        float64  `json:"center_lat"`
	CenterLng        float64  `json:"center_lng"`
}

type OpportunitiesResponse struct {
	Cities              []CityOpportunity `json:"cities"`
	TotalRecommended    int               `json:"total_recommended"`
	AllocationReasoning string            `json:"allocation_reasoning"`
}

func GetPlacementOpportunities(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tStart := time.Now()

		// 1. Get all active bins grouped by city
		type cityBinStats struct {
			City       string  `db:"city"`
			BinCount   int     `db:"bin_count"`
			AvgFill    float64 `db:"avg_fill"`
			CenterLat  float64 `db:"center_lat"`
			CenterLng  float64 `db:"center_lng"`
		}
		var cityStats []cityBinStats
		err := db.Select(&cityStats, `
			SELECT
				b.city,
				COUNT(*) as bin_count,
				COALESCE(AVG(sub.avg_fill), 0) as avg_fill,
				AVG(b.latitude) as center_lat,
				AVG(b.longitude) as center_lng
			FROM bins b
			LEFT JOIN (
				SELECT c.bin_id, AVG(c.fill_percentage) as avg_fill
				FROM checks c
				GROUP BY c.bin_id
			) sub ON sub.bin_id = b.id
			WHERE b.status = 'active' AND b.latitude IS NOT NULL
			GROUP BY b.city
			ORDER BY avg_fill DESC
		`)
		if err != nil {
			log.Printf("❌ [Opportunities] DB error: %v", err)
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		// 2. Get census data grouped by city (aggregate zip-level data)
		type cityCensus struct {
			City       string `db:"city"`
			Population int    `db:"total_pop"`
			AvgIncome  int    `db:"avg_income"`
		}
		var census []cityCensus
		db.Select(&census, `
			SELECT
				b.city,
				SUM(DISTINCT cc.population) as total_pop,
				AVG(cc.median_household_income) as avg_income
			FROM bins b
			JOIN census_income_cache cc ON SUBSTRING(b.zip FROM 1 FOR 5) = cc.zip
			WHERE b.status = 'active' AND cc.population > 0
			GROUP BY b.city
		`)
		censusMap := map[string]cityCensus{}
		for _, c := range census {
			censusMap[strings.ToLower(c.City)] = c
		}

		// 3. Get top-performing streets per city (for corridors)
		type streetPerf struct {
			City   string  `db:"city"`
			Street string  `db:"street"`
			Fill   float64 `db:"avg_fill"`
		}
		var streets []streetPerf
		db.Select(&streets, `
			SELECT b.city, b.current_street as street, AVG(c.fill_percentage) as avg_fill
			FROM bins b
			JOIN checks c ON c.bin_id = b.id
			WHERE b.status = 'active'
			GROUP BY b.city, b.current_street
			ORDER BY avg_fill DESC
		`)
		cityCorridors := map[string][]string{}
		for _, s := range streets {
			key := strings.ToLower(s.City)
			if len(cityCorridors[key]) < 3 {
				// Extract road name from full address (e.g., "1776 N Milpitas Blvd" → "N Milpitas Blvd")
				parts := strings.SplitN(s.Street, " ", 2)
				roadName := s.Street
				if len(parts) > 1 {
					roadName = parts[1]
				}
				cityCorridors[key] = append(cityCorridors[key], roadName)
			}
		}

		// 4. Build city opportunities — include existing bin cities + expansion cities
		existingCities := map[string]bool{}
		var cities []CityOpportunity

		for _, cs := range cityStats {
			existingCities[strings.ToLower(cs.City)] = true

			pop := 0
			income := 0
			if c, ok := censusMap[strings.ToLower(cs.City)]; ok {
				pop = c.Population
				income = c.AvgIncome
			}

			// Opportunity score — proven demand cities
			// High fill + few bins = strongest signal (underserved proven market)
			// Low fill + many bins = weakest (saturated or poor area)
			demand := cs.AvgFill / 100.0                                       // 0-1, higher = more demand
			estimatedNeed := math.Max(float64(pop)/25000.0, 2)                 // at least 2 bins per city
			coverageGap := math.Max(0, 1.0-float64(cs.BinCount)/estimatedNeed) // 0-1, higher = more underserved
			incomeFactor := math.Min(float64(income)/150000.0, 1.0)            // 0-1

			// Multiplicative: high demand AND coverage gap = high score
			// Either being low tanks the score (same logic as placement algorithm)
			demandCoverage := math.Sqrt(demand * coverageGap) // geometric mean, 0-1
			score := math.Round((demandCoverage*0.7+incomeFactor*0.3)*100) / 10

			// Recommended additional bins
			recommended := int(math.Max(0, math.Ceil(estimatedNeed)-float64(cs.BinCount)))
			if recommended > 10 {
				recommended = 10
			}

			// Label
			label := "low"
			if score >= 5 {
				label = "high"
			} else if score >= 3 {
				label = "moderate"
			}

			// Reasoning
			reasoning := ""
			if cs.BinCount <= 2 && cs.AvgFill > 30 {
				reasoning = fmt.Sprintf("%d bin(s) serving %dk population at %.0f%% fill — severely underserved",
					cs.BinCount, pop/1000, cs.AvgFill)
			} else if cs.AvgFill > 50 {
				reasoning = fmt.Sprintf("%d bins averaging %.0f%% fill — high demand, room for more",
					cs.BinCount, cs.AvgFill)
			} else if cs.AvgFill < 25 {
				reasoning = fmt.Sprintf("%d bins averaging only %.0f%% fill — consider relocation over expansion",
					cs.BinCount, cs.AvgFill)
			} else {
				reasoning = fmt.Sprintf("%d bins at %.0f%% fill — moderate demand",
					cs.BinCount, cs.AvgFill)
			}

			corridors := cityCorridors[strings.ToLower(cs.City)]
			if corridors == nil {
				corridors = []string{}
			}

			cities = append(cities, CityOpportunity{
				City:             cs.City,
				BinCount:         cs.BinCount,
				AvgFillRate:      math.Round(cs.AvgFill*10) / 10,
				Population:       pop,
				MedianIncome:     income,
				OpportunityScore: score,
				OpportunityLabel: label,
				Reasoning:        reasoning,
				RecommendedBins:  recommended,
				TopCorridors:     corridors,
				CenterLat:        cs.CenterLat,
				CenterLng:        cs.CenterLng,
			})
		}

		// 5. Add expansion cities (zero bins)
		expansionCenters := []struct {
			City string
			Lat  float64
			Lng  float64
			Pop  int
		}{
			{"Fremont", 37.5485, -121.9886, 230000},
			{"Oakland", 37.8044, -122.2712, 433000},
			{"Berkeley", 37.8716, -122.2727, 124000},
			{"Union City", 37.5934, -122.0439, 75000},
			{"San Leandro", 37.7249, -122.1561, 91000},
			{"Alameda", 37.7652, -122.2416, 79000},
			{"Emeryville", 37.8313, -122.2852, 12000},
		}
		for _, ec := range expansionCenters {
			if existingCities[strings.ToLower(ec.City)] {
				continue
			}
			estimatedNeed := math.Max(float64(ec.Pop)/25000.0, 2)
			// Expansion cities: no proven demand, score conservatively
			// Population-based estimate only — capped below proven-demand cities
			popFactor := math.Min(float64(ec.Pop)/500000.0, 1.0) // larger city = more potential
			score := math.Round(popFactor*30) / 10                // max 3.0 for expansion cities
			recommended := int(math.Min(estimatedNeed, 5))

			label := "moderate"
			if score < 2 {
				label = "low"
			}

			cities = append(cities, CityOpportunity{
				City:             ec.City,
				BinCount:         0,
				AvgFillRate:      0,
				Population:       ec.Pop,
				MedianIncome:     0,
				OpportunityScore: score,
				OpportunityLabel: label,
				Reasoning:        fmt.Sprintf("New territory — %dk population, no existing bins, unvalidated demand", ec.Pop/1000),
				RecommendedBins:  recommended,
				TopCorridors:     []string{},
				CenterLat:        ec.Lat,
				CenterLng:        ec.Lng,
			})
		}

		// Sort by opportunity score descending
		sort.Slice(cities, func(i, j int) bool { return cities[i].OpportunityScore > cities[j].OpportunityScore })

		// 6. Build allocation reasoning
		totalRec := 0
		var highCities, modCities []string
		for _, c := range cities {
			totalRec += c.RecommendedBins
			if c.OpportunityLabel == "high" {
				highCities = append(highCities, c.City)
			} else if c.OpportunityLabel == "moderate" {
				modCities = append(modCities, c.City)
			}
		}

		var allocation string
		if len(highCities) > 0 {
			allocation = fmt.Sprintf("Focus on %s (proven demand, highest opportunity). ", strings.Join(highCities, ", "))
		}
		if len(modCities) > 0 {
			if allocation == "" {
				allocation = fmt.Sprintf("Moderate opportunity in %s. ", strings.Join(modCities[:minInt(3, len(modCities))], ", "))
			} else {
				allocation += fmt.Sprintf("Secondary: %s.", strings.Join(modCities[:minInt(3, len(modCities))], ", "))
			}
		}
		if allocation == "" {
			allocation = "All cities have low opportunity scores — consider improving existing bin placements before expanding."
		}

		log.Printf("📍 [Opportunities] Computed %d cities in %v (total recommended: %d)", len(cities), time.Since(tStart), totalRec)

		resp := OpportunitiesResponse{
			Cities:              cities,
			TotalRecommended:    totalRec,
			AllocationReasoning: allocation,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
