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
7. ESRI batch enrichment (ALL filtered candidates — clothing, crime, income, growth)
8. Score ALL candidates with v2 formula
9. Sort by score, take top N×3
10. POI density + anchor detection per candidate (HERE Browse API)
11. Final score (ESRI base × 0.75 + POI density × 0.25)
12. Vision verification — DISABLED (ESRI + POI density covers these cases)
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

### ESRI Base Score (applied to ALL candidates)
```
POI name boost:    Tier 1 anchor name match → POIScore = 1.5 (else 1.0)
Clothing spend:    20%  — normalize to $5,000/yr = 1.0
Crime index:       15%  — <100 = 1.0, 100-130 = 0.7, 130-200 = 0.3, >200 = 0.1
Income:            15%  — $80-150K = 1.0, >$150K = 0.8, scale below
Fill rate gap:     15%  — nearest bin's fill rate / max fill rate
POI name:          25%  — anchor boost / 1.5
Gap distance:       5%  — capped at 2 miles
Pop growth:         5%  — positive = bonus, negative = penalty
```

### Analog Model Bonus
+0.05 if candidate's ESRI profile matches top performer averages within 20%:
- Clothing spend: $5,400 ± 20%
- Income: $185,000 ± 20%
- Crime: 116 ± 20%

### Final Score (after POI density enrichment)
```
finalScore = (ESRI base score × 0.75) + (POI density score × 0.25)
```
Scaled to 0-10 range. Minimum cutoff: 5.5.

### POI Density Scoring
- Anchor + 5+ businesses: 1.0
- Anchor alone: 0.9
- 8+ non-B2B businesses: 0.8
- 5-7 businesses: 0.6
- 4 businesses: 0.4

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
