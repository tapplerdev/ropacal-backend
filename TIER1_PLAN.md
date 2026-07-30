# Tier-1 realtime tenant isolation — implementation plan

Written 2026-07-29. Every line reference below was read and verified against the
working tree on that date. Companions: `TENANCY_BACKLOG.md` (what is deferred and
why), `PRE_CLIENT_PLAN.md` (the Phase A–E order this slots into as Phase D).

Scope: the four items that close realtime/Redis tenant isolation.

| | Item | Ships without a second org? | Client release needed? |
|---|---|---|---|
| **D1** | `company:events` → `company:{orgID}:events` | build + verify yes, *complete* no | **yes, both** |
| **D2** | org-equality in subscribe authorization | **yes, fully** | no |
| **D3** | Redis GPS keys → `ropacal:org:{orgID}:driver:{id}:location` | **yes, fully** | no |
| **D4** | close the revocation window | **yes, fully** | no |

Recommended order: **D2 → D3 → D4 → D1**. Reasons in "Cross-item ordering".

---

## 0. Corrections to the brief

Findings from the prior reviews were re-checked. Most held. These did not.

### 0.1 The Centrifugo engine Redis is NOT a separate instance — it is the SAME Redis the backend uses

The brief states Centrifugo has "its OWN Redis (separate instance, in the
Centrifugo Railway project)". It does not. Verified by connecting read-only to
the backend's own `REDIS_URL` (`metro.proxy.rlwy.net:41024`, from the cached
Railway prod vars) and scanning:

```
DBSIZE = 153
142 × centrifugo.stream.meta.shift:updates:{UUID}
  8 × centrifugo.stream.meta.driver:location:{UUID}
  3 × centrifugo.stream.meta.driver:events:{UUID}
```

Centrifugo's engine keys live in the same keyspace the backend writes
`ropacal:*` into. Consequences that change the plan:

- **`KEYS ropacal:driver:*:location` (`internal/services/redis/client.go:70`)
  is a blocking full-keyspace scan on the Redis instance Centrifugo depends on
  for pub/sub and history.** Today it scans 153 keys, 142 of them Centrifugo's,
  once per 30s **per organization** (`location_batch_writer.go:68` loops
  `ForEachActiveOrg` and each pass calls `GetAllDriverLocations` at `:82`).
  Two orgs doubles it. This is shared-fate coupling between a background worker
  and the realtime broker, and D3 is the natural place to fix it (see D3.4).
- The `ropacal:` prefix is what keeps the two key families apart, so it must
  survive the rename. `ropacal:org:{orgID}:driver:{id}:location` does.

### 0.2 The "company namespace retains nothing" claim is right — and now empirically confirmed

`centrepo/config.json` (byte-identical to the cached `cf_config.json`; repo
`tapplerdev/binly-centrifugo-service`, HEAD `8a4389f`) gives the `company`
namespace `presence: false`, `force_recovery: false`, and **no** `history_size`
/ `history_ttl`. Independently confirmed against the live Redis: a scan for
`*company*` returns **zero keys**. Renaming the company channel cannot leak
retained messages. Verified safe.

Also confirmed from the same config, matching the brief: `driver` =
presence true / history 10 / ttl 300s / `force_recovery: true` +
`force_recovery_mode: "cache"`; `shift` = 5 / 120s; `manager` = 10 / 300s. All
four namespaces have `subscribe_proxy_enabled: true`; nothing sets
`static_http_headers` or `include_connection_meta` anywhere (Phase A is
genuinely not done).

One addition the brief missed: `driver`'s `force_recovery_mode: "cache"` means a
new subscriber is handed the **last** published fix immediately on subscribe. So
today an admin of any org who subscribes to a foreign driver's channel gets that
driver's current position instantly, not just future fixes. D2 closes that
vector too, because the subscribe never completes.

### 0.3 D1's stated deploy order still has a silent-dead window; it needs dual-publish

The brief's order is "parser accepts both → clients → flip publisher". Walk it:

| Deploy | Parser | Clients | Publisher | Result |
|---|---|---|---|---|
| 1 | both | old | old | fine |
| 2 | both | **new** | old | clients subscribe successfully to a channel **nothing publishes to** — **silently dead**, exactly the failure the brief set out to avoid |
| 3 | both | new | new | fine |

Publisher-last is right about which direction is dangerous, but the safe shape
is **dual-publish in Deploy 1**: the publisher writes the event to *both*
`company:events` and `company:{orgID}:events`. Then every deploy is
independently safe and independently revertible, and there is no window in which
a subscribed client receives nothing. Cost: one extra Centrifugo publish per
event during the transition — free, since `company` has no history.

**Dual-publish error contract — MANDATORY, not an implementation detail.** All 36
call sites treat a publish error as log-only (`handlers/bins.go:235-236,950-951`;
`handlers/shifts.go:492-494,581,656,742,983`;
`handlers/shift_complete.go:1064-1066,1091`;
`services/digest_scheduler.go:392,566,768` use bare `_ =`). So if the
implementation publishes the SCOPED channel first and returns on its error before
reaching the legacy publish, then any rejection of `company:{orgID}:events`
silently stops **every company event reaching the live flat channel** — one
warning line per event, nothing client-visible. That is precisely the failure
dual-publish exists to prevent, reintroduced by ordering.

Required inside `PublishCompanyEvent`:
1. Publish **`company:events` FIRST** (the channel clients are actually on).
2. Publish `company:{orgID}:events` second.
3. Handle the two errors **independently** — a scoped-publish failure must never
   short-circuit or mask the legacy publish.
4. Log a **distinct** message on scoped failure (e.g. `⚠️ [Centrifugo] scoped
   publish failed, legacy delivered`) so Deploy 1 can be verified as working
   rather than merely quiet.

### 0.4 D1's prerequisite is *client org state*, not the login `organization` field work

The brief says "the login `organization` field work (which adds org to the login
response and JWT) must land first". The backend half of that **already shipped**:

- `internal/handlers/auth.go:36-40` defines `LoginOrganization{ID,Name,Slug}`
  and `:46` returns it as `LoginResponse.Organization`.
- `internal/handlers/auth.go:191-194` mints `claims["org_id"] = org.ID` into the
  app JWT.

What is missing is purely client-side capture. Verified: `grep -rn
"organization|orgId|org_id"` over `binly-dashboard` (excluding `node_modules`)
returns **zero hits**; the same grep over `ropacalapp/lib` returns **zero hits**.
`binly-dashboard/lib/auth/types.ts:5-11` (`User`) and `store.ts:5-14`
(`AuthState`) have no org field; `ropacalapp/lib/models/user.dart:9-14` has none.
`GET /api/auth/status` does not return one either (`auth.go` `GetAuthStatus`
returns only `user.ToUserResponse()`).

So D1 is blocked on client org state — but **not** on Phase B's login-form work,
because both clients already decode the app JWT locally and the JWT already
carries `org_id`:

- Dashboard precedent: `components/binly/create-potential-location-dialog.tsx:666`
  — `JSON.parse(atob(parts[1]))`.
- Flutter precedent: `lib/providers/auth_provider.dart:133-147` — `_jwtExpired()`
  does `jsonDecode(utf8.decode(base64Url.decode(base64Url.normalize(parts[1]))))`
  and reads `payload['exp']`. Reading `payload['org_id']` is a three-line
  addition to a helper that already exists and is already exercised on every
  cold start.

This matters, and it also exposes a **silent-failure risk the brief did not
mention**: both clients restore sessions from persisted state.

