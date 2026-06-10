package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
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
	TrafficScore    float64 `json:"traffic_score,omitempty"`
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
	Lat             float64
	Lng             float64
	City            string
	Zip             string
	NearestFillRate float64 // per-bin fill rate of nearest bin
	NearestBinNum   int
	NearestBinDist  float64 // miles
	Score           float64
	TrafficScore    float64
}

const maxGapMiles = 2.0
const minBinsPerCity = 3
const minDedupeDistMiles = 0.15

// stripZipPlus4 returns the 5-digit zip from a zip+4 like "95125-4500"
func stripZipPlus4(zip string) string {
	if idx := strings.Index(zip, "-"); idx > 0 {
		return zip[:idx]
	}
	if len(zip) > 5 {
		return zip[:5]
	}
	return zip
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

	log.Printf("📍 [Recommend] Starting: count=%d, city=%q, minGap=%.1f mi", count, targetCity, minGapMiles)

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
	log.Printf("📍 [Recommend] Found %d active bins with coordinates", len(bins))

	if len(bins) < 3 {
		return "", fmt.Errorf("need at least 3 active bins with coordinates to generate recommendations (found %d)", len(bins))
	}

	// Step 2: Calculate per-bin fill rates from check history
	type binRate struct {
		BinID   string  `db:"bin_id"`
		AvgRate float64 `db:"avg_rate"`
	}
	var binRates []binRate
	h.db.Select(&binRates, `
		WITH ordered_checks AS (
			SELECT bin_id, fill_percentage, checked_on,
				LAG(fill_percentage) OVER (PARTITION BY bin_id ORDER BY checked_on) as prev_fill,
				LAG(checked_on) OVER (PARTITION BY bin_id ORDER BY checked_on) as prev_checked_on
			FROM checks
			WHERE fill_percentage IS NOT NULL
		)
		SELECT bin_id,
			AVG((fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400)) as avg_rate
		FROM ordered_checks
		WHERE prev_fill IS NOT NULL
			AND fill_percentage > prev_fill
			AND (checked_on - prev_checked_on) > 3600
			AND (fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400) < 50
		GROUP BY bin_id
		HAVING COUNT(*) >= 2
	`)
	perBinFillRate := map[string]float64{}
	for _, r := range binRates {
		perBinFillRate[r.BinID] = r.AvgRate
	}
	log.Printf("📍 [Recommend] Calculated per-bin fill rates for %d bins", len(perBinFillRate))

	// Step 3: Get active no-go zones
	var zones []noGoZone
	h.db.Select(&zones, `
		SELECT center_latitude, center_longitude, GREATEST(radius_meters, 500) as radius_meters
		FROM no_go_zones WHERE status = 'active' AND merged_into_zone_id IS NULL
	`)
	log.Printf("📍 [Recommend] Loaded %d active no-go zones", len(zones))

	// Step 4: Get census data (income + population)
	type censusRow struct {
		Zip        string `db:"zip"`
		Income     int    `db:"median_household_income"`
		Population int    `db:"population"`
	}
	var census []censusRow
	h.db.Select(&census, `SELECT zip, median_household_income, population FROM census_income_cache WHERE median_household_income > 0`)
	zipIncome := map[string]int{}
	zipPopulation := map[string]int{}
	for _, c := range census {
		zipIncome[c.Zip] = c.Income
		zipPopulation[c.Zip] = c.Population
	}
	log.Printf("📍 [Recommend] Loaded %d zip code census records (income + population)", len(census))

	// Step 5: Generate candidate locations — midpoints between distant bins in same city
	var candidates []candidate

	// Group bins by city
	allCityBins := map[string][]existingBin{}
	for _, b := range bins {
		allCityBins[b.City] = append(allCityBins[b.City], b)
	}

	for city, cBins := range allCityBins {
		if len(cBins) < minBinsPerCity {
			log.Printf("📍 [Recommend] Skipping %s — only %d bins (need %d)", city, len(cBins), minBinsPerCity)
			continue
		}

		cityGenerated := 0
		for i := 0; i < len(cBins); i++ {
			for j := i + 1; j < len(cBins); j++ {
				dist := haversineDistMiles(cBins[i].Latitude, cBins[i].Longitude, cBins[j].Latitude, cBins[j].Longitude)
				if dist < minGapMiles*2 || dist > maxGapMiles*2 {
					continue
				}

				// Use the higher fill rate of the two neighboring bins
				rate1 := perBinFillRate[cBins[i].ID]
				rate2 := perBinFillRate[cBins[j].ID]
				nearestRate := math.Max(rate1, rate2)
				if nearestRate <= 0 {
					nearestRate = 5.0 // default if no check data
				}

				midLat := (cBins[i].Latitude + cBins[j].Latitude) / 2
				midLng := (cBins[i].Longitude + cBins[j].Longitude) / 2

				points := [][2]float64{{midLat, midLng}}
				if dist > minGapMiles*4 && dist <= maxGapMiles*2 {
					points = append(points,
						[2]float64{cBins[i].Latitude + (cBins[j].Latitude-cBins[i].Latitude)/3, cBins[i].Longitude + (cBins[j].Longitude-cBins[i].Longitude)/3},
						[2]float64{cBins[i].Latitude + 2*(cBins[j].Latitude-cBins[i].Latitude)/3, cBins[i].Longitude + 2*(cBins[j].Longitude-cBins[i].Longitude)/3},
					)
				}

				for _, pt := range points {
					candidates = append(candidates, candidate{
						Lat:             pt[0],
						Lng:             pt[1],
						City:            city,
						Zip:             cBins[i].Zip,
						NearestFillRate: nearestRate,
					})
					cityGenerated++
				}
			}
		}
		log.Printf("📍 [Recommend] %s: generated %d candidates from %d bins", city, cityGenerated, len(cBins))
	}

	log.Printf("📍 [Recommend] Total raw candidates: %d", len(candidates))
	if len(candidates) == 0 {
		return `{"count":0,"recommendations":[],"message":"No geographic gaps found between existing bins. All areas appear well-covered."}`, nil
	}

	// Step 6: Filter no-go zones
	var filtered []candidate
	noGoFiltered := 0
	for _, c := range candidates {
		inNoGo := false
		for _, z := range zones {
			if haversineMetersChat(c.Lat, c.Lng, z.CenterLat, z.CenterLng) <= float64(z.RadiusMeters) {
				inNoGo = true
				break
			}
		}
		if !inNoGo {
			filtered = append(filtered, c)
		} else {
			noGoFiltered++
		}
	}
	candidates = filtered
	log.Printf("📍 [Recommend] After no-go zone filter: %d candidates (%d removed)", len(candidates), noGoFiltered)

	// Step 7: Deduplicate
	var deduped []candidate
	for _, c := range candidates {
		tooClose := false
		for _, d := range deduped {
			if haversineDistMiles(c.Lat, c.Lng, d.Lat, d.Lng) < minDedupeDistMiles {
				tooClose = true
				break
			}
		}
		if !tooClose {
			deduped = append(deduped, c)
		}
	}
	log.Printf("📍 [Recommend] After dedup: %d candidates (removed %d)", len(deduped), len(candidates)-len(deduped))
	candidates = deduped

	// Step 8: Filter by distance to existing bins + attach nearest bin info
	var gapped []candidate
	tooCloseCount, tooFarCount := 0, 0
	for _, c := range candidates {
		minDist := math.MaxFloat64
		nearestNum := 0
		nearestID := ""
		for _, b := range bins {
			d := haversineDistMiles(c.Lat, c.Lng, b.Latitude, b.Longitude)
			if d < minDist {
				minDist = d
				nearestNum = b.BinNumber
				nearestID = b.ID
			}
		}
		if minDist < minGapMiles {
			tooCloseCount++
			continue
		}
		if minDist > maxGapMiles {
			tooFarCount++
			continue
		}
		c.NearestBinNum = nearestNum
		c.NearestBinDist = math.Round(minDist*10) / 10
		// Use the actual nearest bin's fill rate
		if rate, ok := perBinFillRate[nearestID]; ok && rate > 0 {
			c.NearestFillRate = rate
		}
		gapped = append(gapped, c)
	}
	log.Printf("📍 [Recommend] After gap filter: %d candidates (removed %d close, %d far)", len(gapped), tooCloseCount, tooFarCount)
	candidates = gapped

	if len(candidates) == 0 {
		return `{"count":0,"recommendations":[],"message":"No suitable gaps found. Existing bins cover the area well, or all gaps fall within no-go zones."}`, nil
	}

	// Step 9: Score candidates with improved formula
	// New weights: fill_rate 30%, gap 20%, population 20%, traffic 15%, income 15%
	maxRate := 0.0
	maxPop := 0
	for _, c := range candidates {
		if c.NearestFillRate > maxRate {
			maxRate = c.NearestFillRate
		}
		zip5 := stripZipPlus4(c.Zip)
		if pop := zipPopulation[zip5]; pop > maxPop {
			maxPop = pop
		}
	}
	if maxRate <= 0 {
		maxRate = 1
	}
	if maxPop <= 0 {
		maxPop = 1
	}

	for i := range candidates {
		zip5 := stripZipPlus4(candidates[i].Zip)

		// Fill rate score (30%) — per-bin, not city average
		fillScore := candidates[i].NearestFillRate / maxRate

		// Gap distance score (20%)
		gapScore := math.Min(candidates[i].NearestBinDist, maxGapMiles) / maxGapMiles

		// Population density score (20%)
		popScore := 0.5 // default if no data
		if pop, ok := zipPopulation[zip5]; ok && pop > 0 {
			popScore = float64(pop) / float64(maxPop)
		}

		// Income score (15%)
		incomeScore := 0.5 // default
		if income, ok := zipIncome[zip5]; ok && income > 0 {
			incomeScore = float64(income) / 150000.0
			if incomeScore > 1.5 {
				incomeScore = 1.5
			}
		}

		// Traffic placeholder (15%) — will be filled in Step 11 for top candidates
		trafficScore := 0.5 // default, updated later for top candidates

		candidates[i].Score = math.Round((fillScore*0.30+gapScore*0.20+popScore*0.20+trafficScore*0.15+incomeScore*0.15)*100) / 10
	}

	// Step 10: Sort and take top candidates for traffic lookup + geocoding
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	takeCount := count * 2
	if takeCount > len(candidates) {
		takeCount = len(candidates)
	}
	candidates = candidates[:takeCount]
	log.Printf("📍 [Recommend] Top %d candidates selected for traffic lookup + geocoding", takeCount)

	// Step 11: Enrich top candidates with HERE Traffic flow data, filter malls, geocode
	var recommendations []LocationRecommendation
	mallFiltered, badAddrFiltered := 0, 0

	for _, c := range candidates {
		if len(recommendations) >= count {
			break
		}

		// Check mall/Safeway
		nearMall, _ := isNearMallOrSupermarket(c.Lat, c.Lng)
		if nearMall {
			log.Printf("🏬 [Recommend] Filtered: %.4f,%.4f — near mall/supermarket", c.Lat, c.Lng)
			mallFiltered++
			continue
		}

		// Get traffic score from HERE
		trafficJam := getTrafficJamFactor(c.Lat, c.Lng)
		c.TrafficScore = trafficJam

		// Reverse geocode
		address, zip := reverseGeocodeHERE(c.Lat, c.Lng)
		if zip != "" {
			c.Zip = zip
		}
		zip5 := stripZipPlus4(c.Zip)

		// Filter bad addresses
		if isVagueAddress(address, c.City) {
			log.Printf("🚫 [Recommend] Filtered: %.4f,%.4f — vague address %q", c.Lat, c.Lng, address)
			badAddrFiltered++
			continue
		}

		// Recalculate final score with real traffic data
		fillScore := c.NearestFillRate / maxRate
		gapScore := math.Min(c.NearestBinDist, maxGapMiles) / maxGapMiles
		popScore := 0.5
		if pop, ok := zipPopulation[zip5]; ok && pop > 0 {
			popScore = float64(pop) / float64(maxPop)
		}
		incomeScore := 0.5
		if income, ok := zipIncome[zip5]; ok && income > 0 {
			incomeScore = float64(income) / 150000.0
			if incomeScore > 1.5 {
				incomeScore = 1.5
			}
		}
		// Traffic: jamFactor 0-10, higher = busier = better for us
		trafficNorm := math.Min(trafficJam, 10.0) / 10.0

		finalScore := math.Round((fillScore*0.30+gapScore*0.20+popScore*0.20+trafficNorm*0.15+incomeScore*0.15)*100) / 10

		// Build reasoning
		reasoning := fmt.Sprintf("%.1f mi gap from bin #%d", c.NearestBinDist, c.NearestBinNum)
		reasoning += fmt.Sprintf(", fill rate %.1f%%/day", c.NearestFillRate)
		if income, ok := zipIncome[zip5]; ok && income > 0 {
			reasoning += fmt.Sprintf(", income $%dk", income/1000)
		}
		if pop, ok := zipPopulation[zip5]; ok && pop > 0 {
			reasoning += fmt.Sprintf(", pop %dk", pop/1000)
		}
		if trafficJam > 0 {
			trafficLabel := "low"
			if trafficJam >= 5 {
				trafficLabel = "high"
			} else if trafficJam >= 2 {
				trafficLabel = "moderate"
			}
			reasoning += fmt.Sprintf(", %s traffic", trafficLabel)
		}

		rec := LocationRecommendation{
			Latitude:        math.Round(c.Lat*10000) / 10000,
			Longitude:       math.Round(c.Lng*10000) / 10000,
			Address:         address,
			City:            c.City,
			Zip:             c.Zip,
			Score:           finalScore,
			Reasoning:       reasoning,
			NearestBinNum:   c.NearestBinNum,
			NearestBinDist:  c.NearestBinDist,
			AreaAvgFillRate: math.Round(c.NearestFillRate*10) / 10,
			MedianIncome:    zipIncome[zip5],
			TrafficScore:    math.Round(trafficJam*10) / 10,
		}
		recommendations = append(recommendations, rec)
		log.Printf("✅ [Recommend] #%d: %s (score %.1f, fill %.1f, gap %.1f mi, traffic %.1f, pop %dk, income $%dk)",
			len(recommendations), address, finalScore, c.NearestFillRate, c.NearestBinDist,
			trafficJam, zipPopulation[zip5]/1000, zipIncome[zip5]/1000)
	}

	// Re-sort by final score (since traffic data may have changed rankings)
	sort.Slice(recommendations, func(i, j int) bool {
		return recommendations[i].Score > recommendations[j].Score
	})

	log.Printf("📍 [Recommend] Final: %d recommendations (filtered %d malls, %d bad addresses)", len(recommendations), mallFiltered, badAddrFiltered)

	result, _ := json.Marshal(map[string]any{
		"count":           len(recommendations),
		"recommendations": recommendations,
	})
	return string(result), nil
}

