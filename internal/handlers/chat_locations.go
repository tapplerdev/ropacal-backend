package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
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
const minBinsPerCity = 1
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
	// Personal services (not retail foot traffic for donations)
	"spa", "salon", "nail", "wellness", "massage", "waxing", "lash",
	"tattoo", "piercing", "barber",
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

// filterCandidates applies ALL filters consistently to any candidate regardless of source.
// This replaces the scattered inline checks across business_search, graphvenn, and expansion paths.
type potentialLocFilter struct {
	Latitude  float64
	Longitude float64
}

func filterCandidates(
	candidates []candidate,
	bins []existingBin,
	existingPotentials []potentialLocFilter,
	zones []noGoZone,
	minGapMiles float64,
	placementMode string,
	useV2 bool,
) []candidate {
	var filtered []candidate
	binCities := map[string]bool{}
	for _, b := range bins {
		binCities[strings.ToLower(b.City)] = true
	}

	for _, c := range candidates {
		rejected := false
		reason := ""

		// 1. No-go zone check
		for _, z := range zones {
			if haversineMetersChat(c.Lat, c.Lng, z.CenterLat, z.CenterLng) <= float64(z.RadiusMeters) {
				rejected = true
				reason = "in no-go zone"
				break
			}
		}
		if rejected {
			log.Printf("🚫 [Filter] %s — %s", c.NearbyPOI, reason)
			continue
		}

		// 2. B2B keyword filter
		nameLower := strings.ToLower(c.NearbyPOI)
		for _, kw := range b2bTitleKeywords {
			if strings.Contains(nameLower, kw) {
				rejected = true
				reason = fmt.Sprintf("B2B keyword '%s'", kw)
				break
			}
		}
		if rejected {
			log.Printf("🚫 [Filter] %s — %s", c.NearbyPOI, reason)
			continue
		}

		// 3. Mall/Safeway filter
		if strings.Contains(nameLower, "safeway") || strings.Contains(nameLower, "mall") {
			log.Printf("🚫 [Filter] %s — mall/Safeway excluded", c.NearbyPOI)
			continue
		}

		// 4. Gap check — too close to existing bin
		nearestNum := 0
		nearestDist := math.MaxFloat64
		nearestID := ""
		var nearestBinLat, nearestBinLng float64
		tooClose := false
		for _, b := range bins {
			d := haversineDistMiles(c.Lat, c.Lng, b.Latitude, b.Longitude)
			if d < nearestDist {
				nearestDist = d
				nearestNum = b.BinNumber
				nearestID = b.ID
				nearestBinLat = b.Latitude
				nearestBinLng = b.Longitude
			}
			if d < minGapMiles {
				if useV2 {
					// Cross-street awareness: allow if driving distance is 3x+ straight-line
					drivingDist := getOSRMDrivingDistMiles(c.Lat, c.Lng, b.Latitude, b.Longitude)
					if drivingDist > d*3.0 {
						continue // separated by major road, allow
					}
				}
				tooClose = true
			}
		}
		if tooClose {
			log.Printf("🚫 [Filter] %s — too close to Bin #%d (%.2f mi, min %.2f)", c.NearbyPOI, nearestNum, nearestDist, minGapMiles)
			continue
		}

		// Update candidate with nearest bin info
		c.NearestBinNum = nearestNum
		c.NearestBinDist = math.Round(nearestDist*10) / 10
		if rate, ok := perBinFillRateGlobal[nearestID]; ok && rate > 0 {
			c.NearestFillRate = rate
		}

		// 5. Existing potentials check
		for _, pl := range existingPotentials {
			if haversineDistMiles(c.Lat, c.Lng, pl.Latitude, pl.Longitude) < minGapMiles {
				tooClose = true
				break
			}
		}
		if tooClose {
			log.Printf("🚫 [Filter] %s — too close to existing potential location", c.NearbyPOI)
			continue
		}

		// 6. Infill mode: max 10-minute drive from nearest bin
		if placementMode == "infill" {
			if nearestDist > 5.0 {
				// Definitely too far — skip OSRM call
				log.Printf("🚫 [Filter/Infill] %s — %.1f mi from nearest bin #%d (>5 mi, skip drive-time check)", c.NearbyPOI, nearestDist, nearestNum)
				continue
			} else if nearestDist > 2.0 {
				// Borderline — check actual drive time
				driveTime := getOSRMDriveTimeMins(c.Lat, c.Lng, nearestBinLat, nearestBinLng)
				if driveTime > 10.0 {
					log.Printf("🚫 [Filter/Infill] %s — %.1f min drive from nearest bin #%d (>10 min cap)", c.NearbyPOI, driveTime, nearestNum)
					continue
				}
				log.Printf("✅ [Filter/Infill] %s — %.1f mi / %.1f min drive from bin #%d (within 10 min)", c.NearbyPOI, nearestDist, driveTime, nearestNum)
			}
			// < 2.0 miles: always within 10 min drive, no need to check
		}

		// 7. Expand mode: skip candidates in cities with existing bins
		if placementMode == "expand" && binCities[strings.ToLower(c.City)] {
			log.Printf("🚫 [Filter/Expand] %s — city %s already has bins", c.NearbyPOI, c.City)
			continue
		}

		filtered = append(filtered, c)
	}

	log.Printf("📋 [Filter] %d → %d candidates after filtering (mode=%s)", len(candidates), len(filtered), placementMode)
	return filtered
}