- Dashboard: `lib/auth/store.ts:20-46` is `zustand/persist` under
  `binly-auth-storage`. A returning user has `token` in localStorage but would
  have **no** `organization` after an upgrade that only populates it at login.
- Flutter: `auth_provider.dart:70-85` is an explicit **optimistic cold start** —
  a cached `User` snapshot (`_loadCachedUser()`, `:149`) plus a locally-valid
  JWT returns immediately without hitting the backend. A cached user written by
  the old build has no org field.

If D1's client change reads org **only** from the login response, every already
logged-in session builds `company:undefined:events` / `company:null:events`,
which the subscribe proxy denies — a dead dashboard and a dead manager feed for
every user who does not happen to re-login. **The client change must derive org
from the JWT (which every live session already has, since
`middleware/org.go:50-56` 401s any token without `org_id`), with the login
response as the authoritative source when present.** JWT-derived, not
login-derived, is the correct primary.

### 0.5 Counts and line numbers

- `PublishCompanyEvent` call sites: **36, in 16 files** (38 grep hits minus the
  doc comment at `internal/services/centrifugo/client.go:188` and the definition
  at `:190`). The brief says 37 in 17 files; 17 is right only if you count the
  defining file.
- Dashboard `driver:location` sites are **two**, not one at `:103`:
  `lib/hooks/use-active-drivers.ts:100` (subscribe) and `:113` (the
  unsubscribe-path channel string). The brief's `:103` is neither.
- The brief's Redis call-site line numbers are from the older research pass.
  Current, verified: `manager_active_drivers.go:80` (not 78),
  `driver_location.go:682` (not 676), `location_batch_writer.go:82` (not 64).
- `DeleteDriverLocation` (`redis/client.go:98`) has **zero callers** outside its
  own file. It still needs the signature change to compile, but no call-site work.
- `role == "manager"` in `centrifugo.go:331/356/386` is **dead**:
  `users_role_check` on the live DB is `CHECK (role = ANY (ARRAY['driver','admin']))`,
  so no row can hold `manager`. Not a bug, but do not add new logic that depends on it.

Everything else in the brief checked out, specifically: `client.go:191`
(`channel := "company:events"`), `centrifugo.go:322` (`if channelType == "events"`,
with **no** `len(parts)` guard — so it matches the 2-part form), `centrifugo.go:339-357`
(`canViewDriverLocation`, `return role == "admin" || role == "manager"` at `:356`),
`centrifugo.go:526` (24h Centrifugo token), `auth.go:191` (7-day app JWT),
`middleware/org.go:50-56`, `redis/client.go:88` (the 4-segment assert),
`centrifugo_provider.tsx:18` (`subscribe: (channel, handler)`), the four dashboard
`company:events` sites, `centrifugo_service.dart:347/183/244/293/404`, and
`manager_map_page.dart:324`.

---

## 1. Shared prerequisite: org id in client auth state

Needed by D1 only. D2/D3/D4 do not touch clients.

### Dashboard (`binly-dashboard`)

1. `lib/auth/types.ts` — add `organization?: { id: string; name: string; slug: string }`
   to `LoginResponse` (mirrors the backend's `LoginOrganization`, already live).
2. `lib/auth/store.ts` — add `orgId: string | null` to `AuthState`; set it in
   `setAuth` (`:28-32`), clear it in `clearAuth` (`:34-38`).
3. **New** `lib/auth/org.ts` — `orgIdFromToken(token: string): string | null`,
   the `atob(parts[1])` decode already prototyped at
   `create-potential-location-dialog.tsx:663-687`, returning `payload.org_id`.
   Never trusted for authorization (the proxy is the boundary); used only to
   address a channel.
4. A `useOrgId()` selector: `store.orgId ?? orgIdFromToken(store.token)`. The
   fallback is what keeps already-persisted sessions alive (§0.4).
5. `app/(dashboard)/layout.tsx:32` — pass `orgId` into `<CentrifugoProvider>`
   alongside `token`, so the provider can expose a derived channel name rather
   than every call site re-deriving it.

Do **not** widen `subscribe(channel, handler)` (`centrifugo-provider.tsx:18`).
Add a second value to the context instead:

```ts
companyChannel: string | null   // `company:${orgId}:events`, null until org known
```

Four call sites then read it instead of embedding a literal, and the "forgot to
scope one" failure becomes impossible to write. This is strictly better than the
brief's "every call site must build the channel itself".

### Flutter (`ropacalapp`)

1. `lib/providers/auth_provider.dart` — extract the payload decode currently
   inlined in `_jwtExpired` (`:133-147`) into `_jwtPayload(String? token)`,
   then add `String? orgIdFromToken(...)` returning `payload['org_id']`.
   `_jwtExpired` keeps its behavior by calling it.
2. Expose it as a Riverpod provider (`currentOrgIdProvider`). Do **not** add the
   field to the freezed `User` model unless the org is shown in the UI — that
   forces `build_runner` regeneration *and* a cached-user migration
   (`_cacheUser`/`_loadCachedUser`, `:149-160`) for zero benefit.
3. `lib/core/services/centrifugo_service.dart:344-347` —
   `subscribeToCompanyEvents(void Function(...) onEvent)` becomes
   `subscribeToCompanyEvents(String orgId, ...)` and builds
   `'company:$orgId:events'`. One literal, one signature.
4. `lib/providers/centrifugo_provider.dart:206-229`
   (`_subscribeToCompanyEvents`) passes `ref.read(currentOrgIdProvider)`, and
   **returns early with a loud log if it is null** rather than interpolating
   `"null"` into a channel name.

---

## 0.9 Every GPS-dependent positive gate is UNRUNNABLE in the current state

Measured 2026-07-30: `shifts` holds `cancelled=455` and `ended=113` and **zero
rows in any active status**. So `/api/manager/drivers` and
`/api/manager/active-drivers` legitimately return `[]`, and the fleet map is
legitimately empty *right now*.

That means the following positive gates would produce **identical output before
and after a break** — i.e. they pass vacuously and prove nothing:

| Gate | Needs |
|---|---|
| D3 positives 1-3 ("a driver is publishing", `driver_current_location` advances, map shows markers) | an active shift + a device publishing GPS |
| D2 positive ("the marker moves for an admin of the owning org") | same |
| D4a positive ("driver on shift > 2h keeps working") | same, sustained |

**D1's positive gate is the only one runnable as-is** (edit a bin, watch a second
tab receive the event) and it is correctly specified.

Therefore, before running D2/D3/D4a gates, an **active shift with a live GPS
publisher must be deliberately staged.** That is a production write, on the same
footing as D4b's negative gate, and must be planned and owned rather than
discovered on the day. Alternatively mark those gates explicitly DEFERRED — but
do not record them as passed.

Note the state is volatile: a driver logging in and publishing (as happened at
21:30 on 2026-07-30, producing 45 accepted `publish-location` calls and a live
`ropacal:driver:…:location` key) does **not** by itself create an active shift.
Check `SELECT count(*) FROM shifts WHERE status='active'` at gate time, not
earlier.

**Before running any gate**, also confirm whether `CENTRIFUGO_PROXY_SECRET` is
set on the backend. `middleware/centrifugo_proxy.go` enforces the header whenever
the secret is non-empty — NOT only at 2+ orgs. As of 2026-07-30 it IS set and
Centrifugo does send the header (verified: 45 × `publish-location` → 200 under
enforcement), so every gate's `curl` against a proxy endpoint must include
`-H "X-Centrifugo-Proxy-Secret: <value>"` or it will 401 for the wrong reason.

