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
	LocationType    string  `json:"location_type,omitempty"`
	Source          string  `json:"source,omitempty"` // "gap_fill" or "expansion"
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
	NearbyPOI       string // name of the whitelisted POI that matched (e.g. "Chevron Gas")
	Source          string // "gap_fill" or "expansion"
}

const maxGapMiles = 2.0
const minBinsPerCity = 3
const minDedupeDistMiles = 0.15

var badAddressKeywords = []string{
	"highway", "hwy", "bikeway", "trail", "trl", "freeway", "fwy",
	"expressway", "expy", "interchange", "ramp", "overpass", "underpass",
	"bridge", "railroad", "creek", "river trl", "river trail",
}

// Whitelisted retail POI categories — places regular people visit where bins perform well.
// If none of these are within 800m, the candidate is discarded.
var retailWhitelist = []struct {
	Prefix string
	Label  string
}{
	{"700-7600", "gas station"},
	{"600-6000", "convenience store"},
	{"600-6300", "grocery store"},
	{"600-6400", "pharmacy"},
	{"600-6900", "retail store"},
	{"600-6500", "hardware store"},
	{"100-1000", "restaurant"},
	{"100-1100", "coffee shop"},
	{"700-7850", "car wash"},
	{"200-2000", "bar/pub"},
}

// Community POIs — acceptable but lower priority than retail
var communityWhitelist = []struct {
	Prefix string
	Label  string
}{
	{"300-3200", "church"},
	{"550-5510", "park"},
	{"800-8200", "school"},
	{"800-8100", "college"},
	{"700-7400", "post office"},
	{"700-7000", "bank"},
}

const poiBrowseRadiusM = 800 // Search radius for POI classification (meters)

// B2B/service business keywords in POI titles — these are NOT consumer foot traffic
// even if their HERE category says "restaurant" or "car wash"
var b2bTitleKeywords = []string{
	"repair", "service", "detail", "detailing", "printing", "supply",
	"wholesale", "distribution", "logistics", "warehouse", "industrial",
	"manufacturing", "auto body", "autobody", "towing", "welding",
	"plumbing", "electric", "roofing", "contractor", "construction",
	"equipment", "machinery", "parts", "scrap", "salvage", "recycling",
	"storage", "moving", "freight", "shipping", "paving",
}

