package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"time"
)

type LocationRecommendation struct {
	Latitude        float64 `json:"latitude"`
	Longitude       float64 `json:"longitude"`
	Address         string  `json:"address"`
	City            string  `json:"city"`
	Zip             string  `json:"zip"`
	Score           float64 `json:"score"`
	Reasoning       string  `json:"reasoning"`
	NearestBinNum   int     `json:"nearest_bin_number"`
	NearestBinDist  float64 `json:"nearest_bin_distance_miles"`
	AreaAvgFillRate float64 `json:"area_avg_fill_rate"`
	MedianIncome    int     `json:"median_income,omitempty"`
}

type existingBin struct {
	ID             string  `db:"id"`
	BinNumber      int     `db:"bin_number"`
	Latitude       float64 `db:"latitude"`
	Longitude      float64 `db:"longitude"`
	City           string  `db:"city"`
	Zip            string  `db:"zip"`
	FillPercentage *int    `db:"fill_percentage"`
}

type noGoZone struct {
	CenterLat    float64 `db:"center_latitude"`
	CenterLng    float64 `db:"center_longitude"`
	RadiusMeters int     `db:"radius_meters"`
}

type candidate struct {
	Lat            float64
	Lng            float64
	City           string
	Zip            string
	AreaFillRate   float64
	NearestBinNum  int
	NearestBinDist float64 // miles
	Score          float64
}