## D1. `company:events` → `company:{orgID}:events`

### Current state (verified)

- Channel constructed at `internal/services/centrifugo/client.go:191`:
  `channel := "company:events"`, inside
  `PublishCompanyEvent(ctx, eventType string, data interface{})` (`:190`).
- **36 call sites in 16 files.** Org source in scope at each, verified by
  reading the enclosing function:

| Source | Sites |
|---|---|
| `orgdb.From(r)` already assigned in the handler | 27 |
| `db *orgdb.DB` function parameter (`shift_complete.go:1030` `moveCompletionSideEffects`) | 2 (`:1064`, `:1091`) |
| worker per-pass handle (`m.db` / `s.db` / `a.db`) | 5 (`ai_operations_agent.go:846`, `airtag_monitor.go:440`, `digest_scheduler.go:392/566/768`, `move_request_monitor.go:204`, `stale_shift_monitor.go:482` — 7 lines, 5 files) |
| **NOTHING in scope** | **2** |

  The two with nothing in scope are `shifts.go:656` (inside `PauseShift`,
  `:599`) and `shifts.go:742` (inside `ResumeShift`, `:678`). Both take
  `store ShiftStore`, not a `*sqlx.DB`, and neither calls `orgdb.From(r)`. They
  need `orgdb.From(r).OrgID()` added; that is safe because both are registered
  under the authed group (`cmd/server/main.go:462-463`) and
  `middleware.Auth` composes `Org` (`middleware/auth.go:33`).
- All five workers bind their org by **shallow copy** per pass
  (`o := *m; o.db = d`, e.g. `stale_shift_monitor.go:101-102`,
  `location_batch_writer.go:69-70`), and `DigestScheduler.ForOrg`
  (`digest_scheduler.go:63`) is used by the HTTP trigger too
  (`handlers/digest.go:38`). So `m.db.OrgID()` / `s.db.OrgID()` is never a
  stale singleton. Verified — this was the one way a naive D1 could publish an
  event under the wrong tenant.
- Subscribe parser: `authorizeSubscription` (`internal/handlers/centrifugo.go:278`),
  `case "company":` at `:319`, `if channelType == "events"` at `:322` with **no
  length guard**, role check at `:324`, `return role == "admin" || role == "manager"`
  at `:331`.
- Subscribers: dashboard `components/binly/global-centrifugo-sync.tsx:40`,
  `live-map-view.tsx:232`, `potential-locations-list.tsx:48`,
  `shift-details-drawer.tsx:225`; Flutter `centrifugo_service.dart:347`
  reached from `centrifugo_provider.dart:216`.

### Why `company:{orgID}:events` and not `company:events:{orgID}`

Both keep the load-bearing `company:` namespace prefix. But under today's
parser, `company:events:{orgID}` has `parts[1] == "events"` — it would **match**
`:322` and be authorized for any admin of any org, i.e. a stale backend would
silently accept it. `company:{orgID}:events` has `parts[1] == "<uuid>"`, misses
every case, and falls through to
`return false, fmt.Errorf("unknown channel format: %s", channel)` (`:335`) —
which the handler turns into a logged 403 (`:234-243`). **Loud, not silent.**
Pick the org-in-the-middle form for exactly that reason.

### Changes

**Publisher (`internal/services/centrifugo/client.go`)**

```go
func (c *Client) PublishCompanyEvent(ctx context.Context, orgID, eventType string, data interface{}) error
```

Signature change is the point: it breaks compilation at all 36 sites, so no
site can be silently missed. Do **not** be tempted to read the org from
`ctx` via `orgdb.ForContext(ctx)` — that would compile with zero call-site
edits and then **panic in production**, because `orgdb.ForContext` panics when
tenancy is live and no handle is stashed (`internal/orgdb/orgdb.go:110-121`),
and three sites pass `context.Background()`
(`shift_complete.go:1064`, `:1091`, `shifts.go:581`) while every worker holds
its handle as a variable rather than in the context. Verified: `shifts.go:581`
sits inside the `go func()` opened at `:516`, which closes over the `db` bound
at `:203` — so the *handle* is reachable there, the *context* is not.

During Deploy 1 the body publishes to both names; at Deploy 3 the legacy publish
is deleted:

```go
scoped := fmt.Sprintf("company:%s:events", orgID)   // orgID == "" → keep legacy only (dark mode)
// Deploy 1+2 only:
legacy := "company:events"
```

Guard `orgID == ""` (passthrough handles in local dev return `""` from
`OrgID()`, `orgdb.go:82`) by falling back to the legacy channel, so a
non-migrated dev database still works.

**Subscribe parser (`internal/handlers/centrifugo.go`, `case "company"` at `:319`)**

Replace the body with an explicit two-form parse:

- `len(parts) == 3 && parts[2] == "events"` → the scoped form. Require **both**
  `parts[1] == db.OrgID()` **and** the role check. Deny with a distinct log line
  when the orgs differ.
- `len(parts) == 2 && parts[1] == "events"` → legacy. Keep the role-only check,
  but log a deprecation line naming the user so adoption is observable
  (`⚠️ [Centrifugo] LEGACY company:events subscribe by user=%s`). **Deleted in
  Deploy 3.**
- Anything else → the existing `unknown channel format` error.

Note the legacy branch is the isolation hole. **D1 is not complete until Deploy
3 removes it.** Deploys 1–2 are migration, not security.

**Clients** — §1, plus the four dashboard sites switch from `'company:events'`
to the context's `companyChannel` (and skip the effect while it is `null`), and
Flutter passes `orgId` through.

### Deploy sequence

| # | Ships | Why this position |
|---|---|---|
| **1** | backend: parser accepts both forms + publisher dual-writes | The only deploy that can go first. Nothing subscribes to the new name yet, so the parser change is inert; the dual write pre-fills the new channel so Deploy 2 lands on a live feed. Fully backward compatible. |
| **2** | both clients: subscribe to `company:{orgID}:events` | Requires Deploy 1's parser (else 403) **and** its dual publish (else silence). Dashboard and Flutter are independent of each other — either can go first — but Flutter needs a store release, so start its review clock early. |
| **3a** | backend: drop the legacy PARSER branch only (keep dual publish) | Stale clients now fail **LOUD** on their next resubscribe (403 + logged), while the still-running legacy publish keeps feeding any subscription established earlier. This is the deploy that closes the cross-tenant hole. |
| **3b** | backend: drop the legacy PUBLISH | Only after 3a has been quiet for a full business day. Removing publish is what finally silences stale subscriptions, so it goes last. |

**Why Deploy 3 must be split (review finding).** A single combined Deploy 3 is
**SILENT, not semi-loud, for already-established subscriptions.** The subscribe
proxy returns no `ExpireAt` (`internal/handlers/centrifugo.go:264-270`), so a
subscription never expires and is re-authorized **only on (re)subscribe**. A
dashboard tab or Flutter session that has held `company:events` across the deploy
therefore gets no 403 and no error — it just goes quiet until it reconnects.
Splitting means 3a produces a loud, logged 403 on the next resubscribe while
delivery continues for anyone still attached, and 3b (publish removal) is the
only step that can silence anyone — by which point 3a's logs have proven nobody
is left.

