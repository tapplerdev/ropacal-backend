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
- `DB_MAX_OPEN_CONNS` / `DB_MAX_IDLE_CONNS` / `DB_CONN_MAX_LIFETIME` / `DB_CONN_MAX_IDLE_TIME` — pool tuning (defaults 20 / 10 / 30m / 5m; logged at boot). This process owns the only pool against the DB (Railway allows ~100 conns, no PgBouncer)
- `APP_JWT_SECRET` — JWT signing secret (required)
- `APP_SHARED_PASSWORD` — shared login password
- `REDIS_URL` — Redis connection (defaults to `redis://localhost:6379`)
- `FIREBASE_CREDENTIALS_BASE64` or `FIREBASE_CREDENTIALS_FILE` — FCM push notifications
- `CENTRIFUGO_API_URL`, `CENTRIFUGO_API_KEY` — Centrifugo real-time messaging
- `CENTRIFUGO_PROXY_SECRET` — shared secret Centrifugo must send (header `X-Centrifugo-Proxy-Secret`) on its `/api/centrifugo/*` proxy calls. **REQUIRED once tenancy is live.** Unset + tenancy dark = proxies unprotected (historical status quo, loud boot warning). Unset + tenancy LIVE = every proxy request is DENIED, because the tenant comes from attacker-controllable request-body `meta`, so fail-open would mean anyone could name a victim org and forge GPS into it
- **WHERE THE CENTRIFUGO CONFIG ACTUALLY LIVES:** the Centrifugo service deploys
  from the GitHub repo **`tapplerdev/binly-centrifugo-service`** (Railway project
  `binly-centrifugo-service`, service id `e059b63c-ebc7-47a3-a59a-e1e389cf414a`).
  Its `config.json` holds the proxies, namespaces and `allowed_origins`, and the
  Dockerfile bakes it into the image — so there are **no proxy env vars on the
  Railway service**, only `CENTRIFUGO_VAR_PROXY_SECRET`, which the config
  interpolates as `${CENTRIFUGO_VAR_PROXY_SECRET}` into each proxy's
  `http.static_headers`. `railway status` does NOT expose the source repo; it was
  found via the Railway GraphQL API (`service(id:){ serviceInstances{ edges{ node{
  source{ image repo } } } } }`). Changing anything there redeploys the broker and
  drops live WebSocket connections, so check `num_clients` via the Centrifugo
  `/api/info` endpoint first. As of 2026-07-30 the image is PINNED to
  `centrifugo/centrifugo:v6.9.1`; it was previously the floating `:v6`, which
  self-upgraded 6.6.0 -> 6.9.1 with no commit.
- **CENTRIFUGO SERVICE CONFIG (ops, set BOTH together on the external Centrifugo service):** (1) the static `X-Centrifugo-Proxy-Secret` header on the subscribe/publish/publish-location proxy config, matching `CENTRIFUGO_PROXY_SECRET`; (2) **`proxy_include_connection_meta: true`** so proxy requests carry the connection's `meta` (the `{"org_id": ...}` claim minted into connection tokens). Without (2), once the tenancy migration is live EVERY proxy request is denied with "organization context required" — realtime subscriptions, publishes, and driver GPS all stop. (Key names are Centrifugo v4-style; on v5+ granular proxies the per-proxy equivalents are `include_connection_meta` and `static_http_headers`.)
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
  websocket/                — Native WebSocket hub (legacy; /ws route CLOSED 2026-07 — zero clients, broadcasts are no-ops; package pending deletion)
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

## Multi-tenancy

Live since 2026-07-29: shared DB, `organization_id` on 35 tables, RLS keyed on
`current_setting('app.org_id')`, app connects as non-superuser `binly_app`
(owns all tables, `FORCE ROW LEVEL SECURITY`). Request path binds the org from
the JWT via `middleware.Org` -> `orgdb.From(r)`; workers loop per org via
`orgdb.ForEachActiveOrg`; non-request paths use `orgdb.System`.

**Several gates relax at exactly ONE organization and harden at two or more**
(proxy secret, proxy connection-meta, login org slug, the RLS boot tripwire).
Read `TENANCY_BACKLOG.md` before onboarding a second tenant — two Centrifugo
config fields are a hard prerequisite, not polish.

## API Auth

- **Public endpoints (the complete list):** `GET /health`, `POST /api/auth/login`, `/api/internal/*` (`INTERNAL_API_KEY` header — FindMy bridge). Everything else requires an identity.
- **Everything else under /api:** JWT Bearer token required (`middleware.Auth`) — including all reads (bins, routes, zones, analytics, potential locations, `GET /api/areas/boundary`) and the geocoding/directions proxies (no tenant data, but paid API quota)
- **Admin endpoints:** JWT + `admin` role (`middleware.RequireRole("admin")`) — the `/api/manager/*` surface plus `PATCH /api/config/warehouse` and the route-template writes (`POST/PATCH/DELETE /api/routes*`)
- **Centrifugo proxies** (`POST /api/centrifugo/*`): called by the Centrifugo SERVER, not clients — guarded by the `CENTRIFUGO_PROXY_SECRET` shared header (`X-Centrifugo-Proxy-Secret`) — fail-open (warning only) while tenancy is dark, MANDATORY once tenancy is live (unset then = all proxy requests denied). Payload `user` identity is only trustworthy behind that guard. Tenant scope comes from the connection `meta` (`{"org_id": ...}`, minted into the connection token) which Centrifugo forwards only when its service config sets `proxy_include_connection_meta` — see the CENTRIFUGO SERVICE CONFIG note under Environment Variables. Tenancy live + no org in meta = clean Centrifugo-shaped denial.
- **`GET /ws`:** REMOVED (route closed 2026-07; both clients are Centrifugo-only). The `websocket` package + hub wiring remain as dead code pending full deletion; revert = re-register the one route line in main.go
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