// toolRecommendLocations generates AI-powered location recommendations
func (h *ChatHandler) toolRecommendLocations(params map[string]any) (string, error) {
	count := 10
	if c, ok := params["count"].(float64); ok && c > 0 {
		count = int(c)
		if count > 30 {
			count = 30
		}
	}

	targetCity := ""
	if tc, ok := params["target_city"].(string); ok {
		targetCity = tc
	}

	minGapMiles := 0.3
	if mg, ok := params["min_gap_miles"].(float64); ok && mg > 0 {
		minGapMiles = mg
	}

	// Step 1: Get all active bins with coordinates
	var bins []existingBin
	query := `SELECT id, bin_number, latitude, longitude, city, zip, fill_percentage
		FROM bins WHERE status = 'active' AND latitude IS NOT NULL AND longitude IS NOT NULL`
	args := []any{}
	if targetCity != "" {
		query += " AND LOWER(city) = LOWER($1)"
		args = append(args, targetCity)
	}
	if err := h.db.Select(&bins, query, args...); err != nil {
		return "", fmt.Errorf("failed to fetch bins: %w", err)
	}
	if len(bins) < 3 {
		return "", fmt.Errorf("need at least 3 active bins with coordinates to generate recommendations")
	}

	// Step 2: Calculate avg fill rate per city from check history
	type cityRate struct {
		City    string  `db:"city"`
		AvgRate float64 `db:"avg_rate"`
	}
	var rates []cityRate
	h.db.Select(&rates, `
		WITH ordered_checks AS (
			SELECT c.bin_id, c.fill_percentage, c.checked_on, b.city,
				LAG(c.fill_percentage) OVER (PARTITION BY c.bin_id ORDER BY c.checked_on) as prev_fill,
				LAG(c.checked_on) OVER (PARTITION BY c.bin_id ORDER BY c.checked_on) as prev_checked_on
			FROM checks c JOIN bins b ON c.bin_id = b.id
			WHERE c.fill_percentage IS NOT NULL AND b.status = 'active'
		)
		SELECT city, AVG(
			(fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400)
		) as avg_rate
		FROM ordered_checks
		WHERE prev_fill IS NOT NULL
			AND fill_percentage > prev_fill
			AND (checked_on - prev_checked_on) > 3600
			AND (fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400) < 50
		GROUP BY city
	`)
	cityFillRate := map[string]float64{}
	for _, r := range rates {
		cityFillRate[r.City] = r.AvgRate
	}

	// Step 3: Get active no-go zones
	var zones []noGoZone
	h.db.Select(&zones, `
		SELECT center_latitude, center_longitude, GREATEST(radius_meters, 500) as radius_meters
		FROM no_go_zones WHERE status = 'active' AND merged_into_zone_id IS NULL
	`)

	// Step 4: Get census income data
	type incomeRow struct {
		Zip    string `db:"zip"`
		Income int    `db:"median_household_income"`
	}
	var incomes []incomeRow
	h.db.Select(&incomes, `SELECT zip, median_household_income FROM census_income_cache WHERE median_household_income > 0`)
	zipIncome := map[string]int{}
	for _, inc := range incomes {
		zipIncome[inc.Zip] = inc.Income
	}

	// Step 5: Generate candidate locations — midpoints between distant bins in same city
	var candidates []candidate

	// Group bins by city
	cityBins := map[string][]existingBin{}
	for _, b := range bins {
		cityBins[b.City] = append(cityBins[b.City], b)
	}

	for city, cBins := range cityBins {
		fillRate := cityFillRate[city]
		if fillRate <= 0 {
			fillRate = 5.0 // default
		}

		for i := 0; i < len(cBins); i++ {
			for j := i + 1; j < len(cBins); j++ {
				dist := haversineDistMiles(cBins[i].Latitude, cBins[i].Longitude, cBins[j].Latitude, cBins[j].Longitude)
				if dist < minGapMiles*2 {
					continue // bins too close, no gap to fill
				}

				// Generate midpoint
				midLat := (cBins[i].Latitude + cBins[j].Latitude) / 2
				midLng := (cBins[i].Longitude + cBins[j].Longitude) / 2

				// Also generate 1/3 and 2/3 points for wider gaps
				points := [][2]float64{{midLat, midLng}}
				if dist > minGapMiles*4 {
					points = append(points,
						[2]float64{cBins[i].Latitude + (cBins[j].Latitude-cBins[i].Latitude)/3, cBins[i].Longitude + (cBins[j].Longitude-cBins[i].Longitude)/3},
						[2]float64{cBins[i].Latitude + 2*(cBins[j].Latitude-cBins[i].Latitude)/3, cBins[i].Longitude + 2*(cBins[j].Longitude-cBins[i].Longitude)/3},
					)
				}

				for _, pt := range points {
					candidates = append(candidates, candidate{
						Lat:          pt[0],
						Lng:          pt[1],
						City:         city,
						Zip:          cBins[i].Zip,
						AreaFillRate: fillRate,
					})
				}
			}
		}
	}

	if len(candidates) == 0 {
		return `{"count":0,"recommendations":[],"message":"No geographic gaps found between existing bins. All areas appear well-covered."}`, nil
	}

	// Step 6: Filter out candidates near no-go zones
	var filtered []candidate
	for _, c := range candidates {
		inNoGo := false
		for _, z := range zones {
			dist := haversineMetersChat(c.Lat, c.Lng, z.CenterLat, z.CenterLng)
			if dist <= float64(z.RadiusMeters) {
				inNoGo = true
				break
			}
		}
		if !inNoGo {
			filtered = append(filtered, c)
		}
	}
	candidates = filtered

	// Step 7: Deduplicate candidates too close to each other (within 0.1 mile)
	var deduped []candidate
	for _, c := range candidates {
		tooClose := false
		for _, d := range deduped {
			if haversineDistMiles(c.Lat, c.Lng, d.Lat, d.Lng) < 0.1 {
				tooClose = true
				break
			}
		}
		if !tooClose {
			deduped = append(deduped, c)
		}
	}
	candidates = deduped

	// Step 8: Filter candidates too close to existing bins
	var gapped []candidate
	for _, c := range candidates {
		minDist := math.MaxFloat64
		nearestNum := 0
		for _, b := range bins {
			d := haversineDistMiles(c.Lat, c.Lng, b.Latitude, b.Longitude)
			if d < minDist {
				minDist = d
				nearestNum = b.BinNumber
			}
		}
		if minDist >= minGapMiles {
			c.NearestBinNum = nearestNum
			c.NearestBinDist = math.Round(minDist*10) / 10
			gapped = append(gapped, c)
		}
	}
	candidates = gapped

	// Step 9: Score candidates
	// Normalize fill rates
	maxRate := 0.0
	for _, c := range candidates {
		if c.AreaFillRate > maxRate {
			maxRate = c.AreaFillRate
		}
	}
	if maxRate <= 0 {
		maxRate = 1
	}

	for i := range candidates {
		fillScore := candidates[i].AreaFillRate / maxRate // 0-1

		// Income score
		incomeScore := 1.0
		if income, ok := zipIncome[candidates[i].Zip]; ok && income > 0 {
			incomeScore = float64(income) / 100000.0
			if incomeScore > 2.0 {
				incomeScore = 2.0
			}
		}

		// Gap distance bonus (bigger gaps = more valuable, caps at 2 miles)
		gapBonus := math.Min(candidates[i].NearestBinDist, 2.0) / 2.0

		candidates[i].Score = math.Round((fillScore*0.5+gapBonus*0.3+incomeScore*0.2)*100) / 10
	}

	// Step 10: Sort by score and take top N
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	if len(candidates) > count {
		candidates = candidates[:count]
	}

	// Step 11: Filter malls/supermarkets via HERE Browse API and reverse geocode
	var recommendations []LocationRecommendation
	for _, c := range candidates {
		// Check if near mall/supermarket
		nearMall, _ := isNearMallOrSupermarket(c.Lat, c.Lng)
		if nearMall {
			log.Printf("🏬 [Chat] Filtered out candidate at %.4f,%.4f — near mall/supermarket", c.Lat, c.Lng)
			continue
		}

		// Reverse geocode to get address
		address, zip := reverseGeocodeHERE(c.Lat, c.Lng)
		if zip != "" {
			c.Zip = zip
		}

		// Build reasoning
		reasoning := fmt.Sprintf("%.1f mi gap from nearest bin #%d", c.NearestBinDist, c.NearestBinNum)
		reasoning += fmt.Sprintf(", area fill rate %.1f%%/day", c.AreaFillRate)
		if income, ok := zipIncome[c.Zip]; ok && income > 0 {
			reasoning += fmt.Sprintf(", median income $%dk", income/1000)
		}

		rec := LocationRecommendation{
			Latitude:        math.Round(c.Lat*10000) / 10000,
			Longitude:       math.Round(c.Lng*10000) / 10000,
			Address:         address,
			City:            c.City,
			Zip:             c.Zip,
			Score:           c.Score,
			Reasoning:       reasoning,
			NearestBinNum:   c.NearestBinNum,
			NearestBinDist:  c.NearestBinDist,
			AreaAvgFillRate: math.Round(c.AreaFillRate*10) / 10,
			MedianIncome:    zipIncome[c.Zip],
		}
		recommendations = append(recommendations, rec)

		if len(recommendations) >= count {
			break
		}
	}

	result, _ := json.Marshal(map[string]any{
		"count":           len(recommendations),
		"recommendations": recommendations,
	})
	return string(result), nil
}