**Adoption evidence for Deploy 3.** There is no version gate anywhere —
`grep` for `min_version|force_update|minVersion` finds nothing in the backend
or in `ropacalapp/lib`; `app_version` exists only as a column on
`app_error_logs`. And the `company` namespace has `presence: false`, so
Centrifugo presence cannot tell you who is still on the flat channel. The usable
signal is the subscribe proxy's own log — `centrifugo.go:216` already prints
`channel=%s` on every subscribe. Before Deploy 3a, confirm the Railway logs show
**zero** `channel=company:events` lines over a full business day while showing
`channel=company:<uuid>:events` for real admins.

**This gate has a known blind spot — do not over-trust it.** Because
subscriptions never expire (no `ExpireAt`), the log only ever observes *new*
subscribes. A long-lived Flutter session or an un-reloaded browser tab that
subscribed days ago is invisible to it, so the count can read zero while stale
clients are still attached. That is exactly why 3a (parser only) precedes 3b
(publish removal): 3a converts those invisible clients into loud, logged 403s at
their next resubscribe, so 3b is decided on evidence rather than on this
count alone.

### What breaks if the order is violated

| Violation | Presentation |
|---|---|
| **Publisher-first** (flip publish before parser+clients) | **SILENT.** Old clients still subscribe to `company:events` successfully — `:322` has no length guard so the 2-part channel still matches, the role check still passes, the proxy returns `Result` — and then receive nothing forever. No client error, no dashboard indicator (`CentrifugoStatus` stays `connected`), no backend log. Caches simply stop updating; it reads as "the dashboard is stale", which is the hardest class of bug to attribute. |
| **Clients-first** (Deploy 2 before Deploy 1) | **LOUD.** `authorizeSubscription` falls through to `:335`, the handler logs `❌ [Centrifugo] Authorization error ... unknown channel format` (`:234`) and returns 403; the dashboard logs `❌ [Centrifugo] Subscription error` (`centrifugo-provider.tsx:107`), Flutter logs from `subscription.error.listen` (`centrifugo_service.dart:377`). Recovery is instant — deploy the backend. |
| **Deploy 1 without the dual write** (the brief's literal order) | **SILENT, for the whole window between Deploys 2 and 3.** Same shape as publisher-first. §0.3. |
| **Deploy 3a before adoption** | **LOUD for anything that resubscribes** (403 logged both sides; symptom is a manager view with no live updates) but **SILENT for subscriptions already established** — they never re-authorize, so they simply go quiet. Reversible by re-adding the legacy branch. |
| **Deploy 3 combined (3a+3b at once)** | **SILENT for established subscriptions.** Dropping publish and parser together removes both the feed and the error path in one step, so long-lived clients get no 403 and no data. This is why 3 is split. |
| **Org #2 provisioned while the legacy publish still exists (Deploys 1-2)** | **SILENT cross-tenant leak.** The legacy `:322` branch is role-only, so any admin of any org receives every org's events on the flat channel. Hard precondition: 3b completes before a second org exists — or gate the legacy publish on `soleActiveOrgID() != ""`. |
| **Client change reading org only from the login response** | **SILENT** for every already-logged-in session: channel becomes `company:undefined:events`, denied at the proxy, and the dashboard shows "connected" with a dead feed. §0.4 is the mitigation. |

### Gate

**Negative — and this one works with ONE org.** The check is "connection-meta
org == channel org", so a *nonexistent* org id is as foreign as a real second
tenant's. Against the subscribe proxy directly (add
`-H "X-Centrifugo-Proxy-Secret: …"` once Phase A has landed):

```
POST /api/centrifugo/subscribe
{"user":"<a real admin id>","channel":"company:11111111-1111-1111-1111-111111111111:events",
 "meta":{"org_id":"00000000-0000-0000-0000-000000000001"}}
```
must return `{"error":{"code":403,...}}` and log an org-mismatch line. Repeat
with the two ids swapped — also 403. This is the single most valuable gate in
the plan: it proves the cross-tenant denial before a second tenant exists.
After Deploy 3, add: `"channel":"company:events"` must **also** 403.

**Positive — org A's own dashboard still receives events.** Deploy 1: from one
dashboard tab, edit a bin's status; a second tab must reflect it without a
reload, and its console must show
`📡 [GlobalCentrifugoSync] received event: bin_updated`. Deploy 2: the same
check, plus the console must show
`✅ [Centrifugo] Subscribed to company:00000000-0000-0000-0000-000000000001:events`
and **no** `Subscribed to company:events`. Deploy 3: repeat the bin edit, and
separately confirm on the Flutter manager view that a `move_request_created`
raised from the dashboard produces the in-app notification
(`notification_adapters.dart` path). Do not accept "the subscribe succeeded" as
the positive — the whole failure mode is a successful subscribe with no traffic.

### Rollback

- Deploy 1 alone: revert the commit. Both forms disappear; the flat channel was
  never stopped, so old and new clients are unaffected (new clients did not
  exist yet).
- Deploy 2 alone: dashboard is an instant Vercel/Railway rollback. **Flutter is
  not** — a shipped build cannot be recalled. Its safety net is that Deploy 1's
  dual publish keeps feeding the new channel indefinitely, so the mobile change
  is safe to leave in place even if the backend is rolled back to Deploy 1.
  This asymmetry is the reason Deploy 3 must be a *separate* deploy from
  Deploy 1.
- Deploy 3: revert to restore the legacy publish and parser branch. Since
  nothing else depends on it, this is a clean single-commit revert. Keep
  Deploy 3 as its own commit touching only `client.go` and the `case "company"`
  block for exactly this reason.

---

## D2. Org-equality in subscribe authorization

### Current state (verified)

`canViewDriverLocation` (`internal/handlers/centrifugo.go:339-357`):

```go
if userID == driverID { return true, nil }          // :341
err := db.Get(&role, `SELECT role FROM users WHERE id = $1`, userID)   // :347
return role == "admin" || role == "manager", nil    // :356
```

`db` is org-bound to the **subscriber's** org (from connection meta, via
`proxyOrgDB`, `:158-184`). So the lookup confirms the subscriber is an admin
*of their own org* and then authorizes any `driverID` whatsoever. The driver's
org is never consulted. Confirmed hole.

`canViewShift` (`:360-387`) has the identical shape: shift lookup at `:363`,
then `return role == "admin" || role == "manager"` at `:386`. Given a shift
UUID, a foreign admin gets that shift's live updates. **D2 must cover both** —
same function family, same three-line fix, and leaving one half done is worse
than leaving both.

Supporting facts, queried read-only against prod: `users.organization_id` is
`text NOT NULL` with `fk_users_organization`; `users_pkey` is still
`PRIMARY KEY (id)` (so `WHERE id = $1` stays index-backed) plus
`uq_users_org_id UNIQUE (organization_id, id)`; `shifts.organization_id`
exists; `users`, `shifts` and `driver_current_location` all have
`relrowsecurity = t` **and** `relforcerowsecurity = t`; the policy is
`org_isolation_users` for `ALL` with
`organization_id = NULLIF(current_setting('app.org_id', true), '')`.

### Changes

`internal/handlers/centrifugo.go` only. Two functions, plus one small helper.

`canViewDriverLocation` — after the self check at `:341`, resolve the **target**
on the same org-bound handle and compare explicitly:

```go
var target struct {
    Role  string `db:"role"`
    OrgID string `db:"organization_id"`
}
err := db.Get(&target, `SELECT role, organization_id FROM users WHERE id = $1`, driverID)
if err == sql.ErrNoRows { return false, nil }          // foreign or nonexistent driver
if err != nil { return false, fmt.Errorf(...) }
if db.OrgID() != "" && target.OrgID != db.OrgID() { return false, nil }
```

