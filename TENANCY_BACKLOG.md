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

### 1. Centrifugo proxy config — HALF DONE

**`CENTRIFUGO_PROXY_SECRET` is SET and enforced** (done 2026-07-30; verified by
45 × `publish-location` → 200 under enforcement, and every gate in this file had
to send the header). That half is closed.

**`include_connection_meta` IS configured** — resolved 2026-07-30 by locating the
service source, which nothing had recorded:

> The Centrifugo service deploys from the GitHub repo
> **`tapplerdev/binly-centrifugo-service`** (found via the Railway GraphQL API;
> `railway status` does not expose it). Its `config.json` defines the proxies and
> is baked into the image by the Dockerfile. There are NO proxy env vars on the
> Railway service — only `CENTRIFUGO_VAR_PROXY_SECRET`, which the config
> interpolates as `${CENTRIFUGO_VAR_PROXY_SECRET}` into each proxy's
> `http.static_headers`.

`include_connection_meta: true` is set on all three proxies:
`proxies[location_publish]`, `channel.proxy.subscribe`, `channel.proxy.publish`.

The runtime probe added the same day (`proxyOrgDB`'s single-org grace logs
`SINGLE-ORG GRACE ... not connection meta`, rate-limited to once a minute) stays
in place as the confirmation that config matches reality. Reading it:

- line appears while REAL clients are connected → meta is NOT arriving despite
  the config → investigate before onboarding
- line absent while clients are connected → confirmed working

**RESOLVED 2026-07-30 — meta IS forwarded. Verified end to end.**

Tested by connecting as a REAL Centrifugo client (the same `centrifuge` SDK the
dashboard uses, driven from node against the production broker) with a genuine
connection token, then subscribing to `company:{orgID}:events`:

    connected: client=32859973-...
    SUBSCRIBED to company:00000000-0000-0000-0000-000000000001:events
    backend: Subscribe request ... channel=company:00000000-...:events
    backend: Subscription authorized
    SINGLE-ORG GRACE lines: 0

Zero grace lines against a real subscribe means `proxyOrgDB` resolved the tenant
from `meta`, not from the one-org fallback. **Realtime will survive a second
organization.** This was the last blocking unknown on this item.

That distinction is invisible from the backend alone while one org exists, which
is why it needed a real client rather than a curl. The probe stays in place as a
regression detector: if the line ever appears while real clients are connected,
meta has stopped arriving.

Original note on both fields:

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

### 3. Login `organization` field — DONE (2026-07-30)

Shipped on both clients, plus a backend change they needed.

- **Dashboard** (`binly-dashboard` f7984bd, fixes in 332fff0): optional field,
  `?org=` support, slug remembered independently of Remember Me, 400/403 now
  surface the backend's real reason instead of "Invalid email or password".
- **Flutter** (`ropacalapp` 9a40057, fixes in 88ad90b): same, plus session-restore
  backfill. **Needs a store release to reach drivers.**
- **Backend** (24a8d67): `GET /api/auth/status` now returns the organization, so
  a client that is already signed in learns its slug without re-authenticating.
  Without this, any user holding a valid 7-day token when org #2 is provisioned
  would hit a newly-mandatory field they had never been given a value for.

Both were reviewed by an agent; every finding is fixed. Two are worth keeping in
mind because they are the same bug in two codebases: **the remembered slug must
be cleared on logout** (otherwise the next person on a shared device inherits it
and gets an opaque 401 with correct credentials), and **only the SERVER-RESOLVED
slug may be persisted**, never raw input.

Still open on this item: the Flutter label reads "Organization (optional)", which
becomes untrue at org #2 and needs a release to change. Change it in the same
release that goes out around provisioning.

Original contract notes:
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

## Findings from the 2026-07-30 config + migration review

The review confirmed D1's load-bearing assumption and found six real issues.
Four are fixed; two live in the FindMy bridge and are open.

**Confirmed good — do not re-litigate:** Centrifugo resolves a channel's
namespace at the FIRST colon, so `company:{uuid}:events` lands in the `company`
namespace. Per the docs, and already demonstrated by the two-colon
`driver:location:{id}` channel running in production today. D1 is sound.
`include_connection_meta` is the correct v6 key at all three config paths, adds a
top-level `meta` field, and matches the backend's struct tags. No Go code selects
or updates `airtag_accounts` by email, so the per-org migration could not have
turned a single-row lookup into a multi-row one.

**Fixed 2026-07-30:**
- Boot DDL declared `email TEXT NOT NULL UNIQUE` on `airtag_accounts` — the exact
  constraint the migration removes. No migrations runner exists, so any FRESH
  database silently reintroduced the cross-tenant existence oracle. Now an
  idempotent DO block that installs the right constraint for either schema state.
- `CentrifugoSubscribeResult.ExpireAt` was declared and never set, making every
  subscription permanent: D4b's 60s revocation never reached an established
  socket, and Deploy 3a's "zero legacy subscribes" gate could not see the very
  sessions it would cut off. Now 1h, matching the D4a token TTL.