// haversineDistMiles calculates distance in miles between two coordinates
func haversineDistMiles(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusMiles = 3958.8
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMiles * c
}

// isNearMallOrSupermarket checks if a location is near a shopping mall or Safeway via HERE Places API.
// Uses two calls: one for shopping malls (by category), one for Safeway (by name).
func isNearMallOrSupermarket(lat, lng float64) (bool, error) {
	client := &http.Client{Timeout: 5 * time.Second}

	// Check 1: Shopping malls nearby (200m radius)
	// HERE category 600-6900-0098 = Warehouse/Wholesale, but for malls we use the places search with name
	mallURL := fmt.Sprintf(
		"https://browse.search.hereapi.com/v1/browse?at=%.6f,%.6f&limit=3&in=circle:%.6f,%.6f;r=200&name=mall&apiKey=%s",
		lat, lng, lat, lng, HereAPIKey,
	)
	if found, _ := hereBrowseHasResults(client, mallURL); found {
		return true, nil
	}

	// Check 2: Safeway nearby (200m radius)
	safewayURL := fmt.Sprintf(
		"https://browse.search.hereapi.com/v1/browse?at=%.6f,%.6f&limit=3&in=circle:%.6f,%.6f;r=200&name=Safeway&apiKey=%s",
		lat, lng, lat, lng, HereAPIKey,
	)
	if found, _ := hereBrowseHasResults(client, safewayURL); found {
		return true, nil
	}

	return false, nil
}

// hereBrowseHasResults makes a HERE Browse API call and returns true if any items are found
func hereBrowseHasResults(client *http.Client, url string) (bool, error) {
	resp, err := client.Get(url)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false, nil // fail open
	}

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Items []any `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return false, nil
	}

	return len(result.Items) > 0, nil
}

// reverseGeocodeHERE reverse geocodes a coordinate to an address using HERE API
func reverseGeocodeHERE(lat, lng float64) (string, string) {
	url := fmt.Sprintf(
		"https://revgeocode.search.hereapi.com/v1/revgeocode?at=%.6f,%.6f&apiKey=%s",
		lat, lng, HereAPIKey,
	)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Sprintf("%.4f, %.4f", lat, lng), ""
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Items []struct {
			Address struct {
				Label      string `json:"label"`
				PostalCode string `json:"postalCode"`
			} `json:"address"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Items) == 0 {
		return fmt.Sprintf("%.4f, %.4f", lat, lng), ""
	}

	return result.Items[0].Address.Label, result.Items[0].Address.PostalCode
}