// Bay Area cities for expansion mode — places we could expand to
var expansionCities = []struct {
	City string
	Lat  float64
	Lng  float64
}{
	{"Fremont", 37.5485, -121.9886},
	{"Hayward", 37.6688, -122.0808},
	{"Oakland", 37.8044, -122.2712},
	{"Berkeley", 37.8716, -122.2727},
	{"Union City", 37.5934, -122.0439},
	{"Newark", 37.5297, -122.0402},
	{"Milpitas", 37.4323, -121.8996},
	{"San Leandro", 37.7249, -122.1561},
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

	// Step 1: Get all active bins (for gap detection)
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

	// Step 2: Per-bin fill rates from ALL bins (active + retired + missing) — full history
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

	// Also compute per-zip average fill rate (from all bins, including retired)
	type zipRate struct {
		Zip     string  `db:"zip"`
		AvgRate float64 `db:"avg_rate"`
	}
	var zipRates []zipRate
	h.db.Select(&zipRates, `
		WITH ordered_checks AS (
			SELECT c.bin_id, c.fill_percentage, c.checked_on, b.zip,
				LAG(c.fill_percentage) OVER (PARTITION BY c.bin_id ORDER BY c.checked_on) as prev_fill,
				LAG(c.checked_on) OVER (PARTITION BY c.bin_id ORDER BY c.checked_on) as prev_checked_on
			FROM checks c JOIN bins b ON c.bin_id = b.id
			WHERE c.fill_percentage IS NOT NULL
		)
		SELECT SUBSTRING(zip FROM 1 FOR 5) as zip, AVG((fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400)) as avg_rate
		FROM ordered_checks
		WHERE prev_fill IS NOT NULL AND fill_percentage > prev_fill
			AND (checked_on - prev_checked_on) > 3600
			AND (fill_percentage - prev_fill)::float / GREATEST(1, (checked_on - prev_checked_on)::float / 86400) < 50
		GROUP BY SUBSTRING(zip FROM 1 FOR 5)
	`)
	zipFillRate := map[string]float64{}
	for _, r := range zipRates {
		zipFillRate[r.Zip] = r.AvgRate
	}
	log.Printf("📍 [Recommend] Fill rates: %d per-bin, %d per-zip (all history)", len(perBinFillRate), len(zipFillRate))

	// Step 3: No-go zones
	var zones []noGoZone
	h.db.Select(&zones, `SELECT center_latitude, center_longitude, GREATEST(radius_meters, 500) as radius_meters FROM no_go_zones WHERE status = 'active' AND merged_into_zone_id IS NULL`)

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

	// ======================================================================
	// STRATEGY A: Gap-fill candidates (70% of results) — near existing bins
	// ======================================================================
	var gapCandidates []candidate
	if len(bins) >= 3 {
		allCityBins := map[string][]existingBin{}
		for _, b := range bins {
			allCityBins[b.City] = append(allCityBins[b.City], b)
		}
		for city, cBins := range allCityBins {
			if len(cBins) < minBinsPerCity {
				log.Printf("📍 [Recommend] Skipping %s — only %d bins", city, len(cBins))
				continue
			}
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
						gapCandidates = append(gapCandidates, candidate{
							Lat: pt[0], Lng: pt[1], City: city, Zip: cBins[i].Zip,
							NearestFillRate: nearestRate, Source: "gap_fill",
						})
					}
				}
			}
		}
	}
	log.Printf("📍 [Recommend] Gap-fill raw candidates: %d", len(gapCandidates))

	// Filter gap candidates: no-go zones, dedup, distance
	gapCandidates = filterNoGoZones(gapCandidates, zones)
	gapCandidates = deduplicateCandidates(gapCandidates)
	gapCandidates = filterByDistance(gapCandidates, bins, minGapMiles, maxGapMiles, perBinFillRate)
	log.Printf("📍 [Recommend] Gap-fill after filters: %d", len(gapCandidates))

	// ======================================================================
	// STRATEGY B: Expansion candidates (30% of results) — new areas
	// ======================================================================
	expansionCount := int(math.Ceil(float64(count) * 0.3))
	gapCount := count - expansionCount

	var expCandidates []candidate
	// Only generate expansion candidates if not targeting a specific city with existing bins
	shouldExpand := targetCity == "" || !hasBinsInCity(bins, targetCity)

	if shouldExpand {
		// Use expansion cities or the target city if specified
		cities := expansionCities
		if targetCity != "" {
			// User asked for a specific new city — generate candidates there
			cities = []struct {
				City string
				Lat  float64
				Lng  float64
			}{{targetCity, 0, 0}} // lat/lng will be geocoded
		}

		for _, ec := range cities {
			if targetCity != "" && !strings.EqualFold(ec.City, targetCity) {
				continue
			}
			// Skip if we already have bins there
			if hasBinsInCity(bins, ec.City) {
				continue
			}

			// For expansion: search for commercial POIs in this city using HERE Browse
			expPOIs := findCommercialPOIs(ec.Lat, ec.Lng, ec.City)
			for _, poi := range expPOIs {
				// Check no-go zones
				inNoGo := false
				for _, z := range zones {
					if haversineMetersChat(poi.Lat, poi.Lng, z.CenterLat, z.CenterLng) <= float64(z.RadiusMeters) {
						inNoGo = true
						break
					}
				}
				if inNoGo {
					continue
				}

				// Use zip-level fill rate from historical data if available
				zip5 := stripZipPlus4(poi.Zip)
				fillRate := zipFillRate[zip5]
				if fillRate <= 0 {
					fillRate = 5.0 // default for unknown areas
				}

				expCandidates = append(expCandidates, candidate{
					Lat: poi.Lat, Lng: poi.Lng, City: poi.City, Zip: poi.Zip,
					NearestFillRate: fillRate, Source: "expansion",
					LocationType: "commercial", POIScore: 1.0,
				})
			}
		}
		expCandidates = deduplicateCandidates(expCandidates)
		log.Printf("📍 [Recommend] Expansion candidates: %d", len(expCandidates))
	}

	// Adjust counts based on what we actually have
	if len(gapCandidates) < gapCount {
		gapCount = len(gapCandidates)
		expansionCount = count - gapCount
	}
	if len(expCandidates) < expansionCount {
		expansionCount = len(expCandidates)
		gapCount = count - expansionCount
		if gapCount > len(gapCandidates) {
			gapCount = len(gapCandidates)
		}
	}

	// ======================================================================
	// Score and enrich all candidates
	// ======================================================================
	allCandidates := append(gapCandidates, expCandidates...)
	if len(allCandidates) == 0 {
		return `{"count":0,"recommendations":[],"message":"No suitable locations found."}`, nil
	}

	// Find normalization values
	maxRate := 0.0
	maxPop := 0
	for _, c := range allCandidates {
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

	// Preliminary score
	for i := range allCandidates {
		zip5 := stripZipPlus4(allCandidates[i].Zip)
		fillScore := allCandidates[i].NearestFillRate / maxRate
		gapScore := math.Min(allCandidates[i].NearestBinDist, maxGapMiles) / maxGapMiles
		if allCandidates[i].Source == "expansion" {
			gapScore = 0.8 // expansion candidates get a fixed gap bonus
		}
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
		poiDefault := 0.5
		if allCandidates[i].POIScore > 0 {
			poiDefault = allCandidates[i].POIScore
		}
		allCandidates[i].Score = fillScore*0.25 + gapScore*0.15 + popScore*0.15 + 0.5*0.15 + incomeScore*0.10 + poiDefault*0.20
	}

	// Sort and take top candidates per strategy
	sort.Slice(allCandidates, func(i, j int) bool { return allCandidates[i].Score > allCandidates[j].Score })

	// Select: gapCount from gap_fill + expansionCount from expansion
	var topCandidates []candidate
	gapTaken, expTaken := 0, 0
	for _, c := range allCandidates {
		if c.Source == "gap_fill" && gapTaken < gapCount*3 { // 3x for filtering buffer
			topCandidates = append(topCandidates, c)
			gapTaken++
		} else if c.Source == "expansion" && expTaken < expansionCount*3 {
			topCandidates = append(topCandidates, c)
			expTaken++
		}
		if gapTaken >= gapCount*3 && expTaken >= expansionCount*3 {
			break
		}
	}
	log.Printf("📍 [Recommend] Selected %d for enrichment (gap:%d, exp:%d)", len(topCandidates), gapTaken, expTaken)

	// ======================================================================
	// Enrich: traffic, POI classification, geocode, snap to commercial
	// ======================================================================
	var recommendations []LocationRecommendation
	gapResults, expResults := 0, 0

	for _, c := range topCandidates {
		if gapResults >= gapCount && expResults >= expansionCount {
			break
		}
		if c.Source == "gap_fill" && gapResults >= gapCount {
			continue
		}
		if c.Source == "expansion" && expResults >= expansionCount {
			continue
		}

		// Traffic
		trafficJam := getTrafficJamFactor(c.Lat, c.Lng)
		c.TrafficScore = trafficJam

		// POI classification + snap to retail
		poiScore, locationType, nearMall, snappedLat, snappedLng, matchedPOI := classifyAndSnap(c.Lat, c.Lng)
		c.POIScore = poiScore
		c.LocationType = locationType
		c.NearbyPOI = matchedPOI

		if nearMall {
			log.Printf("🏬 [Recommend] Filtered: %.4f,%.4f — near mall/Safeway", c.Lat, c.Lng)
			continue
		}

		// HARD FILTER: no whitelisted retail or community POI = discard
		if locationType == "residential" || locationType == "no_retail" || locationType == "industrial" || locationType == "unknown" {
			log.Printf("🚫 [Recommend] Filtered: %.4f,%.4f — %s", c.Lat, c.Lng, locationType)
			continue
		}

		// Snap to nearest commercial POI if found
		if snappedLat != 0 && snappedLng != 0 {
			c.Lat = snappedLat
			c.Lng = snappedLng
		}

		// Reverse geocode the (possibly snapped) coordinates
		address, zip := reverseGeocodeHERE(c.Lat, c.Lng)
		if zip != "" {
			c.Zip = zip
		}
		zip5 := stripZipPlus4(c.Zip)

		if isVagueAddress(address, c.City) || isBadAddressKeyword(address) {
			log.Printf("🚫 [Recommend] Filtered: %q", address)
			continue
		}

		// Final score with real data
		fillScore := c.NearestFillRate / maxRate
		gapScore := math.Min(c.NearestBinDist, maxGapMiles) / maxGapMiles
		if c.Source == "expansion" {
			gapScore = 0.8
		}
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
		reasoning := ""
		if c.Source == "gap_fill" {
			reasoning = fmt.Sprintf("%.1f mi gap from bin #%d", c.NearestBinDist, c.NearestBinNum)
		} else {
			reasoning = "expansion area"
		}
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
		if c.NearbyPOI != "" {
			reasoning += fmt.Sprintf(", near %s", c.NearbyPOI)
		} else if locationType != "" {
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
			Source:          c.Source,
		}
		recommendations = append(recommendations, rec)
		if c.Source == "gap_fill" {
			gapResults++
		} else {
			expResults++
		}
		log.Printf("✅ [Recommend] #%d [%s]: %s (score %.1f, %s, traffic %.1f)",
			len(recommendations), c.Source, address, finalScore, locationType, trafficJam)
	}

	sort.Slice(recommendations, func(i, j int) bool { return recommendations[i].Score > recommendations[j].Score })
	log.Printf("📍 [Recommend] Final: %d (gap_fill: %d, expansion: %d)", len(recommendations), gapResults, expResults)

	result, _ := json.Marshal(map[string]any{"count": len(recommendations), "recommendations": recommendations})
	return string(result), nil
}