then keep the existing subscriber-role lookup. `canViewShift` gets the same
treatment on `SELECT driver_id, organization_id FROM shifts WHERE id = $1`
(`:363`).

Two deliberate design points:

- **Both belts, not one.** RLS alone already makes `ErrNoRows` the answer for a
  foreign driver. The explicit `target.OrgID != db.OrgID()` comparison is what
  survives an RLS-policy refactor, a future handle that is accidentally
  passthrough, or a migration that drops `FORCE`. Cheap; keep it.
- **`db.OrgID() != ""` guard.** `orgdb.Passthrough` handles return `""`
  (`orgdb.go:82`), and `proxyOrgDB` returns a passthrough while tenancy is dark
  (`centrifugo.go:160-162`). Without the guard, local dev against a
  non-migrated database denies everything.

Log denials distinctly (`🚫 [Centrifugo] Cross-org subscribe blocked: user=%s
channel=%s`) so the gate has something to assert on.

### Deploy sequence

**One deploy. No client changes, no ordering constraints, no coordination with
Centrifugo config.** This is why it goes first.

The only sequencing that matters is *within* the item: change
`canViewDriverLocation` and `canViewShift` **together**. Shipping only the
driver half leaves shift updates (`shift:updates:{id}` — which carries the full
task list, see `shifts.go:526-540`) cross-tenant readable while the changelog
says "D2 done".

### What breaks if the order is violated

There is no order to violate, but two implementation mistakes have distinct
signatures:

- **Comparing against the wrong side** (`target.OrgID != userOrg` where
  `userOrg` came from a second lookup rather than `db.OrgID()`): **silent
  no-op** if both lookups run on the same org-bound handle, because both
  already return the same org. It would look like it works and protect nothing.
  Using `db.OrgID()` — the value that came from the *token*, not from a
  row RLS already filtered — is what makes the comparison meaningful.
- **Forgetting the `db.OrgID() != ""` guard**: **loud** — every subscribe on a
  non-migrated dev DB is denied and every local realtime test fails immediately.

### Gate

**Negative (one org, today).** Pick any UUID that is not in `users`:

```
POST /api/centrifugo/subscribe
{"user":"<real admin id>","channel":"driver:location:22222222-2222-2222-2222-222222222222",
 "meta":{"org_id":"00000000-0000-0000-0000-000000000001"}}
```
Before: `{"result":{...}}` — authorized (this is the bug; capture the output as
the "before" artifact). After: `{"error":{"code":403,"message":"permission denied"}}`
plus the cross-org log line. Repeat for
`shift:updates:33333333-3333-3333-3333-333333333333`.

**Positive.** Same call with a **real** driver id from
`SELECT id FROM users WHERE role='driver' LIMIT 1` (8 exist) must still return
`{"result":...}`. Then end-to-end: with a driver on an active shift, the
dashboard fleet map must still show that driver's marker moving —
`lib/hooks/use-active-drivers.ts:100` subscribes per driver, so a regression
here silently freezes every marker while the map still renders. Also confirm a
driver subscribing to their **own** `driver:location:{self}` still authorizes
(the `userID == driverID` short-circuit at `:341` must stay ahead of the new
lookup) and that the Flutter manager map (`manager_map_page.dart:235`) still
tracks a driver.

### Rollback

Single-commit revert of `internal/handlers/centrifugo.go`. Nothing else changes,
no client is aware of it, no data is migrated. If a revert is needed, expect the
symptom that triggered it to be a **false denial**, so before reverting check
the new log line for which side mismatched — a wrong `organization_id` on a
`users` row is a data problem, not a code problem.

---

## D3. Redis GPS keys → `ropacal:org:{orgID}:driver:{id}:location`

### Current state (verified)

`internal/services/redis/client.go`:

- `SaveDriverLocation` `:54-57` — key at `:55`, 10-minute TTL at `:56`.
- `GetDriverLocation` `:60-63` — key at `:61`.
- `GetAllDriverLocations` `:67-95` — `pattern := "ropacal:driver:*:location"` at
  `:69`, blocking `KEYS` at `:70`, and the **hard 4-segment parse-back** at
  `:87-88`:
  `if len(parts) == 4 && parts[0] == "ropacal" && parts[1] == "driver" && parts[3] == "location"`.
  Adding an org segment makes every key fail this test, so writer and parser
  must change in the **same deploy**. The brief is right about this.
- `DeleteDriverLocation` `:98-101` — zero callers.

Call sites, all with an org already in scope (verified by reading each
enclosing function):

| Site | Org source |
|---|---|
| `handlers/centrifugo_location_proxy.go:196` (write) | `odb` from `proxyOrgDB` at `:82` |
| `handlers/driver_location.go:581` (write, `UpdateLocation` `:465`) | `db := orgdb.From(r)` `:467` |
| `handlers/driver_location.go:216` (`resolveDriverStartLocation` `:211`) | `db *orgdb.DB` param |
| `handlers/driver_location.go:307` (`CheckShiftDriverProximity` `:261`) | `db := orgdb.From(r)` `:263` |
| `handlers/shift_dependencies.go:163` (`CheckBinDependencies` `:146`) | `db := orgdb.From(r)` `:148` |
| `services/stale_shift_monitor.go:339` (`getLastGPSTime`) | `m.db` (per-pass copy, `:101-102`) |
| `handlers/driver_location.go:682` (read-all, `GetAllDrivers` `:671`) | `db := orgdb.From(r)` `:673` |
| `handlers/manager_active_drivers.go:80` (read-all, `GetActiveDrivers` `:61`) | `db := orgdb.From(r)` `:63` |
| `services/location_batch_writer.go:82` (read-all) | `w.db` (per-pass copy, `:69-70`) |

### Changes

**`internal/services/redis/client.go`** — thread `orgID` through all four
methods and rename the read-all:

```go
func driverLocationKey(orgID, driverID string) string {
    if orgID == "" { return fmt.Sprintf("ropacal:driver:%s:location", driverID) } // dark mode
    return fmt.Sprintf("ropacal:org:%s:driver:%s:location", orgID, driverID)
}
func (c *Client) GetOrgDriverLocations(ctx context.Context, orgID string) (map[string]string, error)
```

Replace the positional `strings.Split` assert at `:87-88` with a
`strings.TrimPrefix`/`TrimSuffix` extraction against the *known* prefix, so the
parse can no longer disagree with the writer:

```go
prefix := fmt.Sprintf("ropacal:org:%s:driver:", orgID)
if id, ok := strings.CutPrefix(key, prefix); ok {
    if id, ok := strings.CutSuffix(id, ":location"); ok { result[id] = locationJSON }
}
```

One builder, one matching extractor, both keyed off the same prefix constant.
The current bug class — writer and parser drifting — becomes unrepresentable.

**`internal/services/location_batch_writer.go`** — `:82` becomes
`GetOrgDriverLocations(ctx, w.db.OrgID())`. Keep the `EXISTS (SELECT 1 FROM
users WHERE id = $1)` guard added at `:127`: with org-scoped keys it is
redundant, and that is exactly why it should stay — it is the belt that catches
a key written under the wrong prefix. Do **not** delete it as "now unnecessary".

Also note the loop shape improves for free: today each of N org passes performs
a full-keyspace `KEYS` (`:82` inside `ForEachActiveOrg` at `:68`); after the
change each pass matches only its own prefix.

