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
	LocationType    string  `json:"location_type,omitempty"` // "commercial", "community", "residential"
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
	NearestFillRate float64
	NearestBinNum   int
	NearestBinDist  float64
	Score           float64
	TrafficScore    float64
	POIScore        float64
	LocationType    string
}

const maxGapMiles = 2.0
const minBinsPerCity = 3
const minDedupeDistMiles = 0.15

// Address keywords to filter out — not real bin placement spots
var badAddressKeywords = []string{
	"highway", "bikeway", "trail", "freeway", "expressway",
	"interchange", "ramp", "overpass", "underpass", "bridge",
	"railroad", "rail trail", "creek trail", "river trail",
}

// HERE category IDs for location classification
// Commercial indicators (great for bins — high visibility, parking)
var commercialCategoryPrefixes = []string{
	"700-7600", // Gas/Petrol Station
	"600-6000", // Convenience Store
	"600-6300", // Grocery (but we filter Safeway separately)
	"600-6400", // Drug Store/Pharmacy
	"600-6500", // Hardware/Home Improvement
	"600-6900", // Consumer Goods
	"100-1000", // Restaurant
	"100-1100", // Coffee/Tea
	"700-7850", // Car Wash
	"700-7900", // Auto Dealer/Service
}

// Community indicators (moderate for bins — some foot traffic)
var communityCategoryPrefixes = []string{
	"300-3200", // Church/Place of Worship
	"800-8200", // School
	"800-8100", // University/College
	"550-5510", // Park/Recreation
	"700-7400", // Post Office
	"700-7000", // Bank
}

