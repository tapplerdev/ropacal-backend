package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

// ESRIEnrichmentResult contains demographic and economic data for a location
type ESRIEnrichmentResult struct {
	// Demographics
	MedianHouseholdIncome float64 `json:"median_household_income"`
	TotalPopulation       float64 `json:"total_population"`
	PopulationGrowthRate  float64 `json:"population_growth_rate"` // 2026-2031 %
	DisposableIncome      float64 `json:"disposable_income"`      // Median disposable income

	// Foot traffic proxy
	DaytimePopulation     float64 `json:"daytime_population"`
	DaytimePopDensity     float64 `json:"daytime_pop_density"`

	// Clothing/textile signal
	AvgClothingSpend      float64 `json:"avg_clothing_spend"` // Avg household clothing spending

	// Safety
	CrimeIndex            float64 `json:"crime_index"` // Total crime index (100 = national avg)

	// Metadata
	HasData               bool    `json:"has_data"`
}

// analysisVariables are the ESRI variable names we request per location
var esriAnalysisVariables = []string{
	"KeyUSFacts.MEDHINC_CY",         // Median Household Income
	"KeyUSFacts.TOTPOP_CY",          // Total Population
	"KeyUSFacts.POPGRWCYFY",         // Population Growth Rate 2026-2031
	"DaytimePopulation.DPOP_CY",     // Total Daytime Population
	"DaytimePopulation.DPOPDENSCY",  // Daytime Population Density
	"clothing.X5001_A",              // Avg Household Clothing Spending
	"disposableincome.MEDDI_CY",     // Median Disposable Income
	"crime.CRMCYTOTC",               // Total Crime Index
}