// getTrafficJamFactor queries HERE Traffic Flow API for the jam factor at a location.
// Returns 0-10 where 0=free flow, 10=gridlock. Higher = busier road = better visibility.
func getTrafficJamFactor(lat, lng float64) float64 {
	url := fmt.Sprintf(
		"https://data.traffic.hereapi.com/v7/flow?in=circle:%.6f,%.6f;r=100&locationReferencing=none&apiKey=%s",
		lat, lng, HereAPIKey,
	)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("⚠️ [Traffic] API error at %.4f,%.4f: %v", lat, lng, err)
		return 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("⚠️ [Traffic] HTTP %d at %.4f,%.4f", resp.StatusCode, lat, lng)
		return 0
	}

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Results []struct {
			CurrentFlow struct {
				JamFactor float64 `json:"jamFactor"`
				Speed     float64 `json:"speed"`
			} `json:"currentFlow"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Results) == 0 {
		return 0
	}

	// Return the highest jam factor from nearby road segments
	maxJam := 0.0
	for _, r := range result.Results {
		if r.CurrentFlow.JamFactor > maxJam {
			maxJam = r.CurrentFlow.JamFactor
		}
	}
	return maxJam
}

// isVagueAddress checks if a reverse geocoded address is too vague to be useful
func isVagueAddress(address string, city string) bool {
	lower := strings.ToLower(strings.TrimSpace(address))
	if strings.HasPrefix(lower, "37.") || strings.HasPrefix(lower, "-12") {
		return true
	}
	hasDigit := false
	for _, c := range address {
		if c >= '0' && c <= '9' {
			hasDigit = true
			break
		}
	}
	if !hasDigit {
		return true
	}
	if strings.EqualFold(address, city) {
		return true
	}
	if len(address) < 15 {
		return true
	}
	return false
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
func isNearMallOrSupermarket(lat, lng float64) (bool, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	mallURL := fmt.Sprintf(
		"https://browse.search.hereapi.com/v1/browse?at=%.6f,%.6f&limit=3&in=circle:%.6f,%.6f;r=200&name=mall&apiKey=%s",
		lat, lng, lat, lng, HereAPIKey,
	)
	if found, _ := hereBrowseHasResults(client, mallURL); found {
		return true, nil
	}
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
		return false, nil
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
