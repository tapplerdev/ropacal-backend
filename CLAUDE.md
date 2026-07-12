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
- **OR-Tools** — route optimization with vehicle capacity modeling (the ACTIVE optimizer; runs in-process, `optimization.NewOptimizer()` defaults to it). Mapbox/Google/HERE optimizer clients are retained ONLY for side-by-side comparison, not the live path.
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
- `MAPBOX_ACCESS_TOKEN` — only the comparison optimizer (the active OR-Tools optimizer runs in-process, no token needed)
- `HERE_API_KEY` — geocoding
- `INTERNAL_API_KEY` — secures internal endpoints (FindMy bridge)

## Project Structure

```
cmd/server/main.go          — Entry point: boots DB, services, background workers, routes
internal/
  database/
    database.go             — PostgreSQL connection + all migrations (inline SQL)
    seed.go                 — Seeds initial users and bins
    route_tasks.go          — Route task DB queries (CreateShiftWithTasks, GetShiftTasks[WithDeleted])
  moverequest/              — move-request DOMAIN: typed Status + guarded transitions, Store, Create/EditFields, PlanAssignment, LogAssignmentChange/history, Parse{MoveType,DisposalAction}
  itinerary/                — route_tasks DOMAIN (single writer): AddMove, RemoveByIDs, Resequence, ReconcileMove, CountStops/RecomputeShiftCounts, Parse{TaskType,TimeWindowType}
  shift/                    — shift enum domain: ParseType (shift_type)
  bindomain/                — bin enum domain: ParseStatus (bins.status); named bindomain to avoid shadowing local `bin` vars
  geo/                      — haversine / distance helpers; boundaries.go = embedded boundary polygons: TIGER 2025 CA cities (483, data/ca_places.json) + LA Times LA neighborhoods (114 districts, data/la_districts.json, CC-BY). Lookup routes by HERE type (district→districts, county/postal/state→nil, else→cities) with geo-sanity check + point-in-polygon (holes/MultiPolygon) + DistanceMeters
  handlers/                 — HTTP handlers (thin; orchestrate the domains above)
    shifts.go               — (1.4k) Shift lifecycle; split into shift_{optimization,tasks_edit,query,complete,cancel}.go + driver_location.go
    bin_move_requests.go    — (400) Move request create/schedule; update/assign/cancel split into bin_move_{update,assignment,lifecycle}.go
    routes.go               — (1.5k) Route template CRUD, optimization previews
    bins.go                 — (1k) Bin CRUD, batch geocoding, retirement
    zones.go                — (1k) No-go zones, field observations, incidents
    potential_locations.go  — (870) Potential bin locations, conversion to bins
    centrifugo*.go          — Centrifugo auth proxy + GPS location publish proxy
    analytics.go            — Area/city performance metrics
    chat_locations.go       — recommend_bin_locations: target area = CORE + HALO. Candidates tagged locality in_area (coreContains) | near_area (halo + profile match). refineByAreaProfile keeps near picks only if similar to the area's median calibrated-feature profile (sim≥0.6) AND not a spatial outlier (>max(2.5×median,3km) from centroid). include_nearby toggle; response carries locality/distance_from_area_mi/area_match + in_area/nearby counts.
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
  models/                   — 18 data models (structs with db/json tags)
  services/
    optimization/
      optimizer.go          — Optimizer interface + NewOptimizer() (defaults to OR-Tools, the ACTIVE optimizer)
      ortools_optimizer.go  — OR-Tools client (ACTIVE — the live optimization path)
      mapbox_optimizer.go, google_optimizer.go, here_optimizer.go — retained for side-by-side comparison ONLY
      types.go              — Shared types: Location, Vehicle, Shipment
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
- **route_tasks** — shift_id, task_type (`collection`|`placement`|`pickup`|`dropoff`|`warehouse_stop`|`service`), sequence_order, bin_id, placement_source (`potential_location`|`redeployment`|legacy `warehouse`), move_request_id, time windows, service duration, completion tracking
- **shift_bins** — shift_id, bin_id, sequence_order, stop_type (`collection`|`pickup`|`dropoff`), move_request_id
- **shift_history** — completed shift archive with performance metrics, end_reason
- **checks** — bin check-in records with fill_percentage, photo, shift linkage
- **moves** — bin physical move history
- **bin_move_requests** — scheduled moves with urgency, move_type (`store`|`pickup_only`|`relocation`|`redeployment`), assignment tracking (see "Moves & Redeployments" under Key Flows)
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

- **Public endpoints:** health, geocoding, directions, bins (read), routes, zones, analytics, potential locations, `GET /api/areas/boundary` (true city polygon for the target-area overlay; requires name+lat+lng so the geo-sanity guard arms; districts/unknowns → found=false)
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
3. Driver calls `POST /api/driver/shift/start` → triggers OR-Tools route optimization
4. During shift: location tracking via Centrifugo, task completion via `POST /api/driver/shift/complete-task`. Manager mid-shift edits (add/remove move) re-run optimization.
5. Driver ends shift → archived to `shift_history`

### Route Optimization (shift_optimization.go → `optimization.NewOptimizer()`)
- Uses **OR-Tools** in-process (the active optimizer). Mapbox/Google/HERE clients exist only for side-by-side comparison.
- Models bins as shipments with capacity constraints (placements consume capacity, collections don't)
- Supports custom start/end locations on vehicle
- Each (re)optimize hard-deletes + regenerates the incomplete `warehouse_stop` tasks (system artifacts — no audit churn) and rewrites `sequence_order` in place, preserving bin task IDs

### Real-time Location
- Driver publishes GPS to Centrifugo channel `driver:location:{driverId}`
- Backend intercepts via publish proxy → saves to Redis → optionally snaps to road via OSRM → returns modified coords for broadcast
- Dashboard subscribes to channels for live tracking

### Moves & Redeployments — intent vs execution (two-layer model)

Every bin move has TWO representations that must never be conflated:

1. **`bin_move_requests` = INTENT** (manager-facing domain record): who requested,
   urgency, scheduled date, assignment (driver/shift), full audit history
   (`move_request_history`). All lifecycle machinery keys on it: one-open-move-per-bin
   partial unique index (409 on double-booking), cancel-with-bin-revert, overdue
   monitor, digest reports, the dashboard Move Requests table.
2. **`route_tasks` = EXECUTION** (how the driver physically does it). The shape
   depends on `move_type`:
   - `relocation` / `store` / `pickup_only` → a **pickup + dropoff pair** (dropoff =
     new location, or current warehouse for store/pickup_only).
   - `redeployment` → **ONE `placement` task** (Phase 2, 2026-07): the bin leaves the
     warehouse like any other placement, so the optimizer gets capacity math and
     Load-N warehouse batching with zero special-casing.

**Placement tasks therefore come in two live flavors — discriminate with
`placement_source` (structural fallback: a placement carrying `move_request_id` is a
redeployment):**

| | new-bin placement | redeployment |
|---|---|---|
| `placement_source` | `'potential_location'` (or NULL on old rows) | `'redeployment'` |
| identity | `potential_location_id` set, no move link | `bin_id` + `bin_number` + `move_request_id`, no PL |
| completion | CREATES a bin (converts the potential location) | finalizes the move: move→completed, existing bin→active @ destination (atomic, in the CompleteTask tx) |
| user-facing label | "Placement" | **"Redeployment · Bin #N"** — never expose that it's a placement internally |

(`placement_source='warehouse'` is a third, LEGACY value from pre-move-request
warehouse deployments — historical rows only; its completion path was RETIRED to a
410 tombstone and the pickup-pair redeploy optimizer handling deleted, 2026-07-10,
after verifying zero live shifts carried either shape.)

The layers are hard-linked so they can't drift: completing the placement finalizes
the move; cancelling the move soft-deletes the task and reverts the bin to
`in_storage`; removing the task from a shift releases the move back to the backlog.
Any new code that branches on pickup/dropoff for moves MUST also handle the
redeployment placement shape — key on `move_request_id != nil` / all-tasks-done per
move group, not on task-type pairs (this bit us 4 times in review: progress counts,
task removal, stale completion, warehouse-row minting).

## Conventions

- All handler functions return `http.HandlerFunc` closures that capture `db`, `wsHub`, etc.
- Error responses use standard HTTP status codes with JSON bodies
- IDs are UUIDs (TEXT in Postgres, generated via `google/uuid`)
- Timestamps are Unix epoch seconds (BIGINT), except `scheduled_start/end` and `earliest/latest_arrival` which use TIMESTAMPTZ
- Logging uses emoji prefixes for visual parsing in Railway logs
