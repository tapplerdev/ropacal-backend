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

### 2. Tenant provisioning flow — SCRIPTS NOW EXIST

`scripts/provision-org.sh` and `scripts/teardown-org.sh` (added 2026-07-30).
Provisioning runs all three inserts in ONE transaction; teardown deletes the 35
tenant tables in generated dependency order, then the org.

**Regenerating the teardown order** if the schema changes — the order must be a
topological sort over BLOCKING FK edges only (`confdeltype IN ('a','r')`).
Including the 40 SET NULL and 27 CASCADE edges produces a FALSE cycle among
bins / potential_locations / shifts / users:

```sql
SELECT c.conrelid::regclass::text AS child, c.confrelid::regclass::text AS parent
FROM pg_constraint c
WHERE c.contype='f' AND c.connamespace='public'::regnamespace
  AND c.confdeltype IN ('a','r') AND c.conrelid <> c.confrelid;
```
Then topologically sort the 35 tables that have an `organization_id` column,
emitting a table only once nothing still-pending references it as a parent.

Constraints that shaped the scripts:
- all 35 FKs to `organizations` are RESTRICT and `condeferrable = false`, so
  `SET CONSTRAINTS ALL DEFERRED` is not available
- `config.id` is a SERIAL INTEGER, not a uuid — omit it on insert
- provisioning needs NO new credential: `binly_app` already holds INSERT on
  `organizations`, and `set_config('app.org_id', …)` satisfies the RLS
  `WITH CHECK` plus the `organization_id` column defaults
- a new org's warehouse is seeded at 0,0 and route optimization returns 412
  until it is set for real

### 2b. Original notes
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

## Tier 1 realtime — status 2026-07-30

Four of the five items are **SHIPPED AND GATED**. D1 is one deploy of three in.

| Item | State | Gate result |
|---|---|---|
| **D2** org-equality in subscribe auth | **DONE** | A made-up driver/shift UUID was ALLOWED before the deploy and DENIED after; self-subscribe still works |
| **D3** Redis keys → `ropacal:org:{orgID}:driver:{id}:location`, `KEYS`→`SCAN` | **DONE** | Key landed under the org prefix; another org's prefix and the flat form both empty; Centrifugo's 153 keys untouched; batch writer advanced `driver_current_location` in 12s |
| **D4b** cached per-request membership + role revalidation | **DONE** | A deleted user's still-valid token (exp 6 days out) 401'd at exactly 60s; 90s of continuous requests across the cache boundary all 200 |
| **D4a** connection token TTL 24h → 1h | **DONE** | Freshly issued token has `exp - iat == 3600` |
| **D1** `company:events` → `company:{orgID}:events` | **Deploy 1 of 3** | dual-publish + two-form parser live; see below |

### D1 — what remains

Deploy 1 (backend: publisher dual-writes both channels, parser accepts both
forms) is live and fully backward compatible. **It closes nothing on its own.**

Deploy 1 gate results (2026-07-30):

| Probe | Result |
|---|---|
| admin → `company:{own org}:events` | ALLOW |
| admin → `company:events` (legacy) | ALLOW — backward compatible |
| admin → `company:{other org}:events` | DENY |
| driver → `company:{own org}:events` | DENY |
| admin → `company:events:{org}` (wrong order) | DENY, `unknown channel format` — loud, as designed |
| D2 regression (real vs made-up driver) | ALLOW / DENY, intact |
| Centrifugo accepts `company:{uuid}:events` | yes (namespace resolves at the first colon) |
| `history` on the company namespace | `not available` — confirms the namespace retains nothing |

**Not yet observed with real traffic:** that the dual publish reaches the scoped
channel in production. A failure there is non-breaking and self-announcing — the
legacy publish is unchanged and still delivers, and the scoped failure logs
`⚠️ [Centrifugo] scoped publish failed`. Any admin action or the periodic
monitors will exercise it; check for that line before Deploy 2.

**Gate-harness note:** the first run of this gate reported the own-org scoped
channel as DENIED, which looked like a real bug. It was the test harness — in
zsh, `$ORG:events` applies `:e` as a history modifier, so the string sent was
`company:vents`. Brace org ids in gate scripts (`${ORG}`). The
`unknown channel format` error now names the parsed shape, which is what
identified this in one request.

- **Deploy 2** — both clients subscribe to `company:{orgID}:events`. Needs the
  client org state in §1 of `TIER1_PLAN.md`; the four dashboard sites switch to
  the context's `companyChannel`, and Flutter passes `orgId` through. Flutter
  needs a store release, so start that clock early.
- **Deploy 3a** — drop the legacy PARSER branch. **This is the deploy that
  actually closes the cross-tenant hole.** Before it, confirm the Railway logs
  show zero `channel=company:events` subscribes over a full business day. That
  gate has a known blind spot: the subscribe proxy returns no `ExpireAt`, so a
  subscription is re-authorized only on (re)subscribe and long-lived sessions are
  invisible to the count. 3a converts them into loud 403s, which is why it
  precedes 3b.
- **Deploy 3b** — drop the legacy PUBLISH, only after 3a has been quiet for a
  business day. Removing publish is the only step that can silence anyone.

**Still open before Deploy 2: pin the Centrifugo Docker tag.** The image uses a
floating tag and self-upgraded 6.6.0 → 6.9.1 unannounced during the 2026-07-30
secret rotation. An unannounced version move mid-migration could shift
namespace-resolution behaviour underneath the one assumption D1 rests on. The
Railway CLI does not expose the image source, so this is a dashboard change:
`binly-centrifugo-service` → the Centrifugo service → Settings → Source → Image,
pin to the exact running version. Note it restarts the broker and drops live
WebSocket connections, so pick the window.

## Tier 2 — later

- Delete the `websocket` package and its ~50 `BroadcastToRole` call sites. The
  `/ws` route was closed 2026-07-29 (both clients are Centrifugo-only;
  `ApiConstants.wsUrl` in Flutter has zero consumers). Revert = re-register one
  line in `main.go`.
- `airtag_locations` matches on `bin_number`, which is **not unique** (prod has
  duplicates 56, 86, 116). Cross-tenant id collisions currently surface as the
  FindMy resolver's ambiguity skip — safe and loud, but it should be namespaced.
- Per-org rate limits. (Redis `KEYS` → `SCAN` is DONE — folded into D3 on
  2026-07-30, because the pattern ran against the same Redis instance
  Centrifugo's engine uses, blocking it once per active org every 30 seconds.)
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