// ======================================================================
// Helper functions
// ======================================================================

func filterNoGoZones(candidates []candidate, zones []noGoZone) []candidate {
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
	return filtered
}

func deduplicateCandidates(candidates []candidate) []candidate {
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
	return deduped
}

func filterByDistance(candidates []candidate, bins []existingBin, minGap, maxGap float64, perBinFillRate map[string]float64) []candidate {
	var result []candidate
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
		if minDist < minGap || minDist > maxGap {
			continue
		}
		c.NearestBinNum = nearestNum
		c.NearestBinDist = math.Round(minDist*10) / 10
		if rate, ok := perBinFillRate[nearestID]; ok && rate > 0 {
			c.NearestFillRate = rate
		}
		result = append(result, c)
	}
	return result
}

func hasBinsInCity(bins []existingBin, city string) bool {
	count := 0
	for _, b := range bins {
		if strings.EqualFold(b.City, city) {
			count++
		}
	}
	return count >= minBinsPerCity
}

// findCommercialPOIs searches for commercial POIs in a city using HERE Discover API
func findCommercialPOIs(defaultLat, defaultLng float64, city string) []struct {
	Lat  float64
	Lng  float64
	City string
	Zip  string
} {
	var results []struct {
		Lat  float64
		Lng  float64
		City string
		Zip  string
	}

	// Search for commercial spots: gas stations, strip malls, retail plazas
	queries := []string{"gas station", "dollar tree", "grocery store", "laundromat", "church parking lot"}
	lat, lng := defaultLat, defaultLng

	// If no default coords, geocode the city
	if lat == 0 && lng == 0 {
		addr, _ := geocodeCityHERE(city)
		if addr.Lat != 0 {
			lat, lng = addr.Lat, addr.Lng
		} else {
			return results
		}
	}

	client := &http.Client{Timeout: 8 * time.Second}
	for _, q := range queries {
		url := fmt.Sprintf(
			"https://discover.search.hereapi.com/v1/discover?at=%.6f,%.6f&q=%s&limit=5&in=countryCode:USA&apiKey=%s",
			lat, lng, strings.ReplaceAll(q, " ", "+"), HereAPIKey,
		)
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result struct {
			Items []struct {
				Position struct {
					Lat float64 `json:"lat"`
					Lng float64 `json:"lng"`
				} `json:"position"`
				Address struct {
					City       string `json:"city"`
					PostalCode string `json:"postalCode"`
				} `json:"address"`
			} `json:"items"`
		}
		if json.Unmarshal(body, &result) == nil {
			for _, item := range result.Items {
				if item.Position.Lat != 0 {
					results = append(results, struct {
						Lat  float64
						Lng  float64
						City string
						Zip  string
					}{item.Position.Lat, item.Position.Lng, item.Address.City, item.Address.PostalCode})
				}
			}
		}
	}
	log.Printf("📍 [Expand] Found %d commercial POIs in %s", len(results), city)
	return results
}