func stripZipPlus4(zip string) string {
	if idx := strings.Index(zip, "-"); idx > 0 {
		return zip[:idx]
	}
	if len(zip) > 5 {
		return zip[:5]
	}
	return zip
}

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

	// Step 1: Get all active bins
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
	log.Printf("📍 [Recommend] Found %d active bins", len(bins))
	if len(bins) < 3 {
		return "", fmt.Errorf("need at least 3 active bins with coordinates (found %d)", len(bins))
	}

	// Step 2: Per-bin fill rates
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
			FROM checks WHERE fill_percentage IS NOT NULL
		)
		SELECT bin_id, AVG((fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400)) as avg_rate
		FROM ordered_checks
		WHERE prev_fill IS NOT NULL AND fill_percentage > prev_fill
			AND (checked_on - prev_checked_on) > 3600
			AND (fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400) < 50
		GROUP BY bin_id HAVING COUNT(*) >= 2
	`)
	perBinFillRate := map[string]float64{}
	for _, r := range binRates {
		perBinFillRate[r.BinID] = r.AvgRate
	}
	log.Printf("📍 [Recommend] Per-bin fill rates for %d bins", len(perBinFillRate))

	// Step 3: No-go zones
	var zones []noGoZone
	h.db.Select(&zones, `SELECT center_latitude, center_longitude, GREATEST(radius_meters, 500) as radius_meters FROM no_go_zones WHERE status = 'active' AND merged_into_zone_id IS NULL`)
	log.Printf("📍 [Recommend] %d active no-go zones", len(zones))

	// Step 4: Census data
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

	// Step 5: Generate candidates
	var candidates []candidate
	allCityBins := map[string][]existingBin{}
	for _, b := range bins {
		allCityBins[b.City] = append(allCityBins[b.City], b)
	}
	for city, cBins := range allCityBins {
		if len(cBins) < minBinsPerCity {
			log.Printf("📍 [Recommend] Skipping %s — only %d bins", city, len(cBins))
			continue
		}
		cityGenerated := 0
		for i := 0; i < len(cBins); i++ {
			for j := i + 1; j < len(cBins); j++ {
				dist := haversineDistMiles(cBins[i].Latitude, cBins[i].Longitude, cBins[j].Latitude, cBins[j].Longitude)
				if dist < minGapMiles*2 || dist > maxGapMiles*2 {
					continue
				}
				rate1 := perBinFillRate[cBins[i].ID]
				rate2 := perBinFillRate[cBins[j].ID]
				nearestRate := math.Max(rate1, rate2)
				if nearestRate <= 0 {
					nearestRate = 5.0
				}
				midLat := (cBins[i].Latitude + cBins[j].Latitude) / 2
				midLng := (cBins[i].Longitude + cBins[j].Longitude) / 2
				points := [][2]float64{{midLat, midLng}}
				if dist > minGapMiles*4 {
					points = append(points,
						[2]float64{cBins[i].Latitude + (cBins[j].Latitude-cBins[i].Latitude)/3, cBins[i].Longitude + (cBins[j].Longitude-cBins[i].Longitude)/3},
						[2]float64{cBins[i].Latitude + 2*(cBins[j].Latitude-cBins[i].Latitude)/3, cBins[i].Longitude + 2*(cBins[j].Longitude-cBins[i].Longitude)/3},
					)
				}
				for _, pt := range points {
					candidates = append(candidates, candidate{Lat: pt[0], Lng: pt[1], City: city, Zip: cBins[i].Zip, NearestFillRate: nearestRate})
					cityGenerated++
				}
			}
		}
		log.Printf("📍 [Recommend] %s: %d candidates from %d bins", city, cityGenerated, len(cBins))
	}
	log.Printf("📍 [Recommend] Total raw: %d", len(candidates))
	if len(candidates) == 0 {
		return `{"count":0,"recommendations":[],"message":"No geographic gaps found."}`, nil
	}

	// Step 6: Filter no-go zones
	var filtered []candidate
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
		}
	}
	candidates = filtered

	// Step 7: Dedup
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
	candidates = deduped

	// Step 8: Gap filter + attach nearest bin
	var gapped []candidate
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
		if minDist < minGapMiles || minDist > maxGapMiles {
			continue
		}
		c.NearestBinNum = nearestNum
		c.NearestBinDist = math.Round(minDist*10) / 10
		if rate, ok := perBinFillRate[nearestID]; ok && rate > 0 {
			c.NearestFillRate = rate
		}
		gapped = append(gapped, c)
	}
	candidates = gapped
	log.Printf("📍 [Recommend] After all filters: %d candidates", len(candidates))

	if len(candidates) == 0 {
		return `{"count":0,"recommendations":[],"message":"No suitable gaps found."}`, nil
	}

	// Step 9: Preliminary score (without traffic/POI — those come later for top candidates)
	maxRate := 0.0
	maxPop := 0
	for _, c := range candidates {
		if c.NearestFillRate > maxRate {
			maxRate = c.NearestFillRate
		}
		if pop := zipPopulation[stripZipPlus4(c.Zip)]; pop > maxPop {
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
		fillScore := candidates[i].NearestFillRate / maxRate
		gapScore := math.Min(candidates[i].NearestBinDist, maxGapMiles) / maxGapMiles
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
		// Preliminary score (POI=0.5, traffic=0.5 as defaults)
		candidates[i].Score = fillScore*0.25 + gapScore*0.15 + popScore*0.15 + 0.5*0.15 + incomeScore*0.10 + 0.5*0.20
	}

	// Step 10: Sort and take top candidates for enrichment
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	takeCount := count * 3 // take 3x to account for filtering
	if takeCount > len(candidates) {
		takeCount = len(candidates)
	}
	candidates = candidates[:takeCount]
	log.Printf("📍 [Recommend] Top %d candidates for enrichment", takeCount)

	// Step 11: Enrich with traffic, POI classification, geocode, and validate
	var recommendations []LocationRecommendation

	for _, c := range candidates {
		if len(recommendations) >= count {
			break
		}

		// Get traffic score
		trafficJam := getTrafficJamFactor(c.Lat, c.Lng)
		c.TrafficScore = trafficJam

		// Browse nearby POIs — classifies location AND checks for malls/Safeway in one call
		poiScore, locationType, nearMall := classifyLocation(c.Lat, c.Lng)
		c.POIScore = poiScore
		c.LocationType = locationType

		if nearMall {
			log.Printf("🏬 [Recommend] Filtered: %.4f,%.4f — near mall/supermarket", c.Lat, c.Lng)
			continue
		}

		// Reverse geocode
		address, zip := reverseGeocodeHERE(c.Lat, c.Lng)
		if zip != "" {
			c.Zip = zip
		}
		zip5 := stripZipPlus4(c.Zip)

		// Filter bad addresses
		if isVagueAddress(address, c.City) {
			log.Printf("🚫 [Recommend] Filtered: vague address %q", address)
			continue
		}
		if isBadAddressKeyword(address) {
			log.Printf("🚫 [Recommend] Filtered: bad keyword in %q", address)
			continue
		}

		// Recalculate final score with real data
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
		trafficNorm := math.Min(trafficJam, 10.0) / 10.0

		finalScore := math.Round((fillScore*0.25+gapScore*0.15+popScore*0.15+trafficNorm*0.15+incomeScore*0.10+poiScore*0.20)*100) / 10

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
			tLabel := "low"
			if trafficJam >= 5 {
				tLabel = "high"
			} else if trafficJam >= 2 {
				tLabel = "moderate"
			}
			reasoning += fmt.Sprintf(", %s traffic", tLabel)
		}
		if locationType != "" {
			reasoning += fmt.Sprintf(", %s area", locationType)
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
			LocationType:    locationType,
		}
		recommendations = append(recommendations, rec)
		log.Printf("✅ [Recommend] #%d: %s (score %.1f, fill %.1f, gap %.1f, traffic %.1f, POI %.1f/%s)",
			len(recommendations), address, finalScore, c.NearestFillRate, c.NearestBinDist, trafficJam, poiScore, locationType)
	}

	sort.Slice(recommendations, func(i, j int) bool { return recommendations[i].Score > recommendations[j].Score })
	log.Printf("📍 [Recommend] Final: %d recommendations", len(recommendations))

	result, _ := json.Marshal(map[string]any{"count": len(recommendations), "recommendations": recommendations})
	return string(result), nil
}

// classifyLocation calls HERE Browse API with no category filter to find nearby POIs,
// then classifies as commercial/community/residential based on what's found.
// Also returns whether a mall or Safeway is nearby (for filtering).
func classifyLocation(lat, lng float64) (poiScore float64, locationType string, nearMallOrSafeway bool) {
	url := fmt.Sprintf(
		"https://browse.search.hereapi.com/v1/browse?at=%.6f,%.6f&limit=10&in=circle:%.6f,%.6f;r=150&apiKey=%s",
		lat, lng, lat, lng, HereAPIKey,
	)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0.3, "unknown", false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0.3, "unknown", false
	}

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Items []struct {
			Title      string `json:"title"`
			Categories []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"categories"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0.3, "unknown", false
	}

	if len(result.Items) == 0 {
		return 0.2, "residential", false
	}

	hasCommercial := false
	hasCommunity := false

	for _, item := range result.Items {
		titleLower := strings.ToLower(item.Title)

		// Check for mall or Safeway
		if strings.Contains(titleLower, "mall") || strings.Contains(titleLower, "safeway") {
			nearMallOrSafeway = true
		}

		for _, cat := range item.Categories {
			// Check for mall category
			if cat.ID == "600-6100-0062" {
				nearMallOrSafeway = true
			}

			// Check commercial
			for _, prefix := range commercialCategoryPrefixes {
				if strings.HasPrefix(cat.ID, prefix) {
					hasCommercial = true
					break
				}
			}
			// Check community
			for _, prefix := range communityCategoryPrefixes {
				if strings.HasPrefix(cat.ID, prefix) {
					hasCommunity = true
					break
				}
			}
		}
	}

	if hasCommercial {
		return 1.0, "commercial", nearMallOrSafeway
	}
	if hasCommunity {
		return 0.6, "community", nearMallOrSafeway
	}
	return 0.3, "mixed", nearMallOrSafeway
}

// isBadAddressKeyword checks if address contains keywords that indicate a non-placement spot
func isBadAddressKeyword(address string) bool {
	lower := strings.ToLower(address)
	for _, kw := range badAddressKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

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

func getTrafficJamFactor(lat, lng float64) float64 {
	url := fmt.Sprintf(
		"https://data.traffic.hereapi.com/v7/flow?in=circle:%.6f,%.6f;r=100&locationReferencing=none&apiKey=%s",
		lat, lng, HereAPIKey,
	)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0
	}
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Results []struct {
			CurrentFlow struct {
				JamFactor float64 `json:"jamFactor"`
			} `json:"currentFlow"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Results) == 0 {
		return 0
	}
	maxJam := 0.0
	for _, r := range result.Results {
		if r.CurrentFlow.JamFactor > maxJam {
			maxJam = r.CurrentFlow.JamFactor
		}
	}
	return maxJam
}

func haversineDistMiles(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 3958.8
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return r * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func reverseGeocodeHERE(lat, lng float64) (string, string) {
	url := fmt.Sprintf("https://revgeocode.search.hereapi.com/v1/revgeocode?at=%.6f,%.6f&apiKey=%s", lat, lng, HereAPIKey)
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
