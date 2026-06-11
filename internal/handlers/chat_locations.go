package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
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
	// Automotive B2B
	"repair", "service", "detail", "detailing", "auto body", "autobody",
	"auto glass", "towing", "tires", "tire shop", "motors", "motor",
	"customs", "custom", "smog", "transmission", "muffler", "brake",
	// Industrial/manufacturing
	"printing", "supply", "supplies", "wholesale", "distribution",
	"logistics", "warehouse", "industrial", "manufacturing", "fabricat",
	"welding", "machine shop", "machinery", "equipment", "packaging",
	// Construction/trades
	"plumbing", "plumber", "electric", "roofing", "contractor",
	"construction", "paving", "flooring", "landscap", "painting",
	"hvac", "insulation", "fence", "demolition",
	// Other B2B
	"parts", "scrap", "salvage", "recycling", "storage", "moving",
	"freight", "shipping", "courier", "staffing", "consulting",
	"designs", "furniture", "upholster", "glass",
	// Medical (not foot traffic for donations)
	"acupuncture", "chiropract", "dentist", "clinic", "medical",
	"veterinar", "lab ", "laboratory",
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
	// STRATEGY A: Business-first — search for real businesses in demand areas
	// Step 1: Identify high-demand areas (from bin performance data)
	// Step 2: Search HERE Discover for businesses in those areas
	// Step 3: Filter and score
	// ======================================================================
	var gapCandidates []candidate

	// Identify high-demand cities/areas sorted by fill rate
	type cityDemand struct {
		City     string
		CenterLat float64
		CenterLng float64
		AvgRate   float64
		BinCount  int
	}
	cityDemandMap := map[string]*cityDemand{}
	for _, b := range bins {
		cd, ok := cityDemandMap[b.City]
		if !ok {
			cd = &cityDemand{City: b.City}
			cityDemandMap[b.City] = cd
		}
		cd.CenterLat += b.Latitude
		cd.CenterLng += b.Longitude
		cd.BinCount++
		if rate, ok := perBinFillRate[b.ID]; ok {
			cd.AvgRate += rate
		}
	}
	var demandAreas []cityDemand
	for _, cd := range cityDemandMap {
		if cd.BinCount >= minBinsPerCity {
			cd.CenterLat /= float64(cd.BinCount)
			cd.CenterLng /= float64(cd.BinCount)
			cd.AvgRate /= float64(cd.BinCount)
			demandAreas = append(demandAreas, *cd)
		}
	}
	sort.Slice(demandAreas, func(i, j int) bool { return demandAreas[i].AvgRate > demandAreas[j].AvgRate })

	// Also check GraphVenn for additional demand hotspot areas
	graphvennURL := os.Getenv("GRAPHVENN_SERVICE_URL")
	var gvHotspots []struct{ Lat, Lng float64 }
	if graphvennURL != "" && len(bins) >= 3 {
		gvCandidates := callGraphVennService(graphvennURL, bins, perBinFillRate, zones, count)
		if len(gvCandidates) > 0 {
			log.Printf("📍 [Recommend] GraphVenn identified %d demand hotspot areas", len(gvCandidates))
			for _, c := range gvCandidates {
				gvHotspots = append(gvHotspots, struct{ Lat, Lng float64 }{c.Lat, c.Lng})
			}
		}
	}

	// Search for REAL BUSINESSES in high-demand areas
	businessQueries := []string{"gas station", "laundromat", "dollar tree", "grocery store", "coffee shop"}
	client := &http.Client{Timeout: 8 * time.Second}

	// Search in top demand cities
	searchAreas := demandAreas
	if len(searchAreas) > 5 {
		searchAreas = searchAreas[:5]
	}

	for _, area := range searchAreas {
		for _, query := range businessQueries {
			businesses := discoverBusinesses(client, area.CenterLat, area.CenterLng, query)
			for _, biz := range businesses {
				// Check not too close to existing bin
				tooClose := false
				nearestNum := 0
				nearestID := ""
				nearestDist := math.MaxFloat64
				for _, b := range bins {
					d := haversineDistMiles(biz.Lat, biz.Lng, b.Latitude, b.Longitude)
					if d < nearestDist {
						nearestDist = d
						nearestNum = b.BinNumber
						nearestID = b.ID
					}
					if d < minGapMiles {
						tooClose = true
					}
				}
				if tooClose {
					continue
				}
				// Check no-go zones
				inNoGo := false
				for _, z := range zones {
					if haversineMetersChat(biz.Lat, biz.Lng, z.CenterLat, z.CenterLng) <= float64(z.RadiusMeters) {
						inNoGo = true
						break
					}
				}
				if inNoGo {
					continue
				}
				// Check not a mall or Safeway
				titleLower := strings.ToLower(biz.Name)
				if strings.Contains(titleLower, "mall") || strings.Contains(titleLower, "safeway") {
					continue
				}
				// Check B2B keywords
				isB2B := false
				for _, kw := range b2bTitleKeywords {
					if strings.Contains(titleLower, kw) {
						isB2B = true
						break
					}
				}
				if isB2B {
					continue
				}

				fillRate := 5.0
				if rate, ok := perBinFillRate[nearestID]; ok && rate > 0 {
					fillRate = rate
				}
				gapCandidates = append(gapCandidates, candidate{
					Lat: biz.Lat, Lng: biz.Lng, City: area.City, Zip: biz.Zip,
					NearestFillRate: fillRate, NearestBinNum: nearestNum,
					NearestBinDist: math.Round(nearestDist*10) / 10,
					NearbyPOI: biz.Name, LocationType: "commercial",
					POIScore: 1.0, Source: "business_search",
				})
			}
		}
	}

	// Also search around GraphVenn hotspots (if available)
	for _, hs := range gvHotspots {
		for _, query := range businessQueries[:3] { // fewer queries per hotspot to save API calls
			businesses := discoverBusinesses(client, hs.Lat, hs.Lng, query)
			for _, biz := range businesses {
				tooClose := false
				nearestNum := 0
				nearestID := ""
				nearestDist := math.MaxFloat64
				for _, b := range bins {
					d := haversineDistMiles(biz.Lat, biz.Lng, b.Latitude, b.Longitude)
					if d < nearestDist {
						nearestDist = d
						nearestNum = b.BinNumber
						nearestID = b.ID
					}
					if d < minGapMiles {
						tooClose = true
					}
				}
				if tooClose {
					continue
				}
				inNoGo := false
				for _, z := range zones {
					if haversineMetersChat(biz.Lat, biz.Lng, z.CenterLat, z.CenterLng) <= float64(z.RadiusMeters) {
						inNoGo = true
						break
					}
				}
				if inNoGo {
					continue
				}
				titleLower := strings.ToLower(biz.Name)
				if strings.Contains(titleLower, "mall") || strings.Contains(titleLower, "safeway") {
					continue
				}
				isB2B := false
				for _, kw := range b2bTitleKeywords {
					if strings.Contains(titleLower, kw) {
						isB2B = true
						break
					}
				}
				if isB2B {
					continue
				}
				fillRate := 5.0
				if rate, ok := perBinFillRate[nearestID]; ok && rate > 0 {
					fillRate = rate
				}
				gapCandidates = append(gapCandidates, candidate{
					Lat: biz.Lat, Lng: biz.Lng, City: biz.City, Zip: biz.Zip,
					NearestFillRate: fillRate, NearestBinNum: nearestNum,
					NearestBinDist: math.Round(nearestDist*10) / 10,
					NearbyPOI: biz.Name, LocationType: "commercial",
					POIScore: 1.0, Source: "graphvenn_business",
				})
			}
		}
	}

	log.Printf("📍 [Recommend] Business search found %d candidates", len(gapCandidates))
	gapCandidates = deduplicateCandidates(gapCandidates)
	log.Printf("📍 [Recommend] After dedup: %d", len(gapCandidates))

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
		isGapFill := c.Source == "gap_fill" || c.Source == "business_search" || c.Source == "graphvenn_business"
		if isGapFill && gapTaken < gapCount*3 { // 3x for filtering buffer
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
		isGapSource := c.Source == "gap_fill" || c.Source == "business_search" || c.Source == "graphvenn_business"
		if isGapSource && gapResults >= gapCount {
			continue
		}
		if c.Source == "expansion" && expResults >= expansionCount {
			continue
		}

		// Traffic
		trafficJam := getTrafficJamFactor(c.Lat, c.Lng)
		c.TrafficScore = trafficJam

		// POI density + anchor tenant detection (300m radius)
		poiDensity, hasAnchor, anchorName := scorePOIDensity(c.Lat, c.Lng)

		// Reverse geocode
		address, zip := reverseGeocodeHERE(c.Lat, c.Lng)
		if zip != "" {
			c.Zip = zip
		}
		zip5 := stripZipPlus4(c.Zip)

		if c.Source != "business_search" && c.Source != "graphvenn_business" {
			if isVagueAddress(address, c.City) || isBadAddressKeyword(address) {
				log.Printf("🚫 [Recommend] Filtered: %q", address)
				continue
			}
		}

		// HARD FILTER: minimum 3 retail POIs within 300m — if fewer, it's not a real commercial area
		if poiDensity < 3 {
			log.Printf("🚫 [Recommend] Filtered: %s — only %d POIs within 300m (need 3+)", c.NearbyPOI, poiDensity)
			continue
		}

		// POI density score: 3-5 POIs=0.4, 6-8=0.6, 9+=0.9, anchor bonus +0.1
		densityScore := 0.3
		if poiDensity >= 9 {
			densityScore = 0.9
		} else if poiDensity >= 6 {
			densityScore = 0.6
		} else {
			densityScore = 0.4
		}
		if hasAnchor {
			densityScore = math.Min(densityScore+0.1, 1.0)
		}

		locationType := "commercial"
		if poiDensity >= 9 {
			locationType = "retail plaza"
		} else if poiDensity >= 4 {
			locationType = "commercial strip"
		}
		if hasAnchor {
			locationType += " (anchor: " + anchorName + ")"
		}
		c.LocationType = locationType

		log.Printf("📊 [Density] %s: %d POIs, anchor=%v (%s), density=%.1f",
			c.NearbyPOI, poiDensity, hasAnchor, anchorName, densityScore)

		// Final score: density+anchors 30%, fill 20%, traffic 15%, pop 15%, gap 10%, income 10%
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
		finalScore := math.Round((densityScore*0.30+fillScore*0.20+trafficNorm*0.15+popScore*0.15+gapScore*0.10+incomeScore*0.10)*100) / 10

		// Minimum score cutoff — below this is not worth recommending
		if finalScore < 5.5 {
			log.Printf("🚫 [Recommend] Filtered: %s — score %.1f below 5.5 threshold", c.NearbyPOI, finalScore)
			continue
		}

		// Build reasoning
		reasoning := ""
		if c.Source == "expansion" {
			reasoning = "expansion area"
		} else {
			reasoning = fmt.Sprintf("%.1f mi gap from bin #%d", c.NearestBinDist, c.NearestBinNum)
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
		if poiDensity > 0 {
			reasoning += fmt.Sprintf(", %d nearby POIs", poiDensity)
		}
		if hasAnchor {
			reasoning += fmt.Sprintf(", near %s", anchorName)
		}
		if c.NearbyPOI != "" {
			reasoning += fmt.Sprintf(", at %s", c.NearbyPOI)
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
			LocationType:    c.LocationType,
			Source:          c.Source,
		}
		recommendations = append(recommendations, rec)
		if c.Source != "expansion" {
			gapResults++
		} else {
			expResults++
		}
		log.Printf("✅ [Recommend] #%d [%s]: %s (score %.1f, %s, traffic %.1f)",
			len(recommendations), c.Source, address, finalScore, c.LocationType, trafficJam)
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

// Anchor tenant names — major retailers that generate halo foot traffic
var anchorTenants = []string{
	"target", "walmart", "costco", "home depot", "lowe's", "lowes",
	"trader joe", "whole foods", "safeway", "lucky", "food maxx", "foodmaxx",
	"cvs", "walgreens", "rite aid", "ross", "marshalls", "tj maxx",
	"dollar tree", "99 cents", "big lots", "grocery outlet",
	"planet fitness", "24 hour fitness", "starbucks",
}

// scorePOIDensity counts retail POIs within 300m and detects anchor tenants.
// Returns: (poiCount, hasAnchor, anchorName)
func scorePOIDensity(lat, lng float64) (int, bool, string) {
	url := fmt.Sprintf(
		"https://browse.search.hereapi.com/v1/browse?at=%.6f,%.6f&limit=20&in=circle:%.6f,%.6f;r=300&apiKey=%s",
		lat, lng, lat, lng, HereAPIKey,
	)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, false, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, false, ""
	}
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Items []struct {
			Title      string `json:"title"`
			Categories []struct {
				ID string `json:"id"`
			} `json:"categories"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &result) != nil {
		return 0, false, ""
	}

	retailCount := 0
	hasAnchor := false
	anchorName := ""

	for _, item := range result.Items {
		titleLower := strings.ToLower(item.Title)

		// Skip B2B businesses from count
		isB2B := false
		for _, kw := range b2bTitleKeywords {
			if strings.Contains(titleLower, kw) {
				isB2B = true
				break
			}
		}
		if isB2B {
			continue
		}

		// Check if it's a retail POI (not industrial)
		isRetail := false
		for _, cat := range item.Categories {
			for _, wl := range retailWhitelist {
				if strings.HasPrefix(cat.ID, wl.Prefix) {
					isRetail = true
					break
				}
			}
			for _, wl := range communityWhitelist {
				if strings.HasPrefix(cat.ID, wl.Prefix) {
					isRetail = true
					break
				}
			}
			if isRetail {
				break
			}
		}
		if isRetail {
			retailCount++
		}

		// Check for anchor tenant
		if !hasAnchor {
			for _, anchor := range anchorTenants {
				if strings.Contains(titleLower, anchor) {
					hasAnchor = true
					anchorName = item.Title
					break
				}
			}
		}
	}

	return retailCount, hasAnchor, anchorName
}

type discoveredBusiness struct {
	Name string
	Lat  float64
	Lng  float64
	City string
	Zip  string
}

// discoverBusinesses searches for real businesses near a location using HERE Discover API
func discoverBusinesses(client *http.Client, lat, lng float64, query string) []discoveredBusiness {
	url := fmt.Sprintf(
		"https://discover.search.hereapi.com/v1/discover?at=%.6f,%.6f&q=%s&limit=15&in=countryCode:USA&apiKey=%s",
		lat, lng, strings.ReplaceAll(query, " ", "+"), HereAPIKey,
	)
	resp, err := client.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Items []struct {
			Title    string `json:"title"`
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
	if json.Unmarshal(body, &result) != nil {
		return nil
	}
	var businesses []discoveredBusiness
	for _, item := range result.Items {
		if item.Position.Lat != 0 {
			businesses = append(businesses, discoveredBusiness{
				Name: item.Title,
				Lat:  item.Position.Lat,
				Lng:  item.Position.Lng,
				City: item.Address.City,
				Zip:  item.Address.PostalCode,
			})
		}
	}
	log.Printf("📍 [Discover] %q near (%.3f,%.3f): found %d", query, lat, lng, len(businesses))
	return businesses
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

// callGraphVennService calls the GraphVenn Python microservice to get optimal demand hotspots.
// Builds a demand surface from bin fill rates (radiated outward in rings) and sends it
// to the service for optimal placement computation.
func callGraphVennService(serviceURL string, bins []existingBin, perBinFillRate map[string]float64, zones []noGoZone, count int) []candidate {
	// Build demand surface: each bin radiates demand in a ring at 0.5, 1.0, 1.5 miles
	type demandPoint struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Weight    float64 `json:"weight"`
		Label     string  `json:"label,omitempty"`
	}
	type existBin struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		BinNumber int     `json:"bin_number"`
	}
	type noGoReq struct {
		CenterLatitude  float64 `json:"center_latitude"`
		CenterLongitude float64 `json:"center_longitude"`
		RadiusMeters    int     `json:"radius_meters"`
	}

	var demandPoints []demandPoint
	for _, b := range bins {
		fill := 10.0 // default
		if b.FillPercentage != nil {
			fill = float64(*b.FillPercentage)
		}
		// Also use per-bin fill rate if available (better signal)
		if rate, ok := perBinFillRate[b.ID]; ok && rate > 0 {
			fill = rate * 10 // scale fill rate to weight
		}
		weight := math.Max(fill/100.0, 0.1)

		// Generate demand ring: 8 directions at 3 distances
		for _, distMiles := range []float64{0.5, 1.0, 1.5} {
			distDeg := distMiles / 69.0
			for angleDeg := 0; angleDeg < 360; angleDeg += 45 {
				angleRad := float64(angleDeg) * math.Pi / 180
				dlat := distDeg * math.Cos(angleRad)
				dlng := distDeg * math.Sin(angleRad) / math.Cos(b.Latitude*math.Pi/180)
				distWeight := weight * (1.0 - distMiles/2.0)

				demandPoints = append(demandPoints, demandPoint{
					Latitude:  math.Round((b.Latitude+dlat)*1e6) / 1e6,
					Longitude: math.Round((b.Longitude+dlng)*1e6) / 1e6,
					Weight:    math.Round(distWeight*1000) / 1000,
					Label:     fmt.Sprintf("Near Bin #%d", b.BinNumber),
				})
			}
		}
	}

	var existBins []existBin
	for _, b := range bins {
		existBins = append(existBins, existBin{
			Latitude: b.Latitude, Longitude: b.Longitude, BinNumber: b.BinNumber,
		})
	}

	var noGoReqs []noGoReq
	for _, z := range zones {
		noGoReqs = append(noGoReqs, noGoReq{
			CenterLatitude: z.CenterLat, CenterLongitude: z.CenterLng, RadiusMeters: z.RadiusMeters,
		})
	}

	reqBody := map[string]any{
		"demand_points":                  demandPoints,
		"existing_bins":                  existBins,
		"no_go_zones":                    noGoReqs,
		"count":                          count * 2, // request 2x for filtering buffer
		"radius_meters":                  400,
		"min_distance_from_bins_meters":  500,
		"strategy":                       "greedy",
		"precision":                      3,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		log.Printf("⚠️ [GraphVenn] Failed to marshal request: %v", err)
		return nil
	}

	log.Printf("📍 [GraphVenn] Calling service at %s with %d demand points, %d bins", serviceURL, len(demandPoints), len(bins))

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Post(serviceURL+"/recommend", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("⚠️ [GraphVenn] Service call failed: %v", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("⚠️ [GraphVenn] Service returned HTTP %d: %s", resp.StatusCode, string(respBody)[:200])
		return nil
	}

	var gvResp struct {
		Success   bool `json:"success"`
		Locations []struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Score     float64 `json:"score"`
			Rank      int     `json:"rank"`
		} `json:"locations"`
		CoveragePercentage float64 `json:"coverage_percentage"`
		SolverRuntimeMs    int     `json:"solver_runtime_ms"`
	}

	respBody, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(respBody, &gvResp); err != nil {
		log.Printf("⚠️ [GraphVenn] Failed to parse response: %v", err)
		return nil
	}

	if !gvResp.Success || len(gvResp.Locations) == 0 {
		log.Printf("⚠️ [GraphVenn] No results returned")
		return nil
	}

	log.Printf("✅ [GraphVenn] Got %d hotspots in %dms (%.1f%% coverage)",
		len(gvResp.Locations), gvResp.SolverRuntimeMs, gvResp.CoveragePercentage)

	// Convert to candidates
	var candidates []candidate
	for _, loc := range gvResp.Locations {
		candidates = append(candidates, candidate{
			Lat:    loc.Latitude,
			Lng:    loc.Longitude,
			Source: "graphvenn",
		})
	}
	return candidates
}