// EnrichLocation calls ESRI GeoEnrichment API for a single lat/lng point
func EnrichLocation(lat, lng float64) (*ESRIEnrichmentResult, error) {
	apiKey := os.Getenv("ESRI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ESRI_API_KEY not set")
	}

	// Build analysis variables JSON array
	varsJSON, _ := json.Marshal(esriAnalysisVariables)

	// Build study area
	studyArea := fmt.Sprintf(`[{"geometry":{"x":%.6f,"y":%.6f}}]`, lng, lat)

	// Build request
	params := url.Values{}
	params.Set("f", "json")
	params.Set("token", apiKey)
	params.Set("studyAreas", studyArea)
	params.Set("analysisVariables", string(varsJSON))
	params.Set("returnGeometry", "false")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.PostForm(
		"https://geoenrich.arcgis.com/arcgis/rest/services/World/geoenrichmentserver/Geoenrichment/enrich",
		params,
	)
	if err != nil {
		return nil, fmt.Errorf("ESRI API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var esriResp struct {
		Results []struct {
			Value struct {
				FeatureSet []struct {
					Features []struct {
						Attributes map[string]interface{} `json:"attributes"`
					} `json:"features"`
				} `json:"FeatureSet"`
			} `json:"value"`
		} `json:"results"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &esriResp); err != nil {
		return nil, fmt.Errorf("ESRI response parse error: %w", err)
	}

	if esriResp.Error != nil {
		return nil, fmt.Errorf("ESRI API error %d: %s", esriResp.Error.Code, esriResp.Error.Message)
	}

	if len(esriResp.Results) == 0 || len(esriResp.Results[0].Value.FeatureSet) == 0 ||
		len(esriResp.Results[0].Value.FeatureSet[0].Features) == 0 {
		return &ESRIEnrichmentResult{HasData: false}, nil
	}

	attrs := esriResp.Results[0].Value.FeatureSet[0].Features[0].Attributes

	result := &ESRIEnrichmentResult{
		HasData:               getFloat(attrs, "HasData") == 1,
		MedianHouseholdIncome: getFloat(attrs, "MEDHINC_CY"),
		TotalPopulation:       getFloat(attrs, "TOTPOP_CY"),
		PopulationGrowthRate:  getFloat(attrs, "POPGRWCYFY"),
		DaytimePopulation:     getFloat(attrs, "DPOP_CY"),
		DaytimePopDensity:     getFloat(attrs, "DPOPDENSCY"),
		AvgClothingSpend:      getFloat(attrs, "X5001_A"),
		DisposableIncome:      getFloat(attrs, "MEDDI_CY"),
		CrimeIndex:            getFloat(attrs, "CRMCYTOTC"),
	}

	return result, nil
}

// EnrichLocationsBatch enriches multiple locations in a single API call
// ESRI supports multiple study areas in one request (saves API calls + cost)
func EnrichLocationsBatch(locations []struct{ Lat, Lng float64 }) ([]ESRIEnrichmentResult, error) {
	apiKey := os.Getenv("ESRI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ESRI_API_KEY not set")
	}

	if len(locations) == 0 {
		return nil, nil
	}

	// Build study areas array
	studyAreas := make([]map[string]interface{}, len(locations))
	for i, loc := range locations {
		studyAreas[i] = map[string]interface{}{
			"geometry": map[string]float64{"x": loc.Lng, "y": loc.Lat},
			"attributes": map[string]interface{}{"id": fmt.Sprintf("%d", i)},
		}
	}
	studyAreasJSON, _ := json.Marshal(studyAreas)
	varsJSON, _ := json.Marshal(esriAnalysisVariables)

	params := url.Values{}
	params.Set("f", "json")
	params.Set("token", apiKey)
	params.Set("studyAreas", string(studyAreasJSON))
	params.Set("analysisVariables", string(varsJSON))
	params.Set("returnGeometry", "false")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.PostForm(
		"https://geoenrich.arcgis.com/arcgis/rest/services/World/geoenrichmentserver/Geoenrichment/enrich",
		params,
	)
	if err != nil {
		return nil, fmt.Errorf("ESRI batch request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var esriResp struct {
		Results []struct {
			Value struct {
				FeatureSet []struct {
					Features []struct {
						Attributes map[string]interface{} `json:"attributes"`
					} `json:"features"`
				} `json:"FeatureSet"`
			} `json:"value"`
		} `json:"results"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &esriResp); err != nil {
		return nil, fmt.Errorf("ESRI batch parse error: %w", err)
	}

	if esriResp.Error != nil {
		return nil, fmt.Errorf("ESRI API error %d: %s", esriResp.Error.Code, esriResp.Error.Message)
	}

	results := make([]ESRIEnrichmentResult, len(locations))

	if len(esriResp.Results) > 0 && len(esriResp.Results[0].Value.FeatureSet) > 0 {
		features := esriResp.Results[0].Value.FeatureSet[0].Features
		for i, feat := range features {
			if i >= len(results) {
				break
			}
			attrs := feat.Attributes
			results[i] = ESRIEnrichmentResult{
				HasData:               getFloat(attrs, "HasData") == 1,
				MedianHouseholdIncome: getFloat(attrs, "MEDHINC_CY"),
				TotalPopulation:       getFloat(attrs, "TOTPOP_CY"),
				PopulationGrowthRate:  getFloat(attrs, "POPGRWCYFY"),
				DaytimePopulation:     getFloat(attrs, "DPOP_CY"),
				DaytimePopDensity:     getFloat(attrs, "DPOPDENSCY"),
				AvgClothingSpend:      getFloat(attrs, "X5001_A"),
				DisposableIncome:      getFloat(attrs, "MEDDI_CY"),
				CrimeIndex:            getFloat(attrs, "CRMCYTOTC"),
			}
		}
	}

	log.Printf("📊 [ESRI] Enriched %d locations (8 variables each = %d attributes)",
		len(locations), len(locations)*len(esriAnalysisVariables))

	return results, nil
}

// getFloat safely extracts a float64 from an interface{} map value
func getFloat(attrs map[string]interface{}, key string) float64 {
	v, ok := attrs[key]
	if !ok || v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return val
	case int:
		return float64(val)
	case int64:
		return float64(val)
	default:
		return 0
	}
}
