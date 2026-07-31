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
- **OSRM** — far more load-bearing than "snap-to-road" suggests. SIX call sites: the **distance/duration matrix the VRP solver reasons over** (`ortools_optimizer.go:245`, `route_optimization_shared.go:85` — `/table`), GPS snap (`roads/osrm_client.go` — `/match`), the manager map's live driver polyline (`directions.go:89` — `/route`), route-template distances (`routes.go:1498`), and AI-recommender drive distances (`chat_locations.go:3079`). If OSRM returns wrong numbers the optimizer produces a confident, wrong route — see Service topology for the hosting trap.
- **OR-Tools** — the ACTIVE VRP optimizer, and **NOT in-process**: Go has no OR-Tools binding, so `ORToolsOptimizer` is an HTTP client. It fetches the OSRM matrix, then `POST {ORTOOLS_SERVICE_URL}/optimize` to the **`ortools-service` Python/FastAPI microservice** (separate repo + Railway deployment, see Service topology). `optimization.NewOptimizer()` defaults to it and deliberately never falls back — if that service is down, every shift start and mid-shift re-optimize fails with `OR-Tools service call failed`. Mapbox/Google/HERE optimizer clients are retained ONLY for side-by-side comparison, not the live path.
- **HERE Maps** — geocoding via `services/geocoding_service.go` (`HEREGeocodeResponse`), reached from `handlers/routes.go`, `bins.go`, `shift_complete.go`. The **public geocoding endpoints use GOOGLE instead** (`services/geocoding.go` → `NewGeocodingService()` → `GOOGLE_MAPS_API_KEY`). Two geocoders coexist; do not assume HERE from this line alone.
- **Apple FindMy/AirTag** — bin tracking via AirTags

## Service topology — THIS REPO IS THE HUB

Binly is five separately-deployed services in four Railway projects. Nothing in
`railway status` tells you a service's source repo — that needs the Railway
GraphQL API (`https://backboard.railway.app/graphql/v2`; note team projects live
under `me.workspaces[].projects`, NOT `me.projects`; token at
`~/.railway/config.json`). Recording them here so nobody has to rediscover it.

| Service | Repo | Railway | Reached via | If it's down |
|---|---|---|---|---|
| **ropacal-backend** (this) | `tapplerdev/ropacal-backend` | project `binly-backend-service` | — | everything |
| **ortools-service** | `tapplerdev/ortools-service` | same project, private networking | `ORTOOLS_SERVICE_URL` | **every shift start + re-optimize fails**, no fallback |
| **binly-osrm-service** | `tapplerdev/binly-osrm-service` | own project `osrm-routing-service` | `OSRM_SERVER_URL` | VRP matrix, GPS snap, map polylines |
| **binly-centrifugo-service** | `tapplerdev/binly-centrifugo-service` | own project | `CENTRIFUGO_API_URL` | all realtime |
| **binly-findmy-bridge** | `tapplerdev/binly-findmy-bridge` | same project | `FINDMY_BRIDGE_URL` | AirTag positions go stale |
| **ropacal-placement** | `tapplerdev/ropacal-placement` | **not deployed** | 3 Postgres tables, no HTTP | nothing — it is offline-only |

### ropacal-placement — the modeling sidecar (built, not deployed, deliberately)

A Python service that re-fits the placement site score from realized outcomes,
instead of the exponents being hand-tuned. Cloned locally; **no Dockerfile and no
Railway service, on purpose.** The 2026-07-24 research decision was explicit:

> **"NO request-path Python sidecar — batch job writes coefficients to Go."**

Go must never call it synchronously. It meets Go only in Postgres:
`placement_decisions` (Go writes), `placement_model` + `placement_scores` (Python
writes, Go reads a column). `ortools-service` is the counter-example — a
request-path Python dependency whose outage kills every shift start.

**Status 2026-07-31:** Go now writes `placement_decisions` (see
`handlers/placement_logging.go`). `signalcheck` was run against the real fleet
and returned **out-of-sample rho = +0.167 on n=79, BELOW the 0.222 significance
bar** — site features only weakly predict fill rate. An attempted label fix
(dropping censored 100%-full observations) made it WORSE (+0.090), because a
bin found full is the strongest evidence the site is good. **So the frozen
formula stays and the model layer waits for more bins.** Re-run `signalcheck`
around 250-300 bins before building anything on top of it.

The five JSONB keys Go freezes are a contract with
`ropacal_placement/features.py` FEATURE_SPEC — a rename on either side silently
desyncs training, so `placement_logging_test.go` pins them.

`ortools-service` and `binly-findmy-bridge` are cloned locally under
`~/Documents/GitHub`; the OSRM and Centrifugo services are **not** — they exist
only on GitHub and Railway, so grepping the local filesystem will not find them.

### OSRM hosting — the trap (verified 2026-07-30)

A self-hosted OSRM **exists and works**: `binly-osrm-service` builds
`california-latest.osm.pbf` through the full MLD pipeline at image-build time and
serves it at `https://binly-osrm-service-production.up.railway.app`. But:

- **staging** `OSRM_SERVER_URL` points at it. **production still points at the
  public demo server** — never flipped off the code default.
