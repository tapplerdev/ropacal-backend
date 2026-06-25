# Binly Placement Algorithm v2

## Overview

The placement algorithm recommends locations for new donation bins. It searches for commercial businesses via HERE Discover API, filters candidates through a centralized pipeline, enriches ALL candidates with ESRI demographic data, scores them, then validates with POI density checks.

## Pipeline

```
1. Find demand areas (rank cities by existing bin fill rates)
2. Search for businesses (19 keywords across top 5 demand areas)
3. Search expansion cities (if expand mode or auto mode)
4. Collect all raw candidates
5. Dedup (0.15 mile threshold)
6. Centralized filter (no-go zones, B2B, gap check, mode filters)
7. ESRI batch enrichment (ALL filtered candidates)
8. ESRI demographic gate (reject: income <$50k, crime >200, clothing <$2.5k)
9. Preliminary sort (anchor name match + fill rate), take top N×4
10. POI density + anchor detection per candidate (HERE Browse API)
11. Site quality score (density × 0.50 + anchor × 0.30 + fill × 0.20) × 10
12. Reject below 4.0 cutoff
13. Return top N results
```

## Search Keywords

### Tier 1 — High-Value Anchors
Target, Walmart, Safeway, Trader Joe's, Costco, Home Depot, Lowe's, Grocery Outlet, Food Maxx, 99 Ranch, Lucky Supermarket, Whole Foods, Dollar Tree

### Tier 2 — Common Retail
CVS, Walgreens, grocery store, coffee shop

### Tier 3 — Location Types
shopping center, shopping plaza

### Removed from v1
gas station, laundromat, church parking lot

## Scoring Formula

### ESRI Demographic Gate (pass/fail, NOT scored)
Candidates in areas with bad demographics are rejected before scoring:
```
Income < $50K          → REJECT
Crime index > 200      → REJECT
Clothing spend < $2,500 → REJECT
```
Rationale: ESRI returns identical values for same-city candidates, so using it as a weighted score component compresses all scores into a narrow band. As a gate, it prevents bad markets without diluting site quality ranking.

### Site Quality Score (multiplicative, 0-10 scale)
After passing the ESRI gate, candidates are ranked by site quality using multiplicative aggregation. A weakness in ANY dimension tanks the score — no compensation.

**POI density** — continuous log-scaled:
```
densityScore = ln(1 + POI_count) / ln(1 + 20)
```
Examples: 4 POIs → 0.53, 7 POIs → 0.68, 12 POIs → 0.84, 14 POIs → 0.89

**Anchor tenant** — tiered by traffic draw:
```
National anchor (Target, Walmart, Costco, Home Depot, Lowe's, Safeway, Trader Joe's, Whole Foods): 1.0
Regional chain (CVS, Walgreens, Grocery Outlet, FoodMaxx, 99 Ranch, Lucky, Dollar Tree): 0.7
Non-anchor: 0.15
```

**Fill rate gap** — nearest bin's fill rate / max fill rate (floor 0.1)

**Formula:**
```
finalScore = (density ^ 0.5) × (anchor ^ 0.3) × (fill ^ 0.2) × 10
```
Minimum cutoff: 4.0.

### Score Tiers
- 8.0-9.0: National anchor + dense plaza (Target + 14 POIs = 9.0)
- 7.0-8.0: National anchor + moderate density OR regional chain + dense plaza
- 6.0-7.0: Regional chain + moderate density
- 5.0-6.0: Non-anchor dense plaza (11+ POIs)
- 4.0-5.0: Non-anchor commercial strip (7+ POIs)
- Below 4.0: Rejected (sparse/isolated)

### POI Density Scoring (v2 — continuous log-scaled)
- Uses `ln(1 + count) / ln(21)` for smooth 0-1 range
- 4 POIs: 0.53, 7: 0.68, 10: 0.79, 14: 0.89, 20: 1.0

## Filtering

All filtering is centralized in `filterCandidates()`. Applied consistently to ALL code paths (business search, GraphVenn hotspots, expansion).

### Filter Pipeline
1. **No-go zone** — reject if within any zone radius
2. **B2B keywords** — reject if name contains: repair, service, detail, auto, wholesale, warehouse, industrial, spa, salon, nail, wellness, massage, plumbing, dental, clinic, etc.
3. **Mall/Safeway** — reject names containing "mall" or "safeway"
4. **Gap check** — reject if < minGapMiles from any existing bin
   - v2 OSRM override: if driving distance > 3× straight-line, allow (cross-street awareness)
5. **Existing potentials** — reject if < minGapMiles from existing potential location
6. **Infill mode** — reject if > 3 miles from nearest existing bin
7. **Expand mode** — reject if candidate's city has any active bin

### Gap Distances
- Auto mode: 0.3 miles
- Infill mode: 0.15 miles
- Expand mode: 0.3 miles

## Placement Modes

### Auto (default)
Mixed strategy — 70% infill (near existing bins), 30% expansion (new cities).

### Infill
Only searches demand areas with avg fill ≥ 30%. Max 3-mile distance from nearest bin. Tighter gap (0.15 miles). No expansion candidates.

### Expand
Skips demand areas entirely. Searches expansion cities that have NO existing bins. Standard 0.3-mile gap.

## Data Sources

| Data | Source | Cost |
|------|--------|------|
| Business search | HERE Discover API | Free tier |
| POI density | HERE Browse API (300m radius) | Free tier |
| Demographics | ESRI GeoEnrichment | ~$0.01 per candidate |
| Reverse geocode | HERE Reverse Geocode | Free tier |
| Cross-street check | OSRM Route API | Free (public server) |
| Fill rates | PostgreSQL (checks table) | Free |
| No-go zones | PostgreSQL | Free |

## ESRI Variables Used
```
KeyUSFacts.MEDHINC_CY      — Median Household Income
KeyUSFacts.TOTPOP_CY       — Total Population
KeyUSFacts.POPGRWCYFY      — Population Growth Rate 2026-2031
DaytimePopulation.DPOP_CY  — Total Daytime Population
DaytimePopulation.DPOPDENSCY — Daytime Population Density
clothing.X5001_A            — Avg Household Clothing Spending
disposableincome.MEDDI_CY  — Median Disposable Income
crime.CRMCYTOTC            — Total Crime Index (100 = national avg)
```

## Key Findings from Correlation Analysis

Based on analysis of 20 bins (top 10 + bottom 10 performers), using post-move check data only:

| Metric | Correlation with Fill Rate | Weight in v2 |
|--------|---------------------------|-------------|
| Clothing Spend | r = +0.446 | 20% |
| Crime Index | r = -0.419 | 15% |
| Income | r = +0.218 | 15% |
| Daytime Population | r = +0.044 (neutral) | Not used |
| Population Growth | r = -0.080 (neutral) | 5% |

Demographics explain ~30-40% of bin performance. The remaining 60% is physical placement (which road, parking lot visibility, foot traffic patterns) — handled by POI density + anchor detection.

## Files

- `internal/handlers/chat_locations.go` — main algorithm (~1700 lines)
- `internal/handlers/esri_enrichment.go` — ESRI GeoEnrichment API helper
- `internal/handlers/chat.go` — tool registration + system prompt