- Centrifugo Dockerfile now warns against pinning below **v6.3.0** — the
  `${CENTRIFUGO_VAR_*}` interpolation does not exist before it, so an older pin
  sends the literal string as the proxy secret and 401s every proxy request,
  presenting as a secret mismatch rather than a version problem.
- `binly-findmy-bridge` `.gitignore` now covers `account_*.json`. Those hold the
  iCloud password in plaintext plus ~10 live FindMy tokens; only
  `account_state.json` was ignored, so a `git add -A` would have committed them.
  Verified never committed historically, so no history rewrite is needed.

**OPEN — in `binly-findmy-bridge`, and blocking for org #2:**
1. **Positional account misalignment (a LIVE bug, not just a tenancy one).**
   `sync_engine.py:172` does `zip(self.accounts, self.config.APPLE_ACCOUNTS)`, but
   `get_accounts()` SKIPS accounts that fail login or need 2FA
   (`location_fetcher.py:98`, `:103`), so the lists shift and one account's Apple
   session is PUT to another account's row. Same assumption at
   `sync_engine.py:201-202`, `:319`, `api.py:143`, `:182`. At two orgs this
   carries one tenant's live Apple auth tokens into another tenant's row.
   `state_file = f"account_{i}.json"` has the same problem, and
   `_restore_account_state_from_db` only writes the backend's copy when the local
   file is ABSENT, so a stale file beats the authoritative one.
2. **`_pending_2fa` is keyed by email** (`location_fetcher.py:21`, `:102`;
   `api.py:63-73`, `:86-88`) — precisely the collision the migration now permits.
   Two orgs sharing an Apple ID means the second overwrites the first's pending
   entry and that org can never complete 2FA. `account_id` already exists on
   `AccountCredentials` and is the obvious key.
3. The bridge is org-blind generally: it merges every account's keys into one
   list, so org A's credentials query org B's tags, and it posts locations keyed
   on `bin_number`, which is NOT unique across tenants. The backend fails closed
   (multi-org matches are skipped), so this is silent tracking LOSS rather than
   misrouting — but two orgs owning "Bin 46" breaks it for both.

**Assessed and deliberately not changed:**
- `client.allowed_origins: ["*"]` is largely mitigated, not a live hole. It only
  constrains browsers (requests without an `Origin` header pass regardless, and
  non-browsers can forge it), anonymous connect is disabled, the connection token
  is minted behind `middleware.Auth` on another origin with no cookie auth in the
  handshake, and every subscribe is proxy-authorized per user AND per org.
  Tightening it to the dashboard origins is free and will not break Flutter
  (native clients send no `Origin`), but it is defense-in-depth, not a fix.
- `channel.proxy.publish` is dead config — no namespace enables it except
  `driver`, which overrides it with `publish_proxy_name`. Fail-closed, but
  config.json reads as though it guards shift/manager/company publishes when
  default-deny is what actually does.
- `driver`'s `allow_publish_for_client: true` is safe only because
  `publish_proxy_enabled` routes to the location proxy, which enforces channel
  shape and `req.User == driverID`. Dropping or mistyping that flag would let any
  authenticated client publish into any `driver:*` channel.

## Schema management — FIXED 2026-07-30 (goose + baseline)

**The problem below is resolved.** `internal/database/migrations/00001_baseline_schema.sql`
is a `pg_dump` of production's schema, run by goose (`internal/database/goose.go`,
embedded via `go:embed`, executed at boot before the legacy DDL).

- **Fresh database** → goose builds the entire schema. Verified: an empty
  database boots to `/health` 200 with 42 tables and all 37 RLS policies,
  identical to production, including the four tables that were missing.
- **Existing database** (production) → detected and **STAMPED** at version 1
  without executing, because running it would fail on the first CREATE. Verified
  against a full clone of production's schema: 42 tables → 43 (only
  `goose_db_version` added), boot clean.
- New schema changes go in a numbered goose migration, NOT the legacy DDL list.
- The 22 previously hand-applied files are kept as history in
  `internal/database/migrations/archive/` and are deliberately NOT embedded —
  their effects are already in the baseline.
- Still not carried by the baseline: the **`binly_app` role** (roles are
  cluster-level, not per-database), so a brand-new environment must create it and
  grant table ownership.

Two pg_dump quirks had to be handled for goose: the `\restrict`/`\unrestrict`
psql meta-commands it emits (goose executes SQL directly and cannot parse them),
and `set_config('search_path','',false)` — an empty search_path stops goose
finding its own version table after the migration runs.

### Original finding (kept for context)

## The backend cannot boot against a FRESH database (found 2026-07-30)

Discovered while building a local test environment. Pointing the backend at an
empty database fails at startup:

    ❌ FATAL ERROR: Database migrations failed
       migration failed: pq: relation "move_request_history" does not exist

**Independently verified 2026-07-30, and it is WORSE than the first diagnosis.**
`move_request_history` is 1 of **4** blockers and it is the easy one — fixing it
just moves the error three more times. All 223 boot statements were extracted and
run individually against an empty database; **24 fail across 4 tables**, in two
distinct classes:

| Table | Failing stmts | Class |
|---|---|---|
| **`route_tasks`** | **20** | **No `CREATE TABLE` exists ANYWHERE in the repo** — not in Go, not in `migrations/*.sql`. Only ALTERs and indexes. |
| `move_request_history` | 2 | Not created in the boot path; DDL exists only in `migrations/add_move_request_history.sql` |
| `bin_move_requests` | 1 | Ordering only — the table IS created later in the same slice (line 602) |
| `potential_locations` | 1 | Ordering only — created later in the same slice (line 660) |

Proof of the split: after pre-creating the two genuinely-missing tables, a second
pass over the identical 223 statements returned **zero** failures.

`route_tasks` is the serious one — the central table of the itinerary domain,
~27 query sites, and no canonical schema anywhere. `internal/itinerary/DESIGN.md`
line 76 lists exactly this as Phase 0 work that was never shipped: "add
idempotent `CREATE TABLE route_tasks` DDL (canonical schema; currently only
ALTERs exist)". Fixing it means AUTHORING the schema from `models.RouteTask` plus
the existing ALTERs, not copying a file.

Second trap: the naive fix for `move_request_history` also fails. Its
`CREATE TABLE` in `migrations/add_move_request_history.sql` carries inline
`REFERENCES bin_move_requests(id)` and `REFERENCES users(id)`, so pasting it at
the top of the slice fails on a missing `bin_move_requests`. It has to go AFTER
line 629.

The failure is fatal, not skipped — `database.go:1175-1179` returns on the first
error and `cmd/server/main.go:89-95` wraps it in `log.Fatal`.

Beyond the boot blockers: **`routes` and `route_bins` are never created either**
(only in `migrations/create_routes_table.sql`). They do not stop boot, but
`internal/handlers/routes.go` (12+ query sites) and
`ai_operations_agent.go:445` would 500 at runtime — so even a booting fresh
environment is not functional.

**Minimal correct fix, in order:**
1. Author `CREATE TABLE IF NOT EXISTS route_tasks (...)` from `models.RouteTask`;
   insert after `shifts` (line 187) so its `shift_id`/`bin_id` FKs resolve. The
   existing ALTERs then become no-ops.
2. Add `CREATE TABLE IF NOT EXISTS move_request_history (...)` after the
   `bin_move_requests` block (after line 629), and relocate lines 238-239 (the
   `seq` column and `idx_mrh_move_seq`) to directly follow it.
3. Move the `zone_incidents_move_request_id_fkey` DO block (line 460) to after
   line 629.
4. Move the `fk_bins_source_potential_location` DO block (line 585) to after
   line 683.
5. Add `routes` / `route_bins` DDL so the environment is usable, not just
   bootable.

Tenancy detection and seeding are innocent — verified: with the schema fixed the
real binary boots fully, detects single-tenant passthrough correctly, seeds 4
users and 44 bins, starts every worker and serves 200 on `/health`.

This compounds the airtag boot-DDL finding (fixed the same day): the
fresh-database path is broken in more than one way, and it is untested because
production has had these tables for months and
`CREATE TABLE IF NOT EXISTS` / `ADD COLUMN IF NOT EXISTS` make every boot a
no-op there.

Worth fixing properly rather than patching: either add the missing CREATE TABLE
statements to the boot DDL, or add a real migration runner and stop keeping two
sources of schema truth. Until then, the reliable way to stand up a new
environment is to restore a schema dump from production
(`pg_dump --schema-only`), which is what the local test used.

**Verified working via that route (2026-07-30), all against a local clone of the
production schema with a non-superuser `binly_app` owning 41 tables under FORCE
ROW LEVEL SECURITY — production was never touched:**

| Test | Result |
|---|---|
| `POST /api/internal/organizations` with no / wrong internal key | 401 |
| Validation: missing name, bad slug, missing email, half-supplied or out-of-range coords, coords without address | all rejected with specific messages |
| Create org with warehouse | 201, password returned once |
| Create org without warehouse | 201 + warning about the 412 |
| Duplicate slug | 409 |
| **2 orgs → login without slug** | **400 with the "organization is required" message** |
| 2 orgs → login with slug | 200, returns the organization |
| Slug case-insensitivity (`ACME`) | 200 |
| Right password + WRONG org slug | opaque 401 |
| Unknown slug | opaque 401 (indistinguishable — no tenant enumeration) |
| Both orgs create bin **9001** | both succeed; numbers are per-tenant |
| Each org lists bins | sees ONLY its own |
| acme PATCHes its own bin (control) | 200, change applied |
| **acme PATCHes beta's bin by exact UUID** | **404, beta's data unchanged** |
| `teardown-org.sh beta` | removed cleanly, acme untouched, 0 orphaned rows |
| Back to 1 org | single-org grace returns within ~12s |

The control on the PATCH test matters: a 404 only proves isolation if the same
call against your OWN row returns 200. It did.

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
