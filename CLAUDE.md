# Ropacal Backend

Go backend for the Ropacal/Binly bin management and logistics platform. Deployed on Railway.

## Quick Reference

- **Build:** `go build -o bin/server ./cmd/server`
- **Run:** `./start.sh` (builds + runs) or `go run ./cmd/server`
- **Port:** `$PORT` or `8080`
- **Entry point:** `cmd/server/main.go`

## Tech Stack

- **Go 1.25** with chi/v5 router
- **PostgreSQL** via sqlx (lib/pq driver)
- **Redis** — driver GPS location cache (10-min TTL)
- **Centrifugo** — real-time WebSocket message broker for live location/shift updates
- **Firebase Cloud Messaging** — push notifications to mobile drivers
- **OSRM** — snap-to-road for GPS accuracy, driving directions
- **Mapbox Optimization v2** — route optimization with vehicle capacity modeling
- **HERE Maps** — geocoding (forward + reverse)
- **Apple FindMy/AirTag** — bin tracking via AirTags

## Environment Variables

See `.env.example`. Key vars:
- `DATABASE_URL` — PostgreSQL connection string (required)
- `APP_JWT_SECRET` — JWT signing secret (required)
- `APP_SHARED_PASSWORD` — shared login password
- `REDIS_URL` — Redis connection (defaults to `redis://localhost:6379`)
- `FIREBASE_CREDENTIALS_BASE64` or `FIREBASE_CREDENTIALS_FILE` — FCM push notifications
- `CENTRIFUGO_API_URL`, `CENTRIFUGO_API_KEY` — Centrifugo real-time messaging
- `MAPBOX_ACCESS_TOKEN` — route optimization
- `HERE_API_KEY` — geocoding
- `INTERNAL_API_KEY` — secures internal endpoints (FindMy bridge)

## Project Structure

```
cmd/server/main.go          — Entry point: boots DB, services, background workers, routes
internal/
  database/
    database.go             — PostgreSQL connection + all migrations (inline SQL)
    seed.go                 — Seeds initial users and bins
    route_tasks.go          — Route task DB queries
  handlers/                 — HTTP handlers (~22k lines total)
    shifts.go               — (7.5k) Shift lifecycle, Mapbox optimization, history
    bin_move_requests.go    — (3.3k) Move request CRUD, scheduling, assignment
    routes.go               — (1.4k) Route template CRUD, optimization previews
    bins.go                 — (1k) Bin CRUD, batch geocoding, retirement
    zones.go                — (1k) No-go zones, field observations, incidents
    potential_locations.go  — (870) Potential bin locations, conversion to bins
    centrifugo*.go          — Centrifugo auth proxy + GPS location publish proxy
    analytics.go            — Area/city performance metrics
    auth.go                 — Login (JWT-based)
    users.go                — User CRUD
    checks.go               — Bin check-in records
    moves.go                — Bin physical move history
    directions.go           — OSRM driving directions/polylines
    geocoding.go            — HERE Maps geocoding
    notification_settings.go — Notification config
    user_notifications.go   — Per-user notification inbox
    app_error_logs.go       — Mobile app crash/error reporting
    manager_active_drivers.go — Fleet tracking with Redis GPS
    shift_dependencies.go   — Checks if entities are tied to active shifts
    bin_priority.go         — Priority scoring for bins
    bin_check_recommendations.go — Stale bin flagging
    compare_optimizer.go    — Side-by-side optimizer comparison
    driver_location.go      — Location update endpoint
    config.go               — Warehouse location config
    diagnostics.go          — Diagnostic log receiver
    digest.go               — Daily digest trigger
  helpers/
    move_request_history.go — Move request audit trail helpers
  middleware/
    auth.go                 — JWT auth + RequireRole("admin") middleware
  models/                   — 17 data models (structs with db/json tags)
  services/
    optimization/
      mapbox_optimizer.go   — Mapbox Optimization v2 API client
      google_optimizer.go   — Google Routes Optimization API client
      types.go              — Shared types: Location, Vehicle, Shipment
      optimizer.go          — Optimizer interface
    roads/
      osrm_client.go        — OSRM snap-to-road
      optimizer.go          — OSRM route optimization
      cache.go              — Road-snapping cache
      client.go, factory.go, interface.go
    redis/client.go         — Redis wrapper: save/get/delete driver locations
    centrifugo/client.go    — Centrifugo HTTP API client
    fcm.go                  — Firebase push notifications with dead-token cleanup
    digest_scheduler.go     — Daily digest at 8 AM & 2 PM
    airtag_monitor.go       — AirTag drift detection (every 5 min)
    move_request_monitor.go — Overdue/due-soon move alerts (every 15 min)
    location_batch_writer.go — Redis GPS → PostgreSQL (every 30s)
    notification_helpers.go — Notification building helpers
    geocoding*.go           — HERE Maps geocoding service
    route_optimizer.go      — Route optimization orchestration
  websocket/                — Native WebSocket hub (legacy, alongside Centrifugo)
pkg/utils/response.go      — HTTP response helpers
migrations/                 — 20 SQL migration files (applied via database.go)
```