### 3.4 Recommended: fold in `KEYS` → `SCAN`

`TENANCY_BACKLOG.md` files this as Tier 2. Given §0.1 — the pattern runs against
the **same Redis instance Centrifugo's engine uses**, blocking it, N times per
30 seconds — and given that `GetAllDriverLocations` is being rewritten anyway,
do it here. It is a ~10-line change (`c.Scan(ctx, cursor, pattern, 100)` in a
loop) inside a function whose every caller is already being touched. Separable
if you want a minimal diff, but the coupling it removes is real and it will
never be cheaper than in this deploy.

### Deploy sequence

**One deploy** — writer, both readers and the parse-back are the same function
family and must move together (`:87-88` is the proof).

**The migration question, resolved by measurement.** The brief warns of "up to
10 minutes [where] old keys are unreadable (drivers show offline on the fleet
map until they next publish)". Two facts soften that:

1. Drivers publish every 1–3 seconds while on shift
   (`stale_shift_monitor.go:338` comment; the Flutter publisher is
   `location_tracking_service.dart:577`). So an *active* driver re-populates
   the new key within seconds. The 10-minute figure only applies to a driver
   whose last fix is already stale, i.e. someone who has effectively gone
   offline anyway.
2. Right now the live Redis holds **zero** `ropacal:*` keys (verified by scan —
   no driver is publishing). **Deploy during a no-active-shift window and the
   migration cost is exactly zero.** Check first:
   `redis-cli -u "$REDIS_URL" --scan --pattern 'ropacal:*'` → empty means free.

**Therefore: do not implement read-both.** Beyond being unnecessary, it is
actively wrong to leave in place — merging un-prefixed `ropacal:driver:*` keys
into an org-scoped result re-creates the very leak D3 closes the moment a
second org exists (org B's reader would absorb org A's flat keys). If you do
add it as a belt for a mid-shift deploy, gate it on the single-org check that
already exists (`soleActiveOrgID`, `handlers/centrifugo.go:137`) and delete it
in the following deploy — do not carry it to org #2.

### What breaks if the order is violated

| Violation | Presentation |
|---|---|
| Writer changed, parse-back at `:87-88` not | **SILENT.** `GetAllDriverLocations` returns an empty map: 4-segment assert fails on every 6-segment key. Fleet map and `/api/manager/drivers` return empty arrays with HTTP 200; the batch writer logs `No locations to write (no active drivers)` (`location_batch_writer.go:89`) which reads as normal off-hours output. `driver_current_location` silently stops advancing, and the durable fallback in `resolveDriverStartLocation` (`driver_location.go:232-249`) goes stale, degrading preflight and StartShift **hours later** — far from the cause. This is the worst failure in the whole plan; it is prevented by the two changes being in one function file. |
| Parse-back changed, writer not | **SEMI-LOUD.** Reads return empty immediately (no key matches the new prefix), so the fleet map empties during a live shift — noticed fast, but with the same "no drivers" presentation. |
| Point readers missed (`driver_location.go:216/307`, `shift_dependencies.go:163`, `stale_shift_monitor.go:339`) | **Compile error.** The `orgID` parameter is required, so this cannot ship. That is the reason to change the signature rather than add an overload. |
| Read-both left in past org #2 | **SILENT cross-tenant leak.** No error, no log — org B's fleet map just contains org A's drivers. See above. |

### The cascade this item can trigger — verified, and worse than a broken map

If D3 breaks the read path, the visible symptom is an empty fleet map. The
**invisible** one, two hours later, is that live shifts get auto-ended.

Chain (all verified read-only):
1. The WS GPS fast path writes **Redis only** —
   `internal/handlers/centrifugo_location_proxy.go:196` calls
   `redisClient.SaveDriverLocation` and nothing else.
2. `driver_current_location` in Postgres is advanced **solely** by the 30s batch
   writer (`internal/services/location_batch_writer.go`), which reads Redis via
   the same `GetAllDriverLocations` D3 modifies.
3. `stale_shift_monitor.go` checks Redis first, then **falls back to Postgres**
   `driver_current_location` (`:336-338`, "Checks Redis first, falls back to
   PostgreSQL").
4. `StaleThreshold = 2 * time.Hour` (`:21-22`). Past it, `m.autoEndShift(shift)`
   fires (`:203`) with `end_reason = "driver_disconnected"` and archives the
   shift to `shift_history`.

So a writer/parser mismatch stalls that Postgres row, and **120 minutes later
every active shift is closed underneath its driver** — mid-route, with an
archive record that looks legitimate. Nothing in the first two hours indicates
it is coming.

Consequences for this item:
- Positive gate #3 (`driver_current_location.updated_at` advances within 60s of a
  driver publishing) is **MANDATORY and blocking**, not a follow-up check. It is
  the only signal that arrives inside the 2-hour fuse.
- **Pre-agreed revert trigger:** if that row has not advanced within **5 minutes**
  of the deploy while a driver is publishing, revert immediately. Do not
  investigate first — the investigation window and the fuse are the same clock.
- Re-run the `ropacal:*` key scan **immediately before** deploying. The
  zero-keys observation in §0 was taken with zero active shifts and is a
  statement about that moment, not a property of the system.

### Gate

**Negative (one org).** With a driver publishing:

```
redis-cli -u "$REDIS_URL" --scan --pattern 'ropacal:org:00000000-0000-0000-0000-000000000001:driver:*'   # non-empty
redis-cli -u "$REDIS_URL" --scan --pattern 'ropacal:org:11111111-1111-1111-1111-111111111111:driver:*'   # EMPTY
redis-cli -u "$REDIS_URL" --scan --pattern 'ropacal:driver:*'                                            # EMPTY (nothing writes the flat form any more)
```
The middle line is the org-partitioning proof and needs no second tenant. Also
assert `centrifugo.*` keys are untouched — the two families share one instance
(§0.1) and the rename must not collide.

**Positive.** Three assertions, because "empty" is the failure mode:
1. `GET /api/manager/drivers` and `GET /api/manager/active-drivers` return the
   publishing driver with fresh coordinates.
2. The dashboard fleet map shows that driver's marker and it moves.
3. `SELECT driver_id, updated_at FROM driver_current_location ORDER BY
   updated_at DESC LIMIT 5` advances within 60s of a fix (proves the batch
   writer's per-org read works, which is the assertion the map alone does not
   make — the map reads Redis directly).

Then, minutes later, confirm `resolveDriverStartLocation` still reports
`Source: "redis"` on a preflight.

### Rollback

Single-commit revert of `redis/client.go` + the nine call sites (all mechanical
signature edits). Rolling back re-introduces the flat writer; any org-prefixed
keys written in the meantime become unreadable but expire on their own 10-minute
TTL, and active drivers re-populate the flat keys within seconds. So rollback is
symmetric with the forward migration and equally cheap — again, cheapest during
a no-shift window. Keep D3 in one commit that touches no other item, so
`git revert` is sufficient.

---

## D4. Close the revocation window

### Current state (verified)

Two independent windows, with two different levers. The brief is right that D4
as scoped fixes neither properly.

| Path | Trusted artifact | Lifetime | Set at |
|---|---|---|---|
| Realtime (Centrifugo proxies) | connection token's `meta.org_id` | **24h** | `handlers/centrifugo.go:526` |
| HTTP (every `/api` call) | app JWT's `org_id` claim | **7 days** | `handlers/auth.go:191` |

