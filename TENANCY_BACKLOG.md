# Multi-tenancy — backlog and operational state

Multi-tenancy went live **2026-07-29**: shared database, `organization_id` on 35
tables, Postgres Row-Level Security keyed on `current_setting('app.org_id')`.
The app connects as the non-superuser role `binly_app`, which owns all 41 tables
with `FORCE ROW LEVEL SECURITY`.

Current state: **one organization** (`ropacal`, id
`00000000-0000-0000-0000-000000000001`). Several safety gates deliberately behave
differently at one org versus several — see "The org-count rule" below, because it
determines what must happen before a second tenant onboards.

---

## The org-count rule (read this before onboarding anyone)

Three independent gates relax at exactly one organization and harden at two or
more. This is deliberate: with a single tenant, an attacker naming an org can
only name *that* tenant, which is the pre-tenancy exposure. With two, naming
becomes victim *selection*.

| Gate | 1 org | 2+ orgs |
|---|---|---|
| `middleware.CentrifugoProxyAuth` | warns, allows | **denies every proxy request** |
| `handlers.proxyOrgDB` (connection meta absent) | resolves the sole org | **denies** |
| `handlers.Login` (no `organization` in request) | logs into the sole org | **400, slug required** |
| `database.AssertRLSEnforced` (bypassing DB role) | warns | **refuses to boot** |

Both org-count lookups cache for 30s, so provisioning org #2 tightens them within
seconds without a restart. Verified live on 2026-07-29: a forged proxy request
naming the `ropacal` org returned 401 while a second org existed, and reverted to
200 within 30s of that org's deletion.

---

## BLOCKING — required before a second organization onboards

### 1. Centrifugo proxy secret (owner declined 2026-07-29; deferred here)
Realtime **will be fully denied** the moment a second org exists until both sides
are configured. Not optional, and not a code change — two config fields:

1. Backend (Railway `ropacal-backend` service):
   `CENTRIFUGO_PROXY_SECRET=$(openssl rand -hex 32)`
2. Centrifugo service (a **different Railway project** — not reachable from the
   backend deploy, which is why this can't be automated from here):
   - the same value as a static `X-Centrifugo-Proxy-Secret` header on the
     subscribe / publish / publish-location proxy config
   - **`proxy_include_connection_meta: true`** so proxy calls carry the
     connection's `{"org_id": ...}` meta

Key names are Centrifugo v4-style; on v5+ granular proxies the per-proxy
equivalents are `static_http_headers` and `include_connection_meta`.

Order matters: add the Centrifugo header **first**, then set the backend var.
That order cannot break realtime; the reverse can.

Why it is genuinely required: post-flip the proxies take their tenant from
`meta.org_id` in the **request body** (`handlers.proxyOrgDB`). RLS cannot defend
that — it is handed the org id rather than asked to validate it. Unauthenticated
callers could otherwise pick a victim tenant, publish forged GPS into its scope,
and trip the proximity auto-end path against its shifts.

### 2. Tenant provisioning flow
Org creation is currently manual SQL run as `postgres` (boot seeds are correctly
skipped once tenancy is live — see `cmd/server/main.go`, or they would NOT-NULL
violate and boot-loop). Needs a real path: create `organizations` row + first
admin user + that org's `warehouse_location` config row. Note `config.id` is a
serial integer, not a UUID.

### 3. Login screens need the `organization` field
The single-org grace disappears at two orgs — every client must then send the org
slug or get a 400.
- **Dashboard**: `lib/auth/queries.ts` login body + the login form.
- **Flutter** (4 sites, already scoped): `api_service.dart:190` login body,
  `auth_provider.dart:206/212`, `login_page.dart` (a field; consider persisting
  it beside the existing `remembered_email`), possibly `models/user.dart`.

Contract: `POST /api/auth/login` `{email, password, organization?}` where
`organization` is the org **slug**, case-insensitive. 400 names the missing field;
an unknown slug returns a 401 **identical** to bad credentials (no enumeration).

---

## Tier 1 realtime — before a second organization

Researched in full; not built. Channels are currently flat and org-blind.

- `company:events` is one channel carrying the whole operational feed (36 publish
  sites) and is subscribable by any admin of any org → must become
  `company:{orgID}:events`, with the publisher signature, 4 dashboard subscriber
  sites and the Flutter manager mode updated.
- `canViewDriverLocation` is role-only: any admin can live-track any driver given
  the UUID → add an org-equality predicate now that proxies carry org context.
- Redis GPS keys → `ropacal:org:{orgID}:driver:{id}:location` (the 10-minute TTL
  makes that cutover trivial).
- Shorten the Centrifugo connection-token TTL.

## Tier 2 — later

- Delete the `websocket` package and its ~50 `BroadcastToRole` call sites. The
  `/ws` route was closed 2026-07-29 (both clients are Centrifugo-only;
  `ApiConstants.wsUrl` in Flutter has zero consumers). Revert = re-register one
  line in `main.go`.
- `airtag_locations` matches on `bin_number`, which is **not unique** (prod has
  duplicates 56, 86, 116). Cross-tenant id collisions currently surface as the
  FindMy resolver's ambiguity skip — safe and loud, but it should be namespaced.
- Redis `KEYS` → `SCAN`; per-org rate limits.
- Drop the four `zz_backup_*` tables (RLS-denied to the app role, real data, no
  code path) and the four dead-but-populated tables:
  `driver_location_history` (94k rows), `driver_locations` (6k), `shift_bins`,
  `zone_risk_overrides`.

---

## Security items still open

- **Rotate the `postgres` password.** It was pasted into an assistant transcript
  repeatedly on 2026-07-29 (owner declined rotation at the time). `binly_app` is
  a separate credential and is what the app uses; `postgres` is now humans-only.
- **`middleware/auth.go`** logs *all* request headers when `Authorization` is
  missing. With ~40 endpoints now 401-ing internet scanners that is a growing
  noise/PII surface.
- **`InternalAPIKey`** (`airtag_accounts.go:27`) compares with `!=`, not
  constant-time — align with `CentrifugoProxyAuth`.
- **Claims-shape assertions**: a validly-signed token missing a claim used to
  panic; the checked form landed with tenancy, but audit any new claim reads.

## Known non-tenancy bugs found in passing (2026-07-29)

- `analytics.go` `area_score` returns values like 381 and 991 for what reads as a
  percentage-scaled metric. Pre-existing; the formula's `GREATEST(...)` multiplier
  is unbounded.
- Two positional `rows.Scan` sites break by **column count**, not by name, so
  post-migration they fail *silently* (HTTP 200 + empty array, log-only):
  `bin_check_recommendations.go` `GetBinCheckRecommendations` (21-arg) and
  `app_error_logs.go` `GetAppErrorLogs` (24-arg). Both need explicit column lists.
- `app_error_logs` is empty **not** because the feature is unused — the Flutter
  reporter posted to `localhost` until recently, so release builds reported
  crashes to the device's own loopback. Rows should start appearing.

## Sister repos

- **`ropacal-placement`** (Python) has its multi-tenancy work applied but is
  **not a git repository** — the changes exist only as files on disk. It needs
  `ROPACAL_ORG_ID` set in its environment, and `placement_model` needs DDL before
  a second org trains a model (composite PK `(organization_id, version)`,
  per-org active index) — documented in that repo's `sql/schema.sql`.
- **Backup**: the pre-migration dump (`pg_dump` **17** — the 15 client silently
  produces a 0-byte file against this 17.7 server) was taken to a session
  scratchpad. Re-take a durable one if it is still wanted.