## Database Schema (key tables)

All timestamps are Unix epoch (BIGINT). Migrations are inline in `database/database.go`.

- **users** — id, email, password, name, role (`driver`|`admin`)
- **bins** — bin_number, address, city, status (`active`|`missing`|`retired`|`in_storage`|`pending_move`), fill_percentage, lat/lon, retirement tracking
- **shifts** — driver_id, status (`inactive`|`ready`|`active`|`paused`|`ended`|`cancelled`), shift_type (`standard`|`custom`), custom start/end locations, scheduled_start/end, optimization_metadata
- **route_tasks** — shift_id, task_type (`collection`|`placement`|`pickup`|`dropoff`|`warehouse_stop`|`service`), sequence_order, bin_id, time windows, service duration, completion tracking
- **shift_bins** — shift_id, bin_id, sequence_order, stop_type (`collection`|`pickup`|`dropoff`), move_request_id
- **shift_history** — completed shift archive with performance metrics, end_reason
- **checks** — bin check-in records with fill_percentage, photo, shift linkage
- **moves** — bin physical move history
- **bin_move_requests** — scheduled moves with urgency, move_type (`store`|`pickup_only`|`relocation`), assignment tracking
- **potential_locations** — driver-suggested bin placement locations, conversion workflow
- **no_go_zones** — geographic zones with conflict scores, incident tracking
- **zone_incidents** — vandalism, theft, complaints linked to zones/bins/shifts
- **airtag_locations** — latest AirTag positions per tag
- **config** — key-value config (warehouse location, notification settings)
- **fcm_tokens** — push notification device tokens per user
- **driver_current_location** — latest GPS per driver (1 row per driver, upserted)
- **notification_log** / **user_notifications** — notification audit trail + per-user inbox
- **app_error_logs** — mobile app crash reporting

## API Auth

- **Public endpoints:** health, geocoding, directions, bins (read), routes, zones, analytics, potential locations
- **Driver endpoints:** JWT Bearer token required (`middleware.Auth`)
- **Manager endpoints:** JWT + `admin` role required (`middleware.RequireRole("admin")`)
- **Internal endpoints:** `INTERNAL_API_KEY` header (FindMy bridge)
- JWT uses HMAC signing with `APP_JWT_SECRET`, includes user_id/email/role claims

## Background Workers (started in main.go)

1. **Location Batch Writer** — every 30s, flushes Redis GPS → PostgreSQL `driver_current_location`
2. **Digest Scheduler** — hourly check, sends daily digest push notifications at 8 AM & 2 PM
3. **AirTag Drift Monitor** — every 5 min, detects unexpected bin movement via AirTag positions
4. **Move Request Monitor** — every 15 min, flags overdue/due-soon move requests

## Key Flows

### Shift Lifecycle
1. Manager creates shift via `POST /api/manager/shifts/create-with-tasks` (assigns driver, bins, tasks)
2. Driver calls `POST /api/driver/shift/preflight` (validates shift readiness)
3. Driver calls `POST /api/driver/shift/start` → triggers Mapbox route optimization
4. During shift: location tracking via Centrifugo, task completion via `POST /api/driver/shift/complete-task`
5. Driver ends shift → archived to `shift_history`

### Route Optimization (shifts.go → `optimizeRouteWithMapbox()`)
- Uses Mapbox Optimization v2 API
- Models bins as shipments with capacity constraints (placements consume capacity, collections don't)
- Supports custom start/end locations on vehicle
- Falls back to OSRM if Mapbox fails

### Real-time Location
- Driver publishes GPS to Centrifugo channel `driver:location:{driverId}`
- Backend intercepts via publish proxy → saves to Redis → optionally snaps to road via OSRM → returns modified coords for broadcast
- Dashboard subscribes to channels for live tracking

## Conventions

- All handler functions return `http.HandlerFunc` closures that capture `db`, `wsHub`, etc.
- Error responses use standard HTTP status codes with JSON bodies
- IDs are UUIDs (TEXT in Postgres, generated via `google/uuid`)
- Timestamps are Unix epoch seconds (BIGINT), except `scheduled_start/end` and `earliest/latest_arrival` which use TIMESTAMPTZ
- Logging uses emoji prefixes for visual parsing in Railway logs