- The self-hosted image carries **CALIFORNIA ONLY**. A Toronto coordinate snaps to
  a road in the California desert **3,185 km away** and `/table` returns
  **`code: Ok` with 0.0 km** — it fails SILENTLY. The VRP solver would receive an
  all-zeros matrix and emit confident garbage. So flipping production to it
  as-is would break any non-California tenant invisibly.
- Image last deployed 2026-02-19; the road data does not refresh on its own.
- **Any OSRM smoke test must assert real distances in every served metro.** Status
  codes pass while returning zero.

Fix for a second region: add the region's PBF to the Dockerfile, `osmium merge`,
rebuild, then flip production's `OSRM_SERVER_URL` to the staging value.

## Environment Variables

See `.env.example`. Key vars:
- `DATABASE_URL` — PostgreSQL connection string (required)
- `DB_MAX_OPEN_CONNS` / `DB_MAX_IDLE_CONNS` / `DB_CONN_MAX_LIFETIME` / `DB_CONN_MAX_IDLE_TIME` — pool tuning (defaults 20 / 10 / 30m / 5m; logged at boot). This process owns the only pool against the DB (Railway allows ~100 conns, no PgBouncer)
- `APP_JWT_SECRET` — JWT signing secret (required)
- ~~`APP_SHARED_PASSWORD`~~ — **DEAD CONFIG**: listed here and in `.env.example`, but zero references in any `.go` file. Safe to delete.
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
- `MAPBOX_ACCESS_TOKEN` — only the comparison optimizer; the active OR-Tools path needs no token (it calls `ortools-service` over HTTP)
- `HERE_API_KEY` — HERE geocoding (bins/routes/shift-complete paths only; the public geocoding endpoints use Google)
- `INTERNAL_API_KEY` — secures internal endpoints (FindMy bridge, org + platform-admin provisioning)
- **ROUTING BEHAVIOUR — these four decide what actually runs, and none were documented before 2026-07-30:**
  - `OPTIMIZER_TYPE` — selects the optimizer; prod `ortools`. Unset/invalid also defaults to OR-Tools.
  - `ORTOOLS_SERVICE_URL` — the Python solver. Prod `http://ortools-service.railway.internal:8000` (Railway private networking, same project). Code default `http://localhost:8000`.
  - `OSRM_SERVER_URL` — **code default is the PUBLIC DEMO SERVER** (`http://router.project-osrm.org`), which is non-commercial-only, 1 req/s, revocable without notice. See Service topology.
  - `SNAP_PROVIDER` — **code default is `google`** (Google Roads API), NOT OSRM. Prod explicitly sets `osrm`. A fresh environment silently snaps via Google unless this is set.
- `GOOGLE_MAPS_API_KEY` — Google geocoding (the public geocoding endpoints) + Google Roads snapping + chat_locations
- `PLATFORM_JWT_SECRET` — signs cross-tenant operator tokens. Unset = the whole `/api/platform` surface 404s. MUST differ from `APP_JWT_SECRET` (asserted at boot)
- `TENANCY_ALLOW_BYPASSRLS` — RLS escape hatch; leave unset
- Also read, previously undocumented: `ANTHROPIC_API_KEY` (chat + AI ops agent), `ESRI_API_KEY`, `FINDMY_BRIDGE_URL`, `HERE_APP_ID`, `ALLOWED_ORIGINS`, `GOOGLE_SERVICE_ACCOUNT_JSON` (comparison optimizer only)

## Project Structure

