# Pre-client implementation plan

Everything that must be true before a SECOND organization onboards. Written
2026-07-30, after multi-tenancy went live with one org (`ropacal`).

Companion to `TENANCY_BACKLOG.md` (what's deferred and why) — this is the how.

---

## The dependency that shapes everything

**You cannot test any of this with one organization, and creating the second one
in production immediately hardens three gates.** Specifically, the moment
`organizations` has 2 rows:

- `CentrifugoProxyAuth` denies every proxy request (no secret configured) → all
  realtime dies
- `proxyOrgDB` denies (no connection meta forwarded) → same
- `Login` starts requiring the `organization` slug → every current client 400s

Verified live on 2026-07-29: provisioning a demo org produced exactly this, and
deleting it restored service within 30s.

**There is no staging.** Confirmed by the owner 2026-07-30: the `staging` Railway
environment exists in name only, nothing is deployed in it. Everything is
production. So every phase below is built and verified against the live system,
single-org, with no safety net — that constraint drives the whole order.

**The only viable order** is therefore:

1. Centrifugo proxy config (Phase A)
2. Login `organization` field on both clients (Phase B) — backwards-compatible
   at one org, so it can ship while still single-tenant
3. Provisioning (Phase C)
4. THEN create org #2, and build/verify Tier-1 (Phase D) against a live second
   tenant

A/B must both be complete before org #2 exists, because the moment it does,
realtime dies without the Centrifugo config and every client 400s without the
login field.

### The "stage a tenant quietly" trick does NOT work

Creating org #2 with `status != 'active'` looks like it would let you pre-stage a
tenant without tripping the gates. It does not, because the four gates disagree
about what counts as another tenant:

| Gate | Predicate |
|---|---|
| `middleware/centrifugo_proxy.go:80` | `count(*)` — ALL statuses |
| `database/tenancy.go:67` (boot tripwire) | `COUNT(*)` — ALL statuses |
| `handlers/centrifugo.go:144` (meta grace) | `WHERE status = 'active'` |
| `orgdb/state.go:112` (worker loop) | `WHERE status = 'active'` |

A suspended org #2 trips the two `count(*)` gates. Middleware runs before the
handler, so proxy requests 401 and realtime is down regardless of the grace still
holding downstream. It also risks the boot tripwire refusing to serve.

Each predicate is arguably right for its own purpose — a security gate should be
conservative and count every org whose data exists, while operational grace must
resolve only to a routable (active) org. But the COMBINATION creates a
half-hardened state nobody designed. Decide one rule deliberately and document
why, rather than leaving it as an accident.

Do NOT provision org #2 until Phases A and B are complete and verified.

---

## Phase A — Centrifugo proxy config

Detailed separately (see the Centrifugo section at the end); it is a
prerequisite for org #2 existing at all, not really "pre-client work". Summary:
set per-proxy static header + `include_connection_meta` + explicit timeout as
env vars on the Centrifugo service, then enable enforcement on the backend.

**Gate:** a forged proxy request without the header is rejected, real driver GPS
still flows, and the backend observes `meta.org_id` arriving on all three proxy
endpoints.

---

## Phase B — Login organization field

The backend contract already shipped and is live. This is client work only.

### Contract (already deployed, do not change)

```
POST /api/auth/login
{ "email": "...", "password": "...", "organization": "<org slug>" }
```

- `organization` is the org **slug**, case-insensitive.
- Omitted + exactly one active org → logs into it (grace; how things work today).
- Omitted + several orgs → **400**, body names the field.
- Unknown slug → **401**, byte-identical to a wrong password (no slug
  enumeration — do not "improve" this into a distinct error).
- Suspended/cancelled org → **403** naming the status.
- Success → response gains `organization: {id, name, slug}`; the JWT gains an
  `org_id` claim.

### B1. Dashboard (`binly-dashboard`)

- `lib/auth/queries.ts` — add `organization` to the `useLogin()` POST body.
- The login form — add the field. Persist the last-used slug in localStorage
  beside the existing auth storage so returning users don't retype it.
- Handle the new statuses: 400 → inline "organization required" on the field;
  403 → show the org-status message; 401 → existing invalid-credentials copy
  (must NOT distinguish bad slug from bad password).
- The 401 path must NOT trigger `apiFetch`'s global logout redirect — login is
  deliberately outside that wrapper already (`lib/auth/queries.ts` uses raw
  fetch). Keep it that way.

### B2. Flutter app (`ropacalapp`)

Four sites, already scoped by audit:
- `lib/core/services/api_service.dart:190` — add `organization` to the login body.
- `lib/providers/auth_provider.dart:206` — add the param; pass through at `:212`.
- `lib/features/auth/login_page.dart` — the field; persist as
  `remembered_organization` mirroring the existing `remembered_email` pattern
  (`:38`, `:59`).
- `lib/models/user.dart` — only if the org object is surfaced in the UI.

### B3. Ship order

Clients can ship BEFORE org #2 exists: sending `organization` while only one org
exists is harmless (it is validated, then resolves to that org). So B is safe to
deploy early and should be — it removes a hard cutover from the onboarding day.

**Gate:** log in on both clients with the slug supplied; log in with it omitted
and confirm it still works (grace); log in with a wrong slug and confirm the
error is indistinguishable from a wrong password.

---

## Phase C — Tenant provisioning

Currently manual SQL as `postgres`. Needs to be a repeatable, reviewable path.

### What provisioning must create

1. `organizations` row — `id` (uuid), `name`, `slug` (unique, lowercase), `status='active'`.
2. First admin `users` row — `organization_id`, `email`, bcrypt `password`, `role='admin'`.
3. That org's `warehouse_location` row in `config`.

### Constraints discovered the hard way (2026-07-29)

- **`config.id` is a serial INTEGER, not a UUID.** Omit it on insert; passing a
  uuid errors.
- **Boot seeds are skipped once tenancy is live** (`cmd/server/main.go`) — a new
  org gets NOTHING automatically. This is deliberate: seeding under RLS with no
  org would NOT-NULL violate and boot-loop.
- **`organizations` INSERT is tenant-scoped by RLS.** The `org_catalog_read`
  policy is SELECT-only, so provisioning must run as `postgres`, or as a role
  with an INSERT policy. Do not loosen `binly_app`.
- **A new object created by `postgres` is owned by `postgres`** and therefore
  invisible to `binly_app` (this exact bug bit the `organizations` table itself).
  Any provisioning that creates objects must `ALTER ... OWNER TO binly_app`.
- **`users.email` is unique per `(organization_id, email)`**, so the same person
  can exist in two orgs — but login resolves org first, so that is fine.

### Implementation options (pick one)

1. **A `psql` script in `migrations/` or `scripts/`**, parameterised, run as
   `postgres`. Simplest, no new attack surface, no code. Weakness: manual, and
   the bcrypt hash must be generated separately.
2. **A CLI subcommand** on the Go binary (`ropacal-backend provision-org ...`)
   run as a Railway one-off. Generates the bcrypt hash itself, can validate the
   slug, and can be tested. Needs a `postgres`-privileged connection string
   passed explicitly (NOT the app's `DATABASE_URL`).
3. **A platform-admin HTTP endpoint.** Most convenient, worst security posture —
   it needs a cross-tenant-privileged path in the running app, which is exactly
   what the whole design avoids. Not recommended now.

**Recommendation: option 2.** It keeps privileges out of the serving process
(the command is not a route), makes the bcrypt step correct by construction, and
is testable. Option 1 is an acceptable interim.

**Gate:** provision a throwaway org end to end; its admin logs in, sees an empty
fleet, and cannot see the other org's bins/shifts/config. Then delete it and
confirm the original org is untouched.

---

## Phase D — Tier-1 realtime

The isolation gaps that only matter with two tenants. From the researched plan
(`scratchpad/tenancy_realtime_plan.md`, findings verified against code).

### D1. Per-org event channel

`company:events` is ONE channel carrying the whole operational feed — bins with
addresses, moves, shifts, daily reports, AI recommendations — across **36 publish
sites** (`internal/services/centrifugo/client.go`). Any admin of any org can
subscribe. Becomes `company:{orgID}:events`.

Touches: the publisher signature (every caller must supply an org),
`CentrifugoSubscribeProxy`'s channel parsing, the dashboard subscriber (4 sites),
and the Flutter manager mode.

Keeping the `company:` prefix as the namespace means Centrifugo's namespace
config keeps working unchanged.

### D2. Org-equality on subscribe authorization

`canViewDriverLocation` (`internal/handlers/centrifugo.go`) returns true for ANY
admin — so given a driver UUID, one tenant could live-track another's fleet.
Now that the proxies carry org context (Tier-0), add an explicit predicate: the
channel's driver must belong to the subscriber's org. RLS gives this almost for
free once the lookup runs on the org-bound handle; make the check explicit
anyway rather than relying on a zero-row result.

### D3. Redis key namespacing

Driver GPS keys become `ropacal:org:{orgID}:driver:{id}:location`. The 10-minute
TTL means no migration is needed — old keys simply expire. Touches
`internal/services/redis/client.go`, the batch writer, and
`manager_active_drivers.go` (which reads ALL driver locations).

### D4. Shorter connection-token TTL

Currently long enough that a stale token survives meaningful changes. Shorten it
so revocation (e.g. a user removed from an org) takes effect in minutes.

### Order within D

D2 first (smallest, highest security value, no client changes). Then D3 (server
only). Then D1 last — it is the only one needing a coordinated mobile release,
and it is the biggest diff.

**Gate:** with two orgs live, org B's admin cannot subscribe to org A's
`company:` channel or to any of A's `driver:location:` channels, and A's GPS
never appears in B's Redis scope.

---

## Phase E — Onboard

Provision the real org, hand over credentials, watch the first shift.

---

## Cross-cutting checks before declaring done

- Six background workers still loop per org and no notification crosses orgs
  (five of them select recipients with `WHERE role='admin'` — org-blind by
  themselves; RLS is what scopes them).
- The FindMy bridge still resolves the right org for airtag writes (its resolver
  probes each active org and fails closed on ambiguity — with two orgs that path
  finally exercises for real).
- `/api/analytics/areas` and other aggregate endpoints return per-org numbers.
- The AI chat's session cache stays org-partitioned (keys are org-prefixed).
- Pool sizing still adequate: per-statement transactions × 6 workers × 2 orgs.

---

## Centrifugo config detail (Phase A)

Corrected after a documentation review found the first draft used v5/invalid
paths. **Centrifugo is v6.6.0 OSS.**

- Subscribe/publish proxies live under **`channel.proxy.*`**, NOT
  `client.proxy.*` (which holds only `connect`/`refresh` in v6).
- Env vars CAN augment proxies defined in a config file, field by field — an
  unset var leaves the file value intact.
- Two shapes exist and a deployment can be **mixed**: unified
  (`CENTRIFUGO_CHANNEL_PROXY_<TYPE>_...`) and granular named
  (`CENTRIFUGO_PROXIES_<NAME>_...`). The discriminator is the `proxy_name` field
  in the `* proxy enabled *` startup log lines: `default` → unified, anything
  else → granular with that name as the env infix.
- **`http.static_headers` is REPLACED wholesale, not merged** — if the baked
  config already sets headers on a proxy, an env var wipes them.
- **Never set bare `CENTRIFUGO_PROXIES`** — it replaces the entire array,
  destroying the baked proxy definitions.
- Default proxy timeout is **1s**, and this backend has exceeded it before
  (historical `context deadline exceeded` on publish-location). Set it explicitly.
- Unknown-key warnings do NOT catch "correct spelling, wrong shape" — a
  `CHANNEL_PROXY` var is always a known key. **Verification must be observing the
  header arrive at the backend**, not a clean log.

### Blockers to resolve first

1. **Startup logs are unavailable** — the service has ~162 days uptime and
   Railway's retention doesn't reach its boot lines. Determining the proxy shape
   requires either a deliberate restart (brief GPS gap) or reading the config
   source.
2. **Two existing env vars are silently ignored by v6** and this needs its own
   fix (see below).

### Independent finding: two ignored env vars

| Var | v6 status |
|---|---|
| `CENTRIFUGO_API_KEY` | **ignored** — v6 reads `http_api.key` → `CENTRIFUGO_HTTP_API_KEY` |
| `CENTRIFUGO_TOKEN_HMAC_SECRET_KEY` | **ignored** — v6 reads `client.token.hmac_secret_key` → `CENTRIFUGO_CLIENT_TOKEN_HMAC_SECRET_KEY` |

Consequences:
- Explains the "stale API key" — Centrifugo never read that var; the live key is
  in config.json.
- **The HMAC secret actually verifying driver connection tokens lives in
  config.json.** Rotating it via the env var would change nothing, and rotating
  the BACKEND's signing secret to match a new env value would break every
  connection. This is a live footgun regardless of tenancy.

### Verified good

The backend already mints connection tokens carrying
`meta: {"org_id": "..."}` — confirmed by decoding a live token 2026-07-30. So
`include_connection_meta` is the only missing link on the Centrifugo side.