// perBinFillRateGlobal is set during toolRecommendLocations and used by filterCandidates
var perBinFillRateGlobal map[string]float64

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

	// Algorithm version: "v1" (default) or "v2" (ESRI-enhanced)
	// v2 is the default algorithm (ESRI-enhanced scoring, expanded keywords, centralized filtering)
	// v1 available as fallback if explicitly requested
	algorithm := "v2"
	if alg, ok := params["algorithm"].(string); ok && alg == "v1" {
		algorithm = "v1"
	}
	useV2 := algorithm == "v2"

	// Placement mode: "infill" (near existing high performers) or "expand" (new areas)
	placementMode := "auto" // default: mixed
	if pm, ok := params["mode"].(string); ok {
		placementMode = pm
	}

	// Adjust parameters based on mode
	if useV2 && placementMode == "infill" {
		minGapMiles = 0.15 // tighter spacing OK near proven locations
		log.Printf("📍 [Mode] INFILL — searching near existing high performers, gap=%.2f mi", minGapMiles)
	} else if useV2 && placementMode == "expand" {
		minGapMiles = 0.3 // standard gap for new areas
		log.Printf("📍 [Mode] EXPAND — searching new areas with good demographics")
	}

	tTotal := time.Now()
	log.Printf("📍 [Recommend] Starting: count=%d, city=%q, minGap=%.1f mi, algorithm=%s, mode=%s", count, targetCity, minGapMiles, algorithm, placementMode)

	// Step 1: Get all active bins (for gap detection)
	tDB := time.Now()
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
	perBinFillRateGlobal = perBinFillRate // make available to filterCandidates

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

	// Step 5: Get existing potential locations (to avoid recommending same spots)
	type potentialLoc struct {
		Latitude  float64 `db:"latitude"`
		Longitude float64 `db:"longitude"`
	}
	var existingPotentials []potentialLoc
	h.db.Select(&existingPotentials, `SELECT latitude, longitude FROM potential_locations WHERE latitude IS NOT NULL AND longitude IS NOT NULL`)
	log.Printf("📍 [Recommend] %d existing potential locations to avoid", len(existingPotentials))
	log.Printf("⏱️ [Timing] DB setup: %v (%d bins, %d fill rates, %d zones, %d census)",
		time.Since(tDB), len(bins), len(perBinFillRate), len(zones), len(census))

	// ======================================================================
	// STRATEGY A: Per-bin search — search for real businesses near each existing bin
	// Each bin becomes a search origin. Bins within 0.5mi share one origin.
	// High-fill bins get full keyword search, low-fill bins get anchors only.
	// All searches run in parallel via goroutine worker pool.
	// ======================================================================
	tSearchStart := time.Now()
	var gapCandidates []candidate

	// Keywords for business search
	// Tier 1: high-value anchors (13 keywords — used for ALL bins)
	tier1Keywords := []string{
		"Target", "Walmart", "Safeway", "Trader Joe's", "Costco",
		"Home Depot", "Lowe's", "Grocery Outlet", "Food Maxx",
		"99 Ranch", "Lucky Supermarket", "Whole Foods", "Dollar Tree",
	}
	// Full keyword list: Tier 1 + Tier 2 + Tier 3 (19 keywords — high-fill bins only)
	allKeywords := append(append([]string{}, tier1Keywords...),
		"CVS", "Walgreens", "grocery store", "coffee shop",
		"shopping center", "shopping plaza",
	)
	// v1 fallback
	v1Keywords := []string{"gas station", "laundromat", "dollar tree", "grocery store", "coffee shop"}

	client := &http.Client{Timeout: 8 * time.Second}

	if useV2 && placementMode != "expand" {
		// v2: cluster bins into search origins, build job queue, fan out
		origins := clusterSearchOrigins(bins, perBinFillRate)
		log.Printf("📍 [Search] Clustered %d bins into %d search origins", len(bins), len(origins))
		for i, o := range origins {
			kwCount := len(tier1Keywords)
			if o.MaxFillRate >= 40 {
				kwCount = len(allKeywords)
			}
			if i < 15 { // log first 15 origins
				log.Printf("📍 [Origin] #%d: (%.4f,%.4f) %s, fill=%.1f%%, %d keywords, %d bins",
					i+1, o.Lat, o.Lng, o.City, o.MaxFillRate, kwCount, o.BinCount)
			}
		}

		// Build search jobs
		var jobs []searchJob
		for _, origin := range origins {
			keywords := tier1Keywords
			if origin.MaxFillRate >= 40 {
				keywords = allKeywords
			}
			for _, kw := range keywords {
				jobs = append(jobs, searchJob{
					Lat: origin.Lat, Lng: origin.Lng,
					Query: kw, City: origin.City,
					Source: "business_search",
				})
			}
		}
		log.Printf("📍 [Search] Built %d search jobs from %d origins", len(jobs), len(origins))

		// Fan out with 10 workers
		gapCandidates = fanOutSearch(client, jobs, 10)
	} else if !useV2 {
		// v1 fallback: sequential search from top 5 cities by fill rate
		type cityDemand struct {
			City      string
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
		searchAreas := demandAreas
		if len(searchAreas) > 5 {
			searchAreas = searchAreas[:5]
		}
		for _, area := range searchAreas {
			for _, query := range v1Keywords {
				businesses := discoverBusinesses(client, area.CenterLat, area.CenterLng, query)
				for _, biz := range businesses {
					gapCandidates = append(gapCandidates, candidate{
						Lat: biz.Lat, Lng: biz.Lng, City: area.City, Zip: biz.Zip,
						NearbyPOI: biz.Name, LocationType: "commercial",
						POIScore: 1.0, Source: "business_search",
					})
				}
			}
		}
	}
	// else: expand mode skips business search entirely

	log.Printf("⏱️ [Timing] Business search: %v, found %d raw candidates", time.Since(tSearchStart), len(gapCandidates))
	gapCandidates = deduplicateCandidates(gapCandidates)
	log.Printf("📍 [Recommend] After dedup: %d", len(gapCandidates))

	// ======================================================================
	// STRATEGY B: Expansion candidates — new areas
	// ======================================================================
	var expansionCount, gapCount int
	if useV2 && placementMode == "expand" {
		expansionCount = count    // all results from expansion
		gapCount = 0
	} else if useV2 && placementMode == "infill" {
		expansionCount = 0        // no expansion results
		gapCount = count
	} else {
		expansionCount = int(math.Ceil(float64(count) * 0.3)) // default: 30% expansion
		gapCount = count - expansionCount
	}

	tExpStart := time.Now()
	var expCandidates []candidate
	shouldExpand := placementMode == "expand" || (placementMode != "infill" && (targetCity == "" || !hasBinsInCity(bins, targetCity)))

	if shouldExpand {
		// Build expansion jobs using same fanOutSearch pattern
		cities := expansionCities
		if targetCity != "" {
			cities = []struct {
				City string
				Lat  float64
				Lng  float64
			}{{targetCity, 0, 0}}
		}

		var expJobs []searchJob
		expKeywords := tier1Keywords // use anchor keywords for expansion
		if !useV2 {
			expKeywords = v1Keywords
		}

		for _, ec := range cities {
			if targetCity != "" && !strings.EqualFold(ec.City, targetCity) {
				continue
			}
			hasBins := false
			for _, b := range bins {
				if strings.EqualFold(b.City, ec.City) {
					hasBins = true
					break
				}
			}
			if hasBins {
				log.Printf("📍 [Expand] Skipping %s — already has bins", ec.City)
				continue
			}
			for _, kw := range expKeywords {
				expJobs = append(expJobs, searchJob{
					Lat: ec.Lat, Lng: ec.Lng,
					Query: kw, City: ec.City,
					Source: "expansion",
				})
			}
		}

		if len(expJobs) > 0 {
			log.Printf("📍 [Expand] Built %d expansion search jobs", len(expJobs))
			expCandidates = fanOutSearch(client, expJobs, 10)
			expCandidates = deduplicateCandidates(expCandidates)
			log.Printf("⏱️ [Timing] Expansion search: %v, found %d candidates", time.Since(tExpStart), len(expCandidates))
		}
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
	// Centralized filtering — apply ALL filters consistently to ALL candidates
	// ======================================================================
	tFilter := time.Now()
	rawCandidates := append(gapCandidates, expCandidates...)
	rawCount := len(rawCandidates)
	log.Printf("📍 [Recommend] Raw candidates before filtering: %d (gap:%d, exp:%d)", rawCount, len(gapCandidates), len(expCandidates))
	rawCandidates = deduplicateCandidates(rawCandidates)
	dedupCount := len(rawCandidates)
	log.Printf("📍 [Recommend] After dedup: %d (removed %d duplicates)", dedupCount, rawCount-dedupCount)
	// Convert potentialLoc to potentialLocFilter for the centralized filter
	var potFilters []potentialLocFilter
	for _, pl := range existingPotentials {
		potFilters = append(potFilters, potentialLocFilter{Latitude: pl.Latitude, Longitude: pl.Longitude})
	}
	allCandidates := filterCandidates(rawCandidates, bins, potFilters, zones, minGapMiles, placementMode, useV2)
	log.Printf("⏱️ [Timing] Filter: %v, %d → %d candidates", time.Since(tFilter), dedupCount, len(allCandidates))

	if len(allCandidates) == 0 {
		return `{"count":0,"recommendations":[],"message":"No suitable locations found."}`, nil
	}

	// ======================================================================
	// ESRI batch enrichment — enrich ALL filtered candidates, then score with v2
	// No preliminary scoring gate — every candidate gets ESRI data
	// ======================================================================
	tESRI := time.Now()
	log.Printf("📊 [ESRI] Enriching %d candidates...", len(allCandidates))

	// Build batch locations for ESRI
	esriLocs := make([]struct{ Lat, Lng float64 }, len(allCandidates))
	for i, c := range allCandidates {
		esriLocs[i] = struct{ Lat, Lng float64 }{c.Lat, c.Lng}
	}

	// Batch enrich (handles up to ~200 in one call)
	esriResults, esriErr := EnrichLocationsBatch(esriLocs)
	if esriErr != nil {
		log.Printf("⚠️ [ESRI] Batch enrichment failed: %v — falling back to fill rate scoring", esriErr)
	}

	// Find normalization values
	maxRate := 0.0
	for _, c := range allCandidates {
		if c.NearestFillRate > maxRate {
			maxRate = c.NearestFillRate
		}
	}
	if maxRate <= 0 {
		maxRate = 1
	}

	// v2: ESRI as pass/fail GATE — reject candidates in areas with bad demographics.
	// Demographics decide WHERE to look (which neighborhoods). Site quality decides
	// WHICH specific spot within that area. Mixing them in a weighted average dilutes
	// both signals since ESRI returns identical values for same-city candidates.
	tier1Anchors := []string{"target", "walmart", "safeway", "trader joe", "costco", "home depot", "lowes", "grocery outlet", "food maxx", "99 ranch", "lucky", "whole foods", "dollar tree", "cvs", "walgreens"}
	if useV2 && esriResults != nil {
		var gatedCandidates []candidate
		for i := range allCandidates {
			if i < len(esriResults) && esriResults[i].HasData {
				e := esriResults[i]
				// All-zeros = ESRI has no data for this coordinate. Reject conservatively —
				// can't verify neighborhood safety (caught Oakland FoodMaxx in high-crime area).
				if e.MedianHouseholdIncome == 0 && e.CrimeIndex == 0 && e.AvgClothingSpend == 0 {
					log.Printf("🚫 [ESRI Gate] %s (%.4f,%.4f) — no ESRI data available, rejecting",
						allCandidates[i].NearbyPOI, allCandidates[i].Lat, allCandidates[i].Lng)
					continue
				}
				if e.MedianHouseholdIncome > 0 && e.MedianHouseholdIncome < 50000 {
					log.Printf("🚫 [ESRI Gate] %s — income $%.0fk below $50k minimum", allCandidates[i].NearbyPOI, e.MedianHouseholdIncome/1000)
					continue
				}
				if e.CrimeIndex > 200 {
					log.Printf("🚫 [ESRI Gate] %s — crime index %.0f above 200 maximum", allCandidates[i].NearbyPOI, e.CrimeIndex)
					continue
				}
				if e.AvgClothingSpend > 0 && e.AvgClothingSpend < 2500 {
					log.Printf("🚫 [ESRI Gate] %s — clothing spend $%.0f below $2,500 minimum", allCandidates[i].NearbyPOI, e.AvgClothingSpend)
					continue
				}
			}
			// Candidates without ESRI data pass the gate (don't reject on API failure)
			gatedCandidates = append(gatedCandidates, allCandidates[i])
		}
		gateRejected := len(allCandidates) - len(gatedCandidates)
		log.Printf("📊 [ESRI Gate] %d → %d candidates passed demographic gates (%d rejected)", len(allCandidates), len(gatedCandidates), gateRejected)
		log.Printf("⏱️ [Timing] ESRI enrichment + gate: %v", time.Since(tESRI))
		allCandidates = gatedCandidates
	}

	// Preliminary score for sorting: anchor name match + fill rate gap.
	// This determines which candidates get the expensive POI density check first.
	for i := range allCandidates {
		fillScore := allCandidates[i].NearestFillRate / maxRate
		anchorBoost := 0.0
		nameLower := strings.ToLower(allCandidates[i].NearbyPOI)
		nameNorm := strings.ReplaceAll(strings.ReplaceAll(nameLower, "\u2019", ""), "'", "")
		for _, anchor := range tier1Anchors {
			if strings.Contains(nameNorm, anchor) {
				anchorBoost = 1.0
				break
			}
		}
		allCandidates[i].Score = anchorBoost*0.6 + fillScore*0.4
	}

	// Sort by score and take top candidates
	sort.Slice(allCandidates, func(i, j int) bool { return allCandidates[i].Score > allCandidates[j].Score })

	topCount := count * 4 // 4x buffer for POI density filtering
	if topCount > len(allCandidates) {
		topCount = len(allCandidates)
	}
	topCandidates := allCandidates[:topCount]

	log.Printf("📍 [Recommend] %d candidates passed gates, selected top %d for POI enrichment", len(allCandidates), topCount)
	for i, tc := range topCandidates {
		if i < 10 { // log first 10
			log.Printf("   📌 [%d] %.3f %s (%.4f,%.4f) src=%s nearest=#%d (%.1f mi)",
				i+1, tc.Score, tc.NearbyPOI, tc.Lat, tc.Lng, tc.Source, tc.NearestBinNum, tc.NearestBinDist)
		}
	}

	// ======================================================================
	// Enrich: POI density (parallel), then geocode + scoring (sequential)
	// ======================================================================

	// Pre-fetch POI density for ALL topCandidates in parallel (biggest latency win)
	type poiResult struct {
		Index       int
		Density     int
		HasAnchor   bool
		AnchorName  string
		AnchorLat   float64
		AnchorLng   float64
		RetailRatio float64
	}
	tEnrichStart := time.Now()
	poiResults := make([]poiResult, len(topCandidates))
	{
		poiJobCh := make(chan int, len(topCandidates))
		var poiWg sync.WaitGroup
		poiWorkers := 8
		for w := 0; w < poiWorkers; w++ {
			poiWg.Add(1)
			go func() {
				defer poiWg.Done()
				for idx := range poiJobCh {
					tc := topCandidates[idx]
					density, hasAnc, ancName, ancLat, ancLng, retRatio := scorePOIDensity(tc.Lat, tc.Lng)
					poiResults[idx] = poiResult{
						Index: idx, Density: density,
						HasAnchor: hasAnc, AnchorName: ancName,
						AnchorLat: ancLat, AnchorLng: ancLng,
						RetailRatio: retRatio,
					}
				}
			}()
		}
		for i := range topCandidates {
			poiJobCh <- i
		}
		close(poiJobCh)
		poiWg.Wait()
	}
	log.Printf("⏱️ [Timing] POI density enrichment (%d candidates, 8 workers): %v", len(topCandidates), time.Since(tEnrichStart))

	var recommendations []LocationRecommendation
	gapResults, expResults := 0, 0

	for idx, c := range topCandidates {
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

		// Traffic (v1 only — v2 uses ESRI data instead)
		trafficJam := 0.0
		if !useV2 {
			trafficJam = getTrafficJamFactor(c.Lat, c.Lng)
			c.TrafficScore = trafficJam
		}

		// Use pre-fetched POI density data
		pr := poiResults[idx]
		poiDensity := pr.Density
		hasAnchor := pr.HasAnchor
		anchorName := pr.AnchorName
		anchorLat := pr.AnchorLat
		anchorLng := pr.AnchorLng
		retailRatio := pr.RetailRatio
		_ = retailRatio // used in v1 path below

		if useV2 {
			// v2: anchor = auto-pass, else 4+ non-B2B businesses required
			if !hasAnchor && poiDensity < 4 {
				log.Printf("🚫 [v2] Filtered: %s — no anchor + only %d POIs (need 4+)", c.NearbyPOI, poiDensity)
				continue
			}
		} else {
			// v1: retail ratio filter
			if retailRatio < 0.4 && poiDensity < 6 {
				log.Printf("🚫 [Recommend] Filtered: %s — retail ratio %.0f%% (need 40%%+)", c.NearbyPOI, retailRatio*100)
				continue
			}
		}

		// Snap to anchor tenant if detected — anchor has more foot traffic than the original business
		if hasAnchor && anchorLat != 0 && anchorLng != 0 {
			snapDist := haversineDistMiles(c.Lat, c.Lng, anchorLat, anchorLng)
			log.Printf("🏪 [Snap] Moving pin from %s to anchor %s (%.0fm)", c.NearbyPOI, anchorName, snapDist*1609)
			c.Lat = anchorLat
			c.Lng = anchorLng

			// BUG FIX: Re-check gap after snap — candidate may have moved onto an existing bin
			for _, b := range bins {
				d := haversineDistMiles(c.Lat, c.Lng, b.Latitude, b.Longitude)
				if d < 0.05 { // within ~260 feet = same location
					log.Printf("🚫 [Snap] Post-snap rejection: now %.0fm from Bin #%d", d*1609, b.BinNumber)
					hasAnchor = false // use as flag to skip this candidate
					break
				}
			}
			if !hasAnchor {
				continue // snapped onto existing bin, skip
			}
			hasAnchor = true // restore flag
		}

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

		// HARD FILTER: minimum POI density (v2 already checked above with anchor logic)
		if !useV2 && poiDensity < 3 {
			log.Printf("🚫 [Recommend] Filtered: %s — only %d POIs within 300m (need 3+)", c.NearbyPOI, poiDensity)
			continue
		}

		// POI density score: log-scaled continuous (v2) or tiered (v1)
		var densityScore float64
		if useV2 {
			// v2: continuous log-scaled density — ln(1+count) / ln(1+max)
			// max 20 POIs = 1.0. Spreads 5-14 POI range into 0.60-0.89 instead of all being 1.0.
			maxPOI := 20.0
			densityScore = math.Log(1+float64(poiDensity)) / math.Log(1+maxPOI)
			if densityScore > 1.0 {
				densityScore = 1.0
			}
		} else {
			// v1: original tiered scoring
			densityScore = 0.3
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

		// Final scoring — site quality only (ESRI used as gate above, not in score)
		fillScore := c.NearestFillRate / maxRate

		var finalScore float64

		if useV2 {
			// v2: multiplicative site quality scoring
			// Uses (density^0.5) × (anchor^0.3) × (fill^0.2) × 10
			// Multiplicative = weakness in ANY dimension tanks the score (no compensation).
			// A Target in a dead zone scores worse than a Target at a busy plaza.

			// Tiered anchor score: national anchor > regional chain > non-anchor
			anchorScore := 0.15 // non-anchor floor (prevents zero from killing score)
			nameLower := strings.ToLower(c.NearbyPOI)
			nameNorm := strings.ReplaceAll(strings.ReplaceAll(nameLower, "\u2019", ""), "'", "")
			// Tier 1 national anchors — highest foot traffic
			tier1National := []string{"target", "walmart", "costco", "home depot", "lowes", "safeway", "trader joe", "whole foods"}
			// Tier 2 regional chains — good foot traffic
			tier2Regional := []string{"cvs", "walgreens", "grocery outlet", "food maxx", "99 ranch", "lucky", "dollar tree"}
			isT1 := false
			for _, anchor := range tier1National {
				if strings.Contains(nameNorm, anchor) {
					anchorScore = 1.0
					isT1 = true
					break
				}
			}
			if !isT1 {
				for _, anchor := range tier2Regional {
					if strings.Contains(nameNorm, anchor) {
						anchorScore = 0.7
						break
					}
				}
			}
			// POI density anchor detection also counts
			if hasAnchor && anchorScore < 0.7 {
				anchorScore = 0.7
			}

			// Ensure fill score has a floor (expansion areas with 0 fill shouldn't zero out)
			fillVal := fillScore
			if fillVal < 0.1 {
				fillVal = 0.1
			}

			// Multiplicative: (density^0.5) × (anchor^0.3) × (fill^0.2) × 10
			finalScore = math.Pow(densityScore, 0.5) * math.Pow(anchorScore, 0.3) * math.Pow(fillVal, 0.2) * 10
			finalScore = math.Round(finalScore*10) / 10 // round to 1 decimal
			finalScore = math.Min(10, finalScore)

			log.Printf("📊 [v2 Site] %s: %.1f (density=%.3f, anchor=%.2f, fill=%.2f, POIs=%d, d^.5=%.3f, a^.3=%.3f, f^.2=%.3f)",
				c.NearbyPOI, finalScore, densityScore, anchorScore, fillVal, poiDensity,
				math.Pow(densityScore, 0.5), math.Pow(anchorScore, 0.3), math.Pow(fillVal, 0.2))
		} else {
			// v1 fallback — census-based scoring
			gapScore := math.Min(c.NearestBinDist, maxGapMiles) / maxGapMiles
			if c.Source == "expansion" {
				gapScore = 0.8
			}
			zip5fb := stripZipPlus4(c.Zip)
			popScore := 0.5
			if pop, ok := zipPopulation[zip5fb]; ok && pop > 0 {
				popScore = math.Min(float64(pop)/50000.0, 1.0)
			}
			incomeScore := 0.5
			if income, ok := zipIncome[zip5fb]; ok && income > 0 {
				incomeScore = float64(income) / 150000.0
				if incomeScore > 1.5 { incomeScore = 1.5 }
			}
			trafficNorm := math.Min(trafficJam, 10.0) / 10.0
			finalScore = math.Round((densityScore*0.30+fillScore*0.20+trafficNorm*0.15+popScore*0.15+gapScore*0.10+incomeScore*0.10)*100) / 10
		}

		// Visual verification — DISABLED (saves ~$0.01-0.03 per candidate)
		// ESRI demographics + POI density + B2B filter catch the cases vision was meant for.
		// Vision couldn't distinguish office parks from retail plazas in satellite view.
		// Keeping verifyLocationVisually() function for future use if needed.
		// visualPass, visualReason := verifyLocationVisually(c.Lat, c.Lng, c.NearbyPOI)
		// if !visualPass {
		// 	log.Printf("👁️ [Recommend] Filtered by vision: %s — %s", c.NearbyPOI, visualReason)
		// 	continue
		// }

		// Minimum score cutoff — below this is not worth recommending
		if finalScore < 4.0 {
			log.Printf("🚫 [Recommend] Filtered: %s — score %.1f below 4.0 threshold", c.NearbyPOI, finalScore)
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
	log.Printf("⏱️ [Timing] TOTAL: %v, returning %d results", time.Since(tTotal), len(recommendations))

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

// searchOrigin represents a deduplicated bin location used as a HERE Discover search center.
// Bins within 0.5 miles of each other share one origin to avoid duplicate API calls.
type searchOrigin struct {
	Lat, Lng    float64
	City        string
	MaxFillRate float64
	BinCount    int
}

// searchJob is a single HERE Discover API call to make.
type searchJob struct {
	Lat, Lng float64
	Query    string
	City     string
	Source   string // "business_search" or "expansion"
}

// clusterSearchOrigins groups bins by proximity (0.5mi) and returns deduplicated search origins.
// Bins are sorted by fill rate (highest first) so high-performing bins become cluster centers.
func clusterSearchOrigins(bins []existingBin, perBinFillRate map[string]float64) []searchOrigin {
	// Sort bins by fill rate descending — best performers become cluster centers
	type binWithRate struct {
		Bin  existingBin
		Rate float64
	}
	var binsWithRates []binWithRate
	for _, b := range bins {
		rate := perBinFillRate[b.ID]
		binsWithRates = append(binsWithRates, binWithRate{Bin: b, Rate: rate})
	}
	sort.Slice(binsWithRates, func(i, j int) bool { return binsWithRates[i].Rate > binsWithRates[j].Rate })

	var origins []searchOrigin
	for _, br := range binsWithRates {
		// Check if this bin is within 0.5 miles of an existing origin
		tooClose := false
		for i := range origins {
			if haversineDistMiles(br.Bin.Latitude, br.Bin.Longitude, origins[i].Lat, origins[i].Lng) < 0.5 {
				origins[i].BinCount++
				if br.Rate > origins[i].MaxFillRate {
					origins[i].MaxFillRate = br.Rate
				}
				tooClose = true
				break
			}
		}
		if !tooClose {
			origins = append(origins, searchOrigin{
				Lat:         br.Bin.Latitude,
				Lng:         br.Bin.Longitude,
				City:        br.Bin.City,
				MaxFillRate: br.Rate,
				BinCount:    1,
			})
		}
	}
	return origins
}

// fanOutSearch runs HERE Discover API calls in parallel using a worker pool.
func fanOutSearch(client *http.Client, jobs []searchJob, workers int) []candidate {
	jobCh := make(chan searchJob, len(jobs))
	resultCh := make(chan []candidate, len(jobs))

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				businesses := discoverBusinesses(client, job.Lat, job.Lng, job.Query)
				var candidates []candidate
				for _, biz := range businesses {
					candidates = append(candidates, candidate{
						Lat: biz.Lat, Lng: biz.Lng, City: job.City, Zip: biz.Zip,
						NearbyPOI: biz.Name, LocationType: "commercial",
						POIScore: 1.0, Source: job.Source,
					})
				}
				resultCh <- candidates
			}
		}()
	}

	// Send all jobs
	for _, job := range jobs {
		jobCh <- job
	}
	close(jobCh)

	// Wait for workers then close results
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Collect all results
	var allCandidates []candidate
	for batch := range resultCh {
		allCandidates = append(allCandidates, batch...)
	}
	return allCandidates
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

	// Search for commercial spots — same v2 keywords as main search path
	queries := []string{
		"Target", "Walmart", "Safeway", "Trader Joe's", "Costco",
		"Home Depot", "Lowe's", "Grocery Outlet", "Dollar Tree",
		"CVS", "Walgreens", "grocery store", "coffee shop",
		"shopping center",
	}
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
			"https://discover.search.hereapi.com/v1/discover?in=circle:%.6f,%.6f;r=5000&q=%s&limit=5&apiKey=%s",
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
// Returns: (retailCount, hasAnchor, anchorName, anchorLat, anchorLng, retailRatio)
func scorePOIDensity(lat, lng float64) (int, bool, string, float64, float64, float64) {
	url := fmt.Sprintf(
		"https://browse.search.hereapi.com/v1/browse?at=%.6f,%.6f&limit=20&in=circle:%.6f,%.6f;r=300&apiKey=%s",
		lat, lng, lat, lng, HereAPIKey,
	)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, false, "", 0, 0, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, false, "", 0, 0, 0
	}
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Items []struct {
			Title    string `json:"title"`
			Distance int    `json:"distance"`
			Position struct {
				Lat float64 `json:"lat"`
				Lng float64 `json:"lng"`
			} `json:"position"`
			Categories []struct {
				ID string `json:"id"`
			} `json:"categories"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &result) != nil {
		return 0, false, "", 0, 0, 0
	}

	retailCount := 0
	totalPOIs := len(result.Items)
	hasAnchor := false
	anchorName := ""
	var anchorLat, anchorLng float64

	log.Printf("📊 [Density] Scanning %d POIs within 300m of (%.4f, %.4f)", totalPOIs, lat, lng)

	for _, item := range result.Items {
		titleLower := strings.ToLower(item.Title)
		primaryCat := ""
		if len(item.Categories) > 0 {
			primaryCat = item.Categories[0].ID
		}

		// Skip B2B businesses from count
		isB2B := false
		for _, kw := range b2bTitleKeywords {
			if strings.Contains(titleLower, kw) {
				isB2B = true
				break
			}
		}
		if isB2B {
			log.Printf("   ⬜ %s (%s, %dm) — B2B skipped", item.Title, primaryCat, item.Distance)
			continue
		}

		// Check if it's a retail POI
		isRetail := false
		matchLabel := ""
		for _, cat := range item.Categories {
			for _, wl := range retailWhitelist {
				if strings.HasPrefix(cat.ID, wl.Prefix) {
					isRetail = true
					matchLabel = wl.Label
					break
				}
			}
			for _, wl := range communityWhitelist {
				if strings.HasPrefix(cat.ID, wl.Prefix) {
					isRetail = true
					matchLabel = wl.Label
					break
				}
			}
			if isRetail {
				break
			}
		}
		if isRetail {
			retailCount++
			log.Printf("   ✅ %s (%s, %dm) — %s", item.Title, primaryCat, item.Distance, matchLabel)
		} else {
			log.Printf("   ⬜ %s (%s, %dm) — no whitelist match", item.Title, primaryCat, item.Distance)
		}

		// Check for anchor tenant
		if !hasAnchor {
			for _, anchor := range anchorTenants {
				if strings.Contains(titleLower, anchor) {
					hasAnchor = true
					anchorName = item.Title
					anchorLat = item.Position.Lat
					anchorLng = item.Position.Lng
					log.Printf("   🏪 ANCHOR DETECTED: %s at (%.4f, %.4f) %dm away", item.Title, item.Position.Lat, item.Position.Lng, item.Distance)
					break
				}
			}
		}
	}

	retailRatio := 0.0
	if totalPOIs > 0 {
		retailRatio = float64(retailCount) / float64(totalPOIs)
	}
	log.Printf("📊 [Density] Result: %d/%d retail POIs (%.0f%%), anchor=%v (%s)", retailCount, totalPOIs, retailRatio*100, hasAnchor, anchorName)
	return retailCount, hasAnchor, anchorName, anchorLat, anchorLng, retailRatio
}

// verifyLocationVisually fetches a satellite image and uses Claude Vision to judge if it's a good bin placement spot.
// Returns: (isGoodSpot bool, reason string)
func verifyLocationVisually(lat, lng float64, businessName string) (bool, string) {
	googleKey := os.Getenv("GOOGLE_MAPS_API_KEY")
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	if googleKey == "" || anthropicKey == "" {
		log.Printf("⚠️ [Vision] Skipping visual verification — missing API keys")
		return true, "skipped" // fail open
	}

	// Fetch satellite image from Google Static Maps
	imgURL := fmt.Sprintf(
		"https://maps.googleapis.com/maps/api/staticmap?center=%.6f,%.6f&zoom=18&size=600x600&maptype=satellite&key=%s",
		lat, lng, googleKey,
	)

	imgClient := &http.Client{Timeout: 10 * time.Second}
	imgResp, err := imgClient.Get(imgURL)
	if err != nil {
		log.Printf("⚠️ [Vision] Failed to fetch satellite image: %v", err)
		return true, "image fetch failed"
	}
	defer imgResp.Body.Close()

	imgBytes, _ := io.ReadAll(imgResp.Body)
	if len(imgBytes) == 0 {
		return true, "empty image"
	}

	// Encode image as base64
	imgBase64 := base64.StdEncoding.EncodeToString(imgBytes)

	// Call Claude Vision to analyze the image
	requestBody := map[string]any{
		"model":      "claude-sonnet-4-5-20250929",
		"max_tokens": 200,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       imgBase64,
						},
					},
					{
						"type": "text",
						"text": fmt.Sprintf(`This is a satellite image of a potential clothing donation bin placement at coordinates (%.4f, %.4f), near "%s".

Answer YES or NO: Is this a suitable spot for a clothing donation bin?

Good spots: retail parking lots, gas station lots, strip mall plazas, church parking lots, grocery store parking lots. Look for visible parking areas and commercial buildings.

Bad spots: residential houses/backyards, industrial warehouse rooftops, empty lots, freeways, dense apartment complexes with no public parking.

Respond in this exact format:
VERDICT: YES or NO
REASON: one sentence why`, lat, lng, businessName),
					},
				},
			},
		},
	}

	bodyJSON, _ := json.Marshal(requestBody)
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", anthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	visionClient := &http.Client{Timeout: 30 * time.Second}
	visionResp, err := visionClient.Do(req)
	if err != nil {
		log.Printf("⚠️ [Vision] Claude API error: %v", err)
		return true, "vision API failed"
	}
	defer visionResp.Body.Close()

	visionBody, _ := io.ReadAll(visionResp.Body)
	var visionResult struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(visionBody, &visionResult) != nil || len(visionResult.Content) == 0 {
		log.Printf("⚠️ [Vision] Failed to parse response")
		return true, "parse failed"
	}

	response := visionResult.Content[0].Text
	isGood := strings.Contains(strings.ToUpper(response), "VERDICT: YES")
	reason := response
	if idx := strings.Index(response, "REASON:"); idx >= 0 {
		reason = strings.TrimSpace(response[idx+7:])
	}

	log.Printf("👁️ [Vision] %.4f,%.4f (%s): %s — %s", lat, lng, businessName, map[bool]string{true: "PASS", false: "FAIL"}[isGood], reason)
	return isGood, reason
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
		"https://discover.search.hereapi.com/v1/discover?in=circle:%.6f,%.6f;r=8000&q=%s&limit=15&apiKey=%s",
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

// osrmRouteResult holds both distance and duration from OSRM
type osrmRouteResult struct {
	DistanceMiles float64
	DurationMins  float64
}

// getOSRMRoute returns driving distance (miles) and duration (minutes) between two points
func getOSRMRoute(lat1, lng1, lat2, lng2 float64) osrmRouteResult {
	osrmURL := os.Getenv("OSRM_SERVER_URL")
	if osrmURL == "" {
		osrmURL = "http://router.project-osrm.org"
	}
	url := fmt.Sprintf("%s/route/v1/driving/%.6f,%.6f;%.6f,%.6f?overview=false",
		osrmURL, lng1, lat1, lng2, lat2)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return osrmRouteResult{}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Code   string `json:"code"`
		Routes []struct {
			Distance float64 `json:"distance"`
			Duration float64 `json:"duration"`
		} `json:"routes"`
	}
	if json.Unmarshal(body, &result) != nil || result.Code != "Ok" || len(result.Routes) == 0 {
		return osrmRouteResult{}
	}
	return osrmRouteResult{
		DistanceMiles: result.Routes[0].Distance / 1609.34,
		DurationMins:  result.Routes[0].Duration / 60.0,
	}
}

// getOSRMDrivingDistMiles returns the driving distance between two points via OSRM (legacy wrapper)
func getOSRMDrivingDistMiles(lat1, lng1, lat2, lng2 float64) float64 {
	return getOSRMRoute(lat1, lng1, lat2, lng2).DistanceMiles
}

// getOSRMDriveTimeMins returns the driving time in minutes between two points via OSRM
func getOSRMDriveTimeMins(lat1, lng1, lat2, lng2 float64) float64 {
	return getOSRMRoute(lat1, lng1, lat2, lng2).DurationMins
}