```
cmd/server/main.go          — Entry point: boots DB, services, background workers, routes
internal/
  database/
    database.go             — PostgreSQL connection + a LEGACY idempotent inline DDL list (137 CREATE/ALTER statements) still executed every boot by database.Migrate(), called from main.go AFTER goose
    goose.go                — goose versioned migrations (runs FIRST); baselines an existing DB by stamping v1 without executing
    seed.go                 — Seeds initial users and bins
    route_tasks.go          — Route task DB queries (CreateShiftWithTasks, GetShiftTasks[WithDeleted])
  moverequest/              — move-request DOMAIN: typed Status + guarded transitions, Store, Create/EditFields, PlanAssignment, LogAssignmentChange/history, Parse{MoveType,DisposalAction}
  itinerary/                — route_tasks DOMAIN (single writer): AddMove, RemoveByIDs, Resequence, ReconcileMove, CountStops/RecomputeShiftCounts, Parse{TaskType,TimeWindowType}
  shift/                    — shift enum domain: ParseType (shift_type)
  bindomain/                — bin enum domain: ParseStatus (bins.status); named bindomain to avoid shadowing local `bin` vars
  geo/                      — haversine / distance helpers; boundaries.go = embedded boundary polygons: TIGER 2025 CA cities (483, data/ca_places.json) + LA Times LA neighborhoods (114 districts, data/la_districts.json, CC-BY). Lookup routes by HERE type (district→districts, county/postal/state→nil, else→cities) with geo-sanity check + point-in-polygon (holes/MultiPolygon) + DistanceMeters
  handlers/                 — HTTP handlers (thin; orchestrate the domains above)
    shifts.go               — (1.2k) Shift lifecycle; split into shift_{optimization,tasks_edit,query,complete,cancel}.go + driver_location.go
    bin_move_requests.go    — (490) Move request create/schedule; update/assign/cancel split into bin_move_{update,assignment,lifecycle}.go
    routes.go               — (1.5k) Route template CRUD, optimization previews
    bins.go                 — (1.3k) Bin CRUD, batch geocoding, retirement
    zones.go                — (1.1k) No-go zones, field observations, incidents
    potential_locations.go  — (790) Potential bin locations, conversion to bins
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
    geocoding.go            — GOOGLE geocoder (GOOGLE_MAPS_API_KEY) — backs the PUBLIC geocoding endpoints
    geocoding_service.go    — HERE geocoder (HEREGeocodeResponse); its key is read in handlers/routes.go, not here
    route_optimizer.go      — Route optimization orchestration
  websocket/                — /ws route is CLOSED and broadcasts are no-ops (zero clients), BUT THE PACKAGE IS NOT DEAD: websocket.NewHub() builds the road snapper that handlers/driver_location.go:514 uses via hub.GetRoadsClient() on the live POST /api/driver/location path, and main.go still runs `go wsHub.Run()`. Deleting this package breaks GPS snapping. 52 BroadcastTo* call sites remain.
pkg/utils/response.go      — HTTP response helpers
internal/database/migrations/ — goose migrations (4 files; 00001 is a pg_dump baseline of prod)
  .../migrations/archive/    — 22 pre-goose SQL files, applied BY HAND with nothing recording that they ran
```

## Database Schema (key tables)

All timestamps are Unix epoch (BIGINT). **Schema changes go through goose** (`internal/database/migrations/`, adopted 2026-07-30). A legacy inline DDL list in `database/database.go` ALSO still runs on every boot — it is idempotent, but it means the schema has two sources and a goose migration can be silently undone by the inline DDL if they disagree.

- **users** — id, email, password, name, role (`driver`|`admin`), organization_id. **Known bug:** `handlers/users.go` also accepts `manager`, but the DB CHECK constraint permits only `driver`/`admin` — so `POST /api/manager/users` with `role:"manager"` passes validation then 500s on insert. Fix the handler, not this line.
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

- **Public endpoints (the complete list):** `GET /health`, `POST /api/auth/login`, **`POST /api/platform/auth/login`** (cross-tenant operator login — rate-limited only, no auth middleware; the whole `/api/platform` surface 404s when `PLATFORM_JWT_SECRET` is unset), `/api/internal/*` (`INTERNAL_API_KEY` header — FindMy bridge + provisioning). Everything else requires an identity.
- **Everything else under /api:** JWT Bearer token required (`middleware.Auth`) — including all reads (bins, routes, zones, analytics, potential locations, `GET /api/areas/boundary`) and the geocoding/directions proxies (no tenant data, but paid API quota)
- **Admin endpoints:** JWT + `admin` role (`middleware.RequireRole("admin")`) — the `/api/manager/*` surface plus `PATCH /api/config/warehouse` and the route-template writes (`POST/PATCH/DELETE /api/routes*`)
- **Centrifugo proxies** (`POST /api/centrifugo/*`): called by the Centrifugo SERVER, not clients — guarded by the `CENTRIFUGO_PROXY_SECRET` shared header (`X-Centrifugo-Proxy-Secret`) — fail-open (warning only) while tenancy is dark, MANDATORY once tenancy is live (unset then = all proxy requests denied). Payload `user` identity is only trustworthy behind that guard. Tenant scope comes from the connection `meta` (`{"org_id": ...}`, minted into the connection token) which Centrifugo forwards only when its service config sets `proxy_include_connection_meta` — see the CENTRIFUGO SERVICE CONFIG note under Environment Variables. Tenancy live + no org in meta = clean Centrifugo-shaped denial.
- **`GET /ws`:** REMOVED (route closed 2026-07; both clients are Centrifugo-only). The `websocket` package is **NOT dead code** — the hub owns the road snapper used by `POST /api/driver/location` (see Project Structure). Revert = re-register the one route line in main.go
- JWT uses HMAC signing with `APP_JWT_SECRET`, includes user_id/email/role claims

## Background Workers (started in main.go)

SIX workers, not four (the last two were undocumented until 2026-07-30):

1. **Location Batch Writer** — every 30s, flushes Redis GPS → PostgreSQL `driver_current_location`
2. **Digest Scheduler** — hourly check, sends daily digest push notifications at 8 AM & 2 PM
3. **AirTag Drift Monitor** — every 5 min, detects unexpected bin movement via AirTag positions
4. **Move Request Monitor** — every 15 min, flags overdue/due-soon move requests
5. **Stale Shift Monitor** (`main.go:279`) — **automatically ENDS live shifts that stop reporting GPS.** Only started when Redis is present. If shifts are ending on their own, look here first.
6. **AI Operations Agent** (`main.go:285`) — 30-minute cycles, Anthropic-backed

All six loop per organization via `orgdb.ForEachActiveOrg`.

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
