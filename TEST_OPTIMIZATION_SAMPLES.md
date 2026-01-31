# Route Optimization v2 API - Testing Guide

## Endpoint

**POST** `https://ropacal-backend-production.up.railway.app/api/routes/test-optimization`

**Local:** `http://localhost:8080/api/routes/test-optimization`

## Description

Simple testing endpoint for Mapbox Optimization v2 API that accepts raw coordinates without needing database bin records. Perfect for quick testing with Insomnia or Postman.

## Features

- ✅ No database setup required - just send coordinates
- ✅ Supports up to 1000 locations
- ✅ Includes 5-minute service duration per stop
- ✅ Returns optimized order with distance and time estimates
- ✅ Async polling (1-5 seconds response time)

## Request Format

```json
{
  "locations": [
    {
      "name": "Bin 1",
      "latitude": 11.2256,
      "longitude": -74.1990
    },
    {
      "name": "Bin 2",
      "latitude": 11.2301,
      "longitude": -74.1945
    }
  ]
}
```

### Fields

- `locations` (array, required): Array of location objects
  - `name` (string, optional): Custom name for the location (defaults to `location-0`, `location-1`, etc.)
  - `latitude` (number, required): Latitude coordinate
  - `longitude` (number, required): Longitude coordinate

## Sample Requests

### Small Test (3 locations - Santa Marta area)

```json
{
  "locations": [
    {
      "name": "Downtown Market",
      "latitude": 11.2407,
      "longitude": -74.2002
    },
    {
      "name": "Beach Plaza",
      "latitude": 11.2256,
      "longitude": -74.1990
    },
    {
      "name": "Shopping Center",
      "latitude": 11.2301,
      "longitude": -74.1945
    }
  ]
}
```

### Medium Test (8 locations - Santa Marta)

```json
{
  "locations": [
    {
      "name": "Location 1",
      "latitude": 11.2407,
      "longitude": -74.2002
    },
    {
      "name": "Location 2",
      "latitude": 11.2256,
      "longitude": -74.1990
    },
    {
      "name": "Location 3",
      "latitude": 11.2301,
      "longitude": -74.1945
    },
    {
      "name": "Location 4",
      "latitude": 11.2150,
      "longitude": -74.2050
    },
    {
      "name": "Location 5",
      "latitude": 11.2450,
      "longitude": -74.1900
    },
    {
      "name": "Location 6",
      "latitude": 11.2200,
      "longitude": -74.2100
    },
    {
      "name": "Location 7",
      "latitude": 11.2350,
      "longitude": -74.1950
    },
    {
      "name": "Location 8",
      "latitude": 11.2100,
      "longitude": -74.2000
    }
  ]
}
```

### Large Test (20 locations - Wider area)

```json
{
  "locations": [
    {"latitude": 11.2407, "longitude": -74.2002},
    {"latitude": 11.2256, "longitude": -74.1990},
    {"latitude": 11.2301, "longitude": -74.1945},
    {"latitude": 11.2150, "longitude": -74.2050},
    {"latitude": 11.2450, "longitude": -74.1900},
    {"latitude": 11.2200, "longitude": -74.2100},
    {"latitude": 11.2350, "longitude": -74.1950},
    {"latitude": 11.2100, "longitude": -74.2000},
    {"latitude": 11.2500, "longitude": -74.1850},
    {"latitude": 11.2050, "longitude": -74.2150},
    {"latitude": 11.2600, "longitude": -74.1800},
    {"latitude": 11.2000, "longitude": -74.2200},
    {"latitude": 11.2700, "longitude": -74.1750},
    {"latitude": 11.1950, "longitude": -74.2250},
    {"latitude": 11.2800, "longitude": -74.1700},
    {"latitude": 11.1900, "longitude": -74.2300},
    {"latitude": 11.2550, "longitude": -74.1920},
    {"latitude": 11.2180, "longitude": -74.2080},
    {"latitude": 11.2420, "longitude": -74.1870},
    {"latitude": 11.2280, "longitude": -74.2020}
  ]
}
```