// geocodeCityHERE returns the center coordinates of a city
func geocodeCityHERE(city string) (struct{ Lat, Lng float64 }, error) {
	url := fmt.Sprintf("https://geocode.search.hereapi.com/v1/geocode?q=%s,CA,USA&limit=1&apiKey=%s",
		strings.ReplaceAll(city, " ", "+"), HereAPIKey)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return struct{ Lat, Lng float64 }{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Items []struct {
			Position struct {
				Lat float64 `json:"lat"`
				Lng float64 `json:"lng"`
			} `json:"position"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &result) == nil && len(result.Items) > 0 {
		return struct{ Lat, Lng float64 }{result.Items[0].Position.Lat, result.Items[0].Position.Lng}, nil
	}
	return struct{ Lat, Lng float64 }{}, fmt.Errorf("geocode failed for %s", city)
}

// classifyAndSnap searches within 800m for whitelisted retail POIs.
// Returns the classification, whether to filter (mall/Safeway), and snap coordinates.
// Logs every POI found for debugging.
func classifyAndSnap(lat, lng float64) (poiScore float64, locationType string, nearMallOrSafeway bool, snapLat, snapLng float64, poiName string) {
	url := fmt.Sprintf(
		"https://browse.search.hereapi.com/v1/browse?at=%.6f,%.6f&limit=20&in=circle:%.6f,%.6f;r=%d&apiKey=%s",
		lat, lng, lat, lng, poiBrowseRadiusM, HereAPIKey,
	)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("⚠️ [POI] Browse API error at %.4f,%.4f: %v", lat, lng, err)
		return 0, "unknown", false, 0, 0, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("⚠️ [POI] Browse HTTP %d at %.4f,%.4f", resp.StatusCode, lat, lng)
		return 0, "unknown", false, 0, 0, ""
	}
	body, _ := io.ReadAll(resp.Body)

	type herePOI struct {
		Title    string `json:"title"`
		Distance int    `json:"distance"` // meters from search point
		Position struct {
			Lat float64 `json:"lat"`
			Lng float64 `json:"lng"`
		} `json:"position"`
		Categories []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Primary bool   `json:"primary"`
		} `json:"categories"`
	}
	var result struct {
		Items []herePOI `json:"items"`
	}
	if json.Unmarshal(body, &result) != nil || len(result.Items) == 0 {
		log.Printf("📍 [POI] %.4f,%.4f: no POIs within %dm → RESIDENTIAL (discard)", lat, lng, poiBrowseRadiusM)
		return 0, "residential", false, 0, 0, ""
	}

	log.Printf("📍 [POI] %.4f,%.4f: found %d POIs within %dm", lat, lng, len(result.Items), poiBrowseRadiusM)

	// Scan all POIs, find best retail match
	var bestRetailPOI *herePOI
	var bestRetailLabel string
	bestRetailDist := math.MaxFloat64
	var bestCommunityPOI *herePOI
	var bestCommunityLabel string

	for idx := range result.Items {
		item := &result.Items[idx]
		titleLower := strings.ToLower(item.Title)
		dist := haversineDistMiles(lat, lng, item.Position.Lat, item.Position.Lng)
		primaryCat := ""
		if len(item.Categories) > 0 {
			primaryCat = item.Categories[0].ID
		}

		// Check for mall or Safeway
		if strings.Contains(titleLower, "mall") || strings.Contains(titleLower, "safeway") {
			nearMallOrSafeway = true
			log.Printf("   🚫 %s (%s, %dm) — MALL/SAFEWAY", item.Title, primaryCat, item.Distance)
		}
		for _, cat := range item.Categories {
			if cat.ID == "600-6100-0062" {
				nearMallOrSafeway = true
			}
		}

		// Check against retail whitelist — but reject if title contains B2B keywords
		isB2B := false
		for _, kw := range b2bTitleKeywords {
			if strings.Contains(titleLower, kw) {
				isB2B = true
				break
			}
		}
		if isB2B {
			log.Printf("   ⬜ %s (%s, %dm) — B2B/service (skipped: title match)", item.Title, primaryCat, item.Distance)
			goto nextItem
		}

		for _, wl := range retailWhitelist {
			for _, cat := range item.Categories {
				if strings.HasPrefix(cat.ID, wl.Prefix) {
					log.Printf("   ✅ %s (%s, %dm) — RETAIL MATCH: %s", item.Title, primaryCat, item.Distance, wl.Label)
					if dist < bestRetailDist {
						bestRetailDist = dist
						bestRetailPOI = item
						bestRetailLabel = wl.Label
					}
					goto nextItem
				}
			}
		}

		// Check against community whitelist
		for _, wl := range communityWhitelist {
			for _, cat := range item.Categories {
				if strings.HasPrefix(cat.ID, wl.Prefix) {
					log.Printf("   🔵 %s (%s, %dm) — COMMUNITY: %s", item.Title, primaryCat, item.Distance, wl.Label)
					if bestCommunityPOI == nil {
						bestCommunityPOI = item
						bestCommunityLabel = wl.Label
					}
					goto nextItem
				}
			}
		}

		// Not whitelisted — log as skipped
		log.Printf("   ⬜ %s (%s, %dm) — not whitelisted", item.Title, primaryCat, item.Distance)

	nextItem:
	}

	// Decision: retail > community > nothing
	if bestRetailPOI != nil {
		snapDist := haversineDistMiles(lat, lng, bestRetailPOI.Position.Lat, bestRetailPOI.Position.Lng)
		log.Printf("   → Snapping to %s (%s) at %.4f,%.4f (%.0fm snap)",
			bestRetailPOI.Title, bestRetailLabel, bestRetailPOI.Position.Lat, bestRetailPOI.Position.Lng, snapDist*1609)
		return 1.0, "commercial", nearMallOrSafeway, bestRetailPOI.Position.Lat, bestRetailPOI.Position.Lng, bestRetailPOI.Title
	}
	if bestCommunityPOI != nil {
		log.Printf("   → Community match: %s (%s), no snap (community POIs aren't placement targets)",
			bestCommunityPOI.Title, bestCommunityLabel)
		return 0.6, "community", nearMallOrSafeway, 0, 0, bestCommunityPOI.Title
	}

	log.Printf("   → No whitelisted POI within %dm → DISCARD", poiBrowseRadiusM)
	return 0, "no_retail", nearMallOrSafeway, 0, 0, ""
}

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
	url := fmt.Sprintf("https://data.traffic.hereapi.com/v7/flow?in=circle:%.6f,%.6f;r=100&locationReferencing=none&apiKey=%s", lat, lng, HereAPIKey)
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
	if json.Unmarshal(body, &result) != nil || len(result.Results) == 0 {
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
	if json.Unmarshal(body, &result) != nil || len(result.Items) == 0 {
		return fmt.Sprintf("%.4f, %.4f", lat, lng), ""
	}
	return result.Items[0].Address.Label, result.Items[0].Address.PostalCode
}