- `middleware.Auth` (`middleware/auth.go:32-...`) validates signature and `exp`
  and performs **no database lookup at all**.
- `middleware.Org` (`middleware/org.go:50-63`) requires `claims.OrgID` to be
  non-empty and then calls `orgdb.New(root, claims.OrgID)`, which validates
  **only that the string parses as a UUID** (`orgdb.go:53-61`). It does not
  check that the org exists, that it is `active`, or that the user still belongs
  to it.
- **There is no refresh endpoint.** `grep -n 'refresh|Refresh' cmd/server/main.go
  internal/handlers/auth.go` → no matches.

Net: a user removed from an org keeps full org-scoped read/write access for up
to 7 days, and a live socket keeps its org scope for up to 24h. A *nonexistent*
org id in a token fails **closed** (RLS returns zero rows everywhere), so that
direction is not a leak — the exposure is a legitimately-minted token
outliving its membership.

Client refresh behavior, verified — this is what makes the Centrifugo lever safe:
- Dashboard: `getToken: () => fetchCentrifugoToken()`
  (`centrifugo-provider.tsx:174`); `expires_at` is never read.
- Flutter: `getToken: (event) async {...}` (`centrifugo_service.dart:80-110`);
  `expires_at` is used **only for logging** (`:64/66/87/90`) — the Centrifuge
  SDK drives refresh off the token's own `exp`.

### Changes — split into two, ship them separately

**D4a — Centrifugo connection token TTL: 24h → 1h.** One line,
`handlers/centrifugo.go:526`. Both SDKs auto-refresh. Recommend 1h rather than
minutes: refresh runs through `apiFetch` on the dashboard
(`centrifugo-provider.tsx:58`) which carries the **global 401 → logout redirect**
(`lib/api/client.ts:63-80,125`), so the Centrifugo TTL is also the interval at
which the dashboard *discovers* app-JWT expiry and hard-logs-out. At 1h that is
harmless; at 5m it multiplies logout races and reconnect churn for no extra
security (the subscribe proxy is the real boundary, and it re-authorizes on
every new subscribe regardless).

**D4b — revalidate org membership instead of shortening the app JWT.** Do
**not** shorten the 7-day app JWT. Without a refresh endpoint it would log
dashboard users out on a timer and — worse — bounce **drivers mid-shift**
(Flutter's cold-start path already checks `_jwtExpired` at
`auth_provider.dart:76`, and a mid-shift 401 costs GPS tracking and shift
state). Shortening to 12h is a 14× improvement with zero client work but still
risks a driver who logged in 11h before a shift.

The lever that actually delivers "revocation in minutes" is a cached membership
check in `middleware.Org`, immediately AFTER the org-bound handle is built (it
cannot precede it — see the blocker below):

```go
// (userID, orgID) -> ok, cached ~60s.
// MUST run on an ORG-BOUND handle, never the root pool:
odb, err := orgdb.System(root, claims.OrgID)   // sets app.org_id
// then, on odb:
SELECT u.role FROM users u WHERE u.id = $1 AND u.organization_id = $2
// plus, on the ROOT pool (organizations is catalog-readable):
SELECT 1 FROM organizations WHERE id = $1 AND status = 'active'
```

**BLOCKER this replaces (found in review).** The original snippet ran on the
root pool and would have **401'd every authenticated request in production** —
a total outage. `users` carries exactly ONE policy, `org_isolation_users`
(`ALL`, `USING (organization_id = NULLIF(current_setting('app.org_id', true), ''))`),
with `relrowsecurity=t` and `relforcerowsecurity=t`. On the root pool
`app.org_id` is unset, so that predicate is never true and the query returns
zero rows for *every* user, forever → the middleware rejects everyone.

The two precedents originally cited (`soleActiveOrgID`,
`CentrifugoProxyAuth.isMultiTenant`) are NOT applicable: they query
`organizations`, which has a *second* permissive policy `org_catalog_read`
(`FOR SELECT USING (true)`). `users` has no such policy. This is the same trap
already documented at `internal/handlers/auth.go:147-148`, and the third
instance of it in this project.

Miss → 401 `re-login required` (the status both clients already treat as a
forced logout). Needs no client change, no new endpoint, no token-lifetime
tradeoff.

**Cost is 4 round trips per cache miss, not one.** `orgdb.begin()`
(`internal/orgdb/orgdb.go:129-142`) issues `Beginx` + `SELECT set_config(...)`,
then the statement, then `COMMIT`. Amortised over 60s per (user, org) that is
acceptable, but on Railway RTT the earlier "one indexed lookup" was wrong.

Three details to get right:
- **DB-error policy: fail CLOSED, with a bounded stale window.** Serve the last
  cached positive for at most **5 minutes**, then 503. Unbounded stale-serving
  silently restores the 7-day window during any sustained DB problem — the exact
  failure the item exists to close. Note a 503 from middleware bypasses the
  dashboard's 401→logout path, so it surfaces as a generic error, not a re-login.
- **Cache `role`, not just membership.** `middleware.RequireRole` reads
  `userClaims.Role` from the JWT (`internal/middleware/auth.go:184-195`), so a
  DEMOTED admin keeps admin HTTP authority for up to 7 days. Select `u.role` in
  the same query and 401 when it disagrees with the claim. Still not covered:
  password change revokes nothing (no jti / token version) — state that limit
  rather than implying "revocation on every path".
- It also hardens D4a: `GET /api/centrifugo/token` sits behind `Auth`→`Org`, so
  a revoked user can no longer mint a fresh connection token. Combined window
  for realtime revocation = Centrifugo TTL + 60s.

A real refresh-token mechanism (short access token + rotating refresh token,
both clients) remains the textbook answer and is strictly more work than the
rest of Tier 1 combined. It is not required to achieve D4's stated goal. Record
it as Tier 2 and note explicitly *why* it was not chosen.

### Deploy sequence

Two independent deploys, either order. Recommended **D4b then D4a**, so that by
the time connection tokens are being reissued hourly, the endpoint reissuing
them is already membership-checked.

### What breaks if the order is violated

- **D4a before D4b:** nothing breaks; you simply get more frequent token
  reissues that are not yet membership-checked. Harmless, hence "either order".
- **Shortening the app JWT instead of doing D4b:** **LOUD and bad.** Dashboard
  users bounce to `/login` on a timer; drivers lose their session mid-shift.
  Explicitly rejected above.
- **D4b failing open on DB error:** **SILENT.** The check appears to work and
  enforces nothing whenever the pool is briefly saturated. This is why the
  error policy is part of the change, not an afterthought.

### Gate

**Positive — D4a.** Leave a dashboard tab open for **>2h** (2× the new TTL) and
confirm it still receives a `company:` event (raise one by editing a bin) — this
proves refresh works rather than just that the first connection worked. Console
must show a `/api/centrifugo/token` refetch and **no** disconnect/logout. On
Flutter, keep a driver on shift >2h and confirm
`✅ [Centrifugo] Token refreshed successfully` appears and GPS keeps landing in
Redis.

**Positive — D4b.** A normal admin session makes >60s of continuous requests
across the cache boundary with no interruption; and `SELECT count(*) FROM
pg_stat_activity` / pool metrics show no growth (proves the cache is caching,
not adding a lookup per request).