## Response Format

```json
{
  "success": true,
  "message": "Route optimization completed successfully",
  "total_stops": 8,
  "total_distance_km": 12.45,
  "total_distance_miles": 7.74,
  "estimated_duration": "1.2 hours (72 minutes)",
  "duration_hours": 1.2,
  "service_duration_per_stop": "5 minutes (300 seconds)",
  "optimized_order": [
    {
      "index": 2,
      "name": "Location 3",
      "latitude": 11.2301,
      "longitude": -74.1945,
      "eta": "2025-01-30T10:15:30Z",
      "odometer_km": 2.5
    },
    {
      "index": 4,
      "name": "Location 5",
      "latitude": 11.2450,
      "longitude": -74.1900,
      "eta": "2025-01-30T10:25:45Z",
      "odometer_km": 5.2
    }
  ]
}
```

### Response Fields

- `success` (boolean): Whether optimization succeeded
- `message` (string): Status message
- `total_stops` (integer): Number of locations optimized
- `total_distance_km` (float): Total route distance in kilometers
- `total_distance_miles` (float): Total route distance in miles
- `estimated_duration` (string): Human-readable duration estimate
- `duration_hours` (float): Duration in hours (includes driving + 5 min per stop)
- `service_duration_per_stop` (string): Time spent at each location
- `optimized_order` (array): Stops in optimized sequence
  - `index` (integer): Original position in your request (0-based)
  - `name` (string): Location name
  - `latitude` (float): Latitude coordinate
  - `longitude` (float): Longitude coordinate
  - `eta` (string): Estimated time of arrival (ISO 8601)
  - `odometer_km` (float): Cumulative distance at this stop

## Testing with Insomnia

1. Create a new **POST** request
2. Set URL to: `https://ropacal-backend-production.up.railway.app/api/routes/test-optimization`
3. Set **Body** to **JSON**
4. Paste one of the sample requests above
5. Click **Send**
6. Response will take 1-5 seconds (async optimization)

## Testing with cURL

```bash
curl -X POST https://ropacal-backend-production.up.railway.app/api/routes/test-optimization \
  -H "Content-Type: application/json" \
  -d '{
    "locations": [
      {"name": "Location 1", "latitude": 11.2407, "longitude": -74.2002},
      {"name": "Location 2", "latitude": 11.2256, "longitude": -74.1990},
      {"name": "Location 3", "latitude": 11.2301, "longitude": -74.1945}
    ]
  }'
```

## Notes

- **Warehouse**: All routes start and end at the warehouse (hardcoded in backend)
- **Service Duration**: Each stop includes 5 minutes for bin collection
- **Max Locations**: 1000 per request (Mapbox v2 API limit)
- **Response Time**: 1-5 seconds (async optimization with polling)
- **Timeout**: 30 seconds max (will return error if optimization takes longer)

## Understanding the Optimized Order

The `optimized_order` array shows the best sequence to visit locations:

- **Original order**: [Location 1, Location 2, Location 3]
- **Optimized order**: May be [Location 3, Location 1, Location 2]
- The `index` field tells you which original location each stop represents

## Example: Interpreting Results

If you send:
```json
{
  "locations": [
    {"name": "A", "latitude": 11.24, "longitude": -74.20},
    {"name": "B", "latitude": 11.22, "longitude": -74.19},
    {"name": "C", "latitude": 11.23, "longitude": -74.19}
  ]
}
```

And get back:
```json
{
  "optimized_order": [
    {"index": 2, "name": "C"},
    {"index": 0, "name": "A"},
    {"index": 1, "name": "B"}
  ]
}
```

This means the optimal route is: **Warehouse → C → A → B → Warehouse**

## Error Responses

### Too Many Locations
```json
{
  "error": "Cannot optimize more than 1000 locations (Mapbox v2 API limit)"
}
```

### Invalid Request
```json
{
  "error": "Invalid request body"
}
```

### Optimization Timeout
```json
{
  "error": "Optimization timeout - please try again"
}
```
