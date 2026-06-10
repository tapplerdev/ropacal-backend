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

const maxGapMiles = 2.0     // Don't recommend locations > 2 miles from any bin
const minBinsPerCity = 3    // Need at least 3 bins in a city to prove demand
const minDedupeDistMiles = 0.15 // Dedup candidates within 0.15 miles of each other

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

	// Step 2: Calculate avg fill rate per city from check history
	type cityRate struct {
		City     string  `db:"city"`
		AvgRate  float64 `db:"avg_rate"`
		BinCount int     `db:"bin_count"`
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
		SELECT city,
			AVG((fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400)) as avg_rate,
			COUNT(DISTINCT bin_id) as bin_count
		FROM ordered_checks
		WHERE prev_fill IS NOT NULL
			AND fill_percentage > prev_fill
			AND (checked_on - prev_checked_on) > 3600
			AND (fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400) < 50
		GROUP BY city
	`)
	cityFillRate := map[string]float64{}
	cityBinCount := map[string]int{}
	for _, r := range rates {
		cityFillRate[r.City] = r.AvgRate
		cityBinCount[r.City] = r.BinCount
		log.Printf("📍 [Recommend] City %s: fill rate %.1f%%/day, %d bins with check data", r.City, r.AvgRate, r.BinCount)
	}

	// Step 3: Get active no-go zones
	var zones []noGoZone
	h.db.Select(&zones, `
		SELECT center_latitude, center_longitude, GREATEST(radius_meters, 500) as radius_meters
		FROM no_go_zones WHERE status = 'active' AND merged_into_zone_id IS NULL
	`)
	log.Printf("📍 [Recommend] Loaded %d active no-go zones", len(zones))

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
	log.Printf("📍 [Recommend] Loaded %d zip code income records", len(incomes))

	// Step 5: Generate candidate locations — midpoints between distant bins in same city
	// Only consider cities with enough bins to prove demand
	var candidates []candidate

	// Group bins by city
	allCityBins := map[string][]existingBin{}
	for _, b := range bins {
		allCityBins[b.City] = append(allCityBins[b.City], b)
	}

	for city, cBins := range allCityBins {
		// Skip cities with too few bins — not enough data to prove demand
		if len(cBins) < minBinsPerCity {
			log.Printf("📍 [Recommend] Skipping %s — only %d bins (need %d)", city, len(cBins), minBinsPerCity)
			continue
		}

		fillRate := cityFillRate[city]
		if fillRate <= 0 {
			fillRate = 5.0 // default
		}

		cityGenerated := 0
		for i := 0; i < len(cBins); i++ {
			for j := i + 1; j < len(cBins); j++ {
				dist := haversineDistMiles(cBins[i].Latitude, cBins[i].Longitude, cBins[j].Latitude, cBins[j].Longitude)
				if dist < minGapMiles*2 {
					continue // bins too close, no gap to fill
				}

				// Cap: don't generate candidates from bins that are too far apart
				// (midpoint would land in a random area with no relevance)
				if dist > maxGapMiles*2 {
					continue
				}

				// Generate midpoint
				midLat := (cBins[i].Latitude + cBins[j].Latitude) / 2
				midLng := (cBins[i].Longitude + cBins[j].Longitude) / 2

				// Also generate 1/3 and 2/3 points for wider gaps (but still within cap)
				points := [][2]float64{{midLat, midLng}}
				if dist > minGapMiles*4 && dist <= maxGapMiles*2 {
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

	// Step 6: Filter out candidates near no-go zones
	var filtered []candidate
	noGoFiltered := 0
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
		} else {
			noGoFiltered++
		}
	}
	candidates = filtered
	log.Printf("📍 [Recommend] After no-go zone filter: %d candidates (%d removed)", len(candidates), noGoFiltered)

	// Step 7: Deduplicate candidates too close to each other
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
	log.Printf("📍 [Recommend] After dedup: %d candidates (removed %d duplicates)", len(deduped), len(candidates)-len(deduped))
	candidates = deduped

	// Step 8: Filter candidates too close to existing bins OR too far from any bin
	var gapped []candidate
	tooCloseCount := 0
	tooFarCount := 0
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
		if minDist < minGapMiles {
			tooCloseCount++
			continue // too close to an existing bin
		}
		if minDist > maxGapMiles {
			tooFarCount++
			continue // too far from any bin — likely in a dead zone
		}
		c.NearestBinNum = nearestNum
		c.NearestBinDist = math.Round(minDist*10) / 10
		gapped = append(gapped, c)
	}
	log.Printf("📍 [Recommend] After gap filter: %d candidates (removed %d too close, %d too far)", len(gapped), tooCloseCount, tooFarCount)
	candidates = gapped

	if len(candidates) == 0 {
		return `{"count":0,"recommendations":[],"message":"No suitable gaps found. Existing bins cover the area well, or all gaps fall within no-go zones."}`, nil
	}

	// Step 9: Score candidates
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

		// Gap distance bonus — sweet spot is 0.5-1.5 miles
		// Too close = not needed, too far = might be dead zone
		gapBonus := math.Min(candidates[i].NearestBinDist, maxGapMiles) / maxGapMiles

		candidates[i].Score = math.Round((fillScore*0.5+gapBonus*0.3+incomeScore*0.2)*100) / 10
	}

	// Step 10: Sort by score, take more than needed (to account for mall/geocode filtering)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	// Take 2x what we need to account for filtering losses
	takeCount := count * 2
	if takeCount > len(candidates) {
		takeCount = len(candidates)
	}
	candidates = candidates[:takeCount]
	log.Printf("📍 [Recommend] Top %d candidates selected for geocoding (need %d final)", takeCount, count)

	// Step 11: Filter malls/supermarkets via HERE Browse API, reverse geocode, validate address quality
	var recommendations []LocationRecommendation
	mallFiltered := 0
	badAddressFiltered := 0

	for _, c := range candidates {
		if len(recommendations) >= count {
			break
		}

		// Check if near mall/supermarket
		nearMall, _ := isNearMallOrSupermarket(c.Lat, c.Lng)
		if nearMall {
			log.Printf("🏬 [Recommend] Filtered: %.4f,%.4f — near mall/supermarket", c.Lat, c.Lng)
			mallFiltered++
			continue
		}

		// Reverse geocode to get address
		address, zip := reverseGeocodeHERE(c.Lat, c.Lng)
		if zip != "" {
			c.Zip = zip
		}

		// Filter out bad reverse geocodes — if address is just a city/county name
		// or doesn't contain a street number, it's not a real address
		if isVagueAddress(address, c.City) {
			log.Printf("🚫 [Recommend] Filtered: %.4f,%.4f — vague address %q", c.Lat, c.Lng, address)
			badAddressFiltered++
			continue
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
		log.Printf("✅ [Recommend] #%d: %s (score %.1f, gap %.1f mi)", len(recommendations), address, c.Score, c.NearestBinDist)
	}

	log.Printf("📍 [Recommend] Final: %d recommendations (filtered %d malls, %d bad addresses)", len(recommendations), mallFiltered, badAddressFiltered)

	result, _ := json.Marshal(map[string]any{
		"count":           len(recommendations),
		"recommendations": recommendations,
	})
	return string(result), nil
}

// isVagueAddress checks if a reverse geocoded address is too vague to be useful
// (e.g. just a city name, county name, or highway name without a street number)
func isVagueAddress(address string, city string) bool {
	lower := strings.ToLower(strings.TrimSpace(address))

	// If address is just coordinates, it's bad
	if strings.HasPrefix(lower, "37.") || strings.HasPrefix(lower, "-12") {
		return true
	}

	// If address doesn't contain any digit, it's probably just a place/area name
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

	// Check if address is just the city/county name
	if strings.EqualFold(address, city) {
		return true
	}

	// If address is very short (< 15 chars), likely just a partial name
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

	// Check 1: Shopping malls nearby (200m radius)
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