**Negative — D4b.** Requires one deliberate, reversible **write**, so it is out
of scope for this planning pass and must be run knowingly: create a throwaway
admin in the ropacal org, log in as them, keep the token, confirm a request
succeeds, `DELETE` that user row, then confirm the **same** token 401s within
60s — while a local decode of that token shows `exp` still far in the future
(that last part is what distinguishes "revocation worked" from "the token just
expired"). Restore by deleting nothing else; the throwaway user is the only
artifact.

**Negative — D4a.** Decode a freshly issued connection token and assert
`exp - iat == 3600`.

### Rollback

Both are one-line/one-block reverts in separate commits. D4a: restore
`24 * time.Hour`; live connections keep working (SDKs simply refresh less
often). D4b: remove the check; the only effect is the window reopening. Neither
touches data, clients, or Centrifugo config, so both can be reverted
independently and at any time.

---

## Cross-item ordering

```
D2  ── backend only, 1 deploy, no deps ───────────────────────► ship first
D3  ── backend only, 1 deploy, no deps (prefer a no-shift window)
D4b ── backend only, 1 deploy, no deps
D4a ── backend only, 1 deploy, after D4b (soft preference)
D1  ── 3 deploys, needs §1 client org state, needs a mobile release
```

Rationale:

- **D2 first.** Smallest diff, highest security value, zero client coupling, and
  it is the only item whose negative gate can be run *before* anything else
  changes — which makes it the cheapest place to validate that the
  proxy-request test harness (the `curl` in the gates) works at all. Every later
  gate reuses it.
- **D3 before D1.** Both touch the realtime path, but D3's risk is concentrated
  in a single deploy that is free during a quiet window, whereas D1 spans three
  deploys and a store review. Don't have both in flight.
- **D4 anywhere before D1's Deploy 3.** D4b tightens `/api/centrifugo/token`,
  which D1's clients call more often once D4a lands. Independent otherwise.
- **D1 last**, per `PRE_CLIENT_PLAN.md`'s "order within D", and because it is the
  only item with an irreversible step (a shipped Flutter build).

### Interaction with Phases A–C of `PRE_CLIENT_PLAN.md`

- **Phase A (Centrifugo proxy secret + `include_connection_meta`) is a hard
  prerequisite for org #2, not for building D1–D4.** All four items work today
  under the single-org grace: `proxyOrgDB` (`centrifugo.go:158-184`) resolves the
  sole active org when meta is absent, so D1's org-equality check and D2's
  target-org check have a real org to compare against even before Centrifugo
  forwards meta. **But note the consequence:** until Phase A lands, the org the
  proxy uses comes from `soleActiveOrgID`, not from the connection — so D1/D2's
  cross-org denials are *implemented and testable* but not yet *load-bearing*.
  They become load-bearing the moment meta arrives. Both must ship before org #2
  regardless.
- **Phase B (login `organization` field) is NOT a blocker for D1**, contrary to
  the brief — see §0.4. The backend contract is already live and both clients can
  read `org_id` from the JWT they already hold. D1's client work and Phase B's
  form work touch adjacent code (`lib/auth/*`, `login_page.dart`) so they are
  natural to ship in one client release, but neither depends on the other.
- **Phase C (provisioning)** is what unblocks the two-org gates below. Nothing in
  D1–D4 needs it to be *built*.

### Testable with one org vs. requires two

**Fully provable with one org (today):**
- D2, both directions. A UUID absent from `users` is exactly as foreign to an
  org-bound handle as a real second tenant's driver, because the denial comes
  from `ErrNoRows` / an `organization_id` mismatch either way.
- D1's org-equality logic, using a syntactically valid but nonexistent org id in
  the channel (`company:11111111-…:events`) against a real admin's meta.
- D1's positive path end to end (dashboard + Flutter manager view).
- D3 entirely — the negative is a Redis prefix scan for a foreign org id.
- D4a and D4b entirely.

**Requires a second org:**
- That a *real* second tenant's admin, holding a *real* connection with their
  own meta, is denied — i.e. that meta plumbing, `proxyOrgDB`, the channel
  parse and the org comparison compose correctly on live traffic rather than in
  a hand-rolled `curl` body. The single-org gates prove each link; only two orgs
  prove the chain.
- That org A's events never appear in org B's dashboard, and that
  `GetOrgDriverLocations(orgB)` excludes org A's drivers under concurrent load.
- The batch writer writing both orgs' `driver_current_location` rows correctly
  in one 30s cycle, with the `EXISTS` guard doing its job
  (`location_batch_writer.go:115-139`).

**Reminder from `PRE_CLIENT_PLAN.md`:** creating org #2 immediately hardens four
gates that disagree about what counts as a second tenant (two count all
statuses, two count only `active`), so a "quietly staged" tenant is not
available. Phases A and B must be complete first.

---

## Could not verify

Stated plainly rather than assumed.

1. **Centrifugo's namespace resolution for a 3-segment channel.** The plan
   depends on `company:{orgID}:events` resolving to the `company` namespace
   (first segment before the default `:` boundary) so that
   `subscribe_proxy_enabled: true` still applies. I could not exercise a running
   Centrifugo. Two readings exist — a non-matching namespace is either treated
   as no-namespace (no proxy) or rejected as an unknown channel — and both make
   keeping the `company:` prefix mandatory, so the design is unaffected. But
   **verify empirically in Deploy 1** before Deploy 2 ships: subscribe to
   `company:<orgid>:events` from a browser console and confirm the backend logs
   `🔐 [Centrifugo] Subscribe request: ... channel=company:<orgid>:events`
   (`centrifugo.go:216`). If that log line never appears, the subscribe proxy
   is not being invoked for the new form and the namespace assumption is wrong.
2. **The live Centrifugo service config.** I read
   `centrepo/config.json` at commit `8a4389f` from a scratchpad clone of
   `tapplerdev/binly-centrifugo-service` (now private) and confirmed it matches
   the separately cached `cf_config.json`. I could not confirm the running
   service is on that commit, nor read its Railway env vars — and
   `PRE_CLIENT_PLAN.md` records that env vars can augment file-defined proxies
   field by field. The deployment is **mixed-shape** (`channel.proxy.subscribe`
   / `channel.proxy.publish` unified, plus a granular named `location_publish`
   proxy), which matters for Phase A's env-var naming but not for D1–D4.
3. **Whether the Centrifugo service and the backend share one Redis *database*
   or merely one *instance*.** I proved they share the instance (§0.1) by seeing
   `centrifugo.*` keys via the backend's own `REDIS_URL`, which means the same
   logical DB index. I did not check whether Centrifugo is configured with a
   key prefix that could change under a config update.
4. **Real-world Flutter manager-mode usage.** The code path is live
   (`centrifugo_provider.dart:216`, `manager_map_page.dart:235/324`) and 6 admin
   users exist, but I have no telemetry on how many admins actually use the app
   versus the dashboard. That number determines how painful D1's Deploy 3 is for
   stale builds, and there is no version gate to fall back on (§D1, "Adoption
   evidence").
5. **Railway deploy/log retention for the adoption check.** Deploy 3's gate
   assumes a full business day of subscribe-proxy logs is queryable. The
   Centrifugo service's own boot logs are already documented as beyond retention
   (`PRE_CLIENT_PLAN.md`, "Blockers"); I did not confirm the backend service's
   retention window.
6. **Nothing was executed against production.** All DB and Redis access was
   read-only (`SELECT`, `SCAN`, `DBSIZE`). No source file was modified. No
   Railway or Centrifugo configuration was touched. The one gate that needs a
   write (D4b's negative) is called out as such and deliberately not run.
