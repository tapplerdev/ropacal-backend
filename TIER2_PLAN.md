# Tier-2 tenant isolation — implementation plan, and a verdict on Tier-1

Written 2026-07-29 (planning only: no source file modified, nothing executed
against production — all DB access was `SELECT`, no Redis/Railway/Centrifugo
changes). Companions: `TIER1_PLAN.md` (the plan this builds on and critiques),
`TENANCY_BACKLOG.md` (where Tier-2 was first filed), `PRE_CLIENT_PLAN.md`.

Two jobs in one document:

- **§1 Tier-1 compatibility** — is `TIER1_PLAN.md` a sound foundation, and what
  must change in it *before* implementation so Tier-2 is not made harder.
- **§2–§5** — the Tier-2 plan itself, per item, with deploy order, failure
  presentation, gates and rollback.

**Three items filed as Tier-2 are misfiled and are promoted to Tier-1 in §1.5.**

---

## 0. Item register and tier verdicts

| # | Item | Backlog tier | **Verdict** | Why the tier moved |
|---|---|---|---|---|
| **T1** | Org-scope `driver:location:*` (+ `shift:updates:*`) channels | 2 | **2** (confirmed) | D2 closes the *read* at the authorization boundary; the channel name is then defense-in-depth. Needs a store release → genuinely Tier-2. |
| **T2** | Delete the `websocket` package + 40 `BroadcastToRole` sites | 2 | **2, last** | Zero tenancy content. Wide mechanical diff that collides with the same handler files D1 edits. |
| **T3a** | Redis `KEYS` → `SCAN` | 2 | **→ TIER-1, inside D3** | D3 ships the per-org loop as the *permanent* shape, so the blocking op's multiplier becomes the tenant count. Same function, same deploy, ~10 lines. §1.5.1. |
| **T3b** | Per-org driver index set (avoid keyspace iteration entirely) | — | **2, conditional** | Only if the 30 s sweep shows up in latency. D3's prefix leaves room. §2.3. |
| **T4a** | `resolveAirtagOrg` cache bypasses the ambiguity guard | 2 | **→ TIER-1** | Silent cross-tenant **write** for ≤1 h after any org/bin change; the guard is the only defense on this path. §1.5.2. |
| **T4b** | `airtag_monitor` bridge fallback pulls the global fleet | 2 | **→ TIER-1, highest of the three** | Fires **automatically ~3 min after org #2 is provisioned**, with no log. §1.5.2. |
| **T4c** | `airtag_accounts.email` globally `UNIQUE` | 2 | **→ TIER-1 (bundle with T4a/b)** | One-line migration; cross-tenant existence oracle. Lower severity, but free once you are already in this file. |
| **T5** | Six workers' `orgdb.Passthrough` original receiver | 2 | **2, reframed** | Premise is wrong in the *safe* direction: passthrough under live RLS reads **zero rows**, not "unscoped". Real residue is one table. §2.5. |
| **T6** | `census_income_cache` has no RLS | 2 | **2, merged into T5** | Verified: the **only** table in `public` without RLS, and the only surface where T5 has teeth. No runtime write path exists. |
| **T7** | In-process caches with no org key (`roads/cache.go`, `optimizer.go`) | 2 | **2, downgraded** | Neither is a tenancy leak. `lastPositions` is an unbounded-map bug. §2.6. |
| **T8** | Per-org rate limits | 2 | **2** | Availability, not isolation. §2.7. |
| **T9** | Drop 4 `zz_backup_*` + 4 dead-but-populated tables | 2 | **2, ordered** | Irreversible and **silent** if the dump step is skipped. §2.8. |
| **T10** | Centrifugo `allowed_origins` + **floating Docker tag** | 2 | **tag pin → before D1 Deploy 2** | The floating tag can change channel/namespace semantics under the migration with no changelog. §1.5.3. |

---

## 1. Tier-1 compatibility

**Overall verdict: `TIER1_PLAN.md` is sound and should be implemented
essentially as written.** Its ordering (D2 → D3 → D4b → D4a → D1), its
dual-publish correction (§0.3), its org-in-the-middle channel form (§D1) and its
JWT-derived client org state (§0.4) are all right, and the last two are exactly
what makes Tier-2 cheap. Nothing in Tier-1 has to be unpicked.

Five things must change before implementation. Four are small; one (1.5) is a
retier. The checklist is §1.7.

### 1.1 Does `company:{orgID}:events` generalize to org-scoped driver channels?

**Yes, cleanly.** The rule Tier-1 establishes is *namespace first, org at
`parts[1]`, kind and resource after*. Extended:

| Family | Today | Tier-2 form | `parts[1]` under today's parser | Failure if a stale backend sees it |
|---|---|---|---|---|
| company | `company:events` | `company:{org}:events` | uuid ≠ `"events"` | falls to `unknown channel format` → **loud 403** |
| driver GPS | `driver:location:{id}` | `driver:{org}:location:{id}` | uuid ≠ `"location"` | same → **loud 403** |
| shift | `shift:updates:{id}` | `shift:{org}:updates:{id}` | uuid ≠ `"updates"` | same → **loud 403** |
| driver events | `driver:events:{id}` | *(no change needed — see below)* | — | — |
| manager notif | `manager:notifications:{id}` | *(no change needed)* | — | — |

Two useful consequences:

- The loud-not-silent property Tier-1 argues for `company` (`TIER1_PLAN.md`
  §D1, "Why `company:{orgID}:events` and not `company:events:{orgID}`") holds
  identically for `driver` and `shift`, for the same reason. The scheme is
  therefore *the* right choice, not merely a locally right one.
- Centrifugo namespace resolution is unaffected: the namespace is the substring
  before the first `:`, which stays `driver` / `shift` / `company`. The same
  unverified assumption as `TIER1_PLAN.md` "Could not verify" #1 applies, and it
  is discharged by the same test — one empirical check now covers both tiers.

**Scope reduction Tier-2 inherits for free:** `driver:events:{driverID}`
(`internal/handlers/centrifugo.go:296-301`, inside `case "driver"`) and
`manager:notifications:{managerID}` (`:311-317`) authorize on
`userID == resourceID` only. A v4 UUID is globally unique, so no foreign user can
ever satisfy that equality, in any tenant. **Those two families need no org
scoping for isolation** — only for naming consistency. Tier-2's *mandatory*
channel surface is `driver:location:*` and `shift:updates:*`; the other two are
optional and should be done in the same release only because the client literals
sit three lines apart.

### 1.2 Does the parser Tier-1 proposes generalize? — **No. This is the one change that matters.**

`TIER1_PLAN.md` §D1 "Subscribe parser" proposes replacing *the body of
`case "company"`* with an inline two-form parse. The outer parse is untouched:

```go
// internal/handlers/centrifugo.go:280-286  (unchanged by Tier-1 as written)
parts := strings.Split(channel, ":")
...
namespace := parts[0]    // driver, shift, manager      ← :285
channelType := parts[1]  // location, updates, notifications  ← :286
```

The `channelType := parts[1]` binding is the problem. Once *any* family carries
an org at `parts[1]`, that variable no longer means what its name says, and the
`switch` has to re-parse per case. Tier-1 as written therefore leaves:

- one ad-hoc two-form parse inside `case "company"` (`:319-333`),
- an outer positional binding that is now a lie,
- and a second, third and fourth ad-hoc two-form parse for Tier-2 to add in
  `case "driver"` and `case "shift"` — **in two files**, because
  `authorizePublication` (`:469-498`, with the same `parts[0]`/`parts[1]`
  bindings at `:476-477`) repeats the identical positional destructuring, and
  `CentrifugoLocationPublishProxy`
  (`internal/handlers/centrifugo_location_proxy.go:94-95`) hard-asserts
  `len(parts) != 3 || parts[0] != "driver" || parts[1] != "location"`.

That is 3 copies of the same two-form parse in 3 functions across 2 files, all of
which must agree — the exact drift class Tier-1 correctly designs *out* of D3
(one key builder, one matching extractor, "the current bug class becomes
unrepresentable", §D3 "Changes"). D1 should apply its own D3 discipline.

**Required change to Tier-1:** in D1, introduce one parser and route all three
call sites through it. Suggested shape, in `internal/handlers/centrifugo.go`
above `authorizeSubscription`:

```go
// parsedChannel is the single source of truth for channel shape. Legacy is the
// pre-tenancy flat form and is the only shape with an empty OrgID.
type parsedChannel struct {
    Namespace  string // "driver" | "shift" | "manager" | "company"
    OrgID      string // "" for the legacy flat form
    Kind       string // "location" | "updates" | "notifications" | "events"
    ResourceID string // "" for company
    Legacy     bool
}

// Accepts, per family:
//   scoped: {ns}:{orgUUID}:{kind}[:{resourceID}]
//   legacy: {ns}:{kind}[:{resourceID}]
// Discriminated by whether parts[1] parses as a UUID — never by len(parts),
// because resourceIDs are UUIDs too and both forms can have the same length.
func parseChannel(channel string) (parsedChannel, error)
```

Discriminating on `uuid.Parse(parts[1])` rather than on `len(parts)` is the
load-bearing detail: `driver:{org}:location:{id}` (4 parts) and a hypothetical
legacy 4-part form are length-ambiguous, and `google/uuid` is already a direct
dependency (`internal/orgdb/orgdb.go:29`).

Cost of doing this in D1: ~40 lines and it *simplifies* the `case "company"`
diff Tier-1 already planned. Cost of not doing it: Tier-2 rewrites all four
`switch` cases plus two more functions, and the migration windows of D1 and T1
each need their own bespoke legacy branch instead of one shared `Legacy` flag.

**Everything else about D1's parser plan is correct and should be kept**, in
particular that the legacy branch is the isolation hole and D1 is not complete
until Deploy 3 removes it.

### 1.2b `PublishToChannel` is a compile-time-invisible escape hatch — delete it in D1

Tier-1's central safety argument for the publisher is structural: *"Signature
change is the point: it breaks compilation at all 36 sites, so no site can be
silently missed"* (§D1 "Changes"). That guarantee has a hole.

`internal/services/centrifugo/client.go:114` is
`PublishToChannel(ctx, channel string, data interface{})` — a raw escape hatch
that takes an arbitrary channel string. It has **4 callers, and all four build a
channel by hand with `fmt.Sprintf`:**

- `internal/handlers/shift_optimization.go:1605` → `shift:updates:%s`
- `internal/handlers/shift_tasks_edit.go:330` → `shift:updates:%s`
- `internal/handlers/shift_tasks_edit.go:1289` (`oldDriverChannel`) → `shift:updates:%s`
- `internal/handlers/shift_tasks_edit.go:1302` (`newDriverChannel`) → `shift:updates:%s`

**Good news for Tier-1:** none of the four builds a `company:` channel, so D1's
36-site count is complete and its compile-break guarantee holds *today*.
Verified.

**Bad news for Tier-2, and for the guarantee itself:** all four build
`shift:updates:` — one of the two families T1 must scope. When
`PublishShiftUpdate` (`client.go:90-91`) gains an `orgID` parameter, these four
sites **keep compiling** and keep publishing to the flat channel. So T1's
publish surface is 19 sites (13 `PublishShiftUpdate` + 4 hand-built + 2
`PublishDriverLocation`), of which 4 are invisible to the compiler. That is the
same silent-miss class D1 designed its signature change to prevent.

**Required change to Tier-1:** delete `PublishToChannel` in D1 and convert its
four callers to `PublishShiftUpdate`. All four build byte-identical strings to
what the typed helper builds, so it is a mechanical, behavior-preserving change
that costs nothing now — and it makes the compile-break guarantee *structural*
rather than *currently true*, which is precisely what T1 needs to rely on later.
Do it in D1, not T1: T1 is the deploy that needs the guarantee, so the guarantee
must predate it.

### 1.3 Does the Redis key scheme leave room for SCAN and coexist with Centrifugo's keys?

**Yes to both, with one caveat Tier-1 should state.**

*Coexistence.* `ropacal:org:{orgID}:driver:{id}:location` keeps the `ropacal:`
prefix, which is the only thing separating the two key families in the shared
instance (`TIER1_PLAN.md` §0.1). Centrifugo's keys are `centrifugo.stream.meta.*`
— dot-separated under a distinct prefix, so no pattern in either family can match
the other. Verified disjoint. Nothing in Tier-2 needs to change here.

*Room for SCAN.* Yes — `SCAN … MATCH ropacal:org:{orgID}:driver:*:location` is a
drop-in for `KEYS` with the same pattern, and `TIER1_PLAN.md` §3.4 already
recommends folding it in. §1.5.1 argues it must be mandatory, not recommended.

*The caveat Tier-1 omits.* `SCAN` removes the **block**, not the **work**:
`MATCH` is applied server-side to each returned batch, so the cursor still walks
the entire keyspace. The keyspace is dominated by Centrifugo — 142 of 153 keys
are `centrifugo.stream.meta.shift:updates:*` (§0.1) — and that term grows with
*Centrifugo's* load, not with our driver count. So after D3 the 30 s sweep is
`O(Centrifugo keyspace) × orgs`, non-blocking but not free.

The prefix scheme leaves the fix available without another rename: a per-org
index at `ropacal:org:{orgID}:drivers` (a `SET` of driver ids, or a `HASH`) turns
the sweep into `SMEMBERS` + `MGET` with no keyspace iteration at all. **Do not
build it in Tier-1** — it adds a second write per fix and an invalidation
question, and there is no evidence yet that the sweep matters. Filed as T3b
(§2.3), conditional on measurement. The point for Job 2 is only that D3's key
shape does not foreclose it.

### 1.4 Does D2's authorization work compose with per-org driver channels?

**Yes — additively, and D2 stays load-bearing after T1. It is not rewritten.**

After T1 there are three org values in play at a subscribe:

| Value | Source | Trust |
|---|---|---|
| channel org (`parts[1]`) | the **client's** channel string | attacker-controlled text |
| `db.OrgID()` | connection-token `meta`, via `proxyOrgDB` (`centrifugo.go:155-183`) | server-minted |
| `target.OrgID` | a `users` / `shifts` row read through the org-bound handle | DB fact |

T1 adds exactly one comparison — `parsed.OrgID != db.OrgID()` — as an early
reject. D2's `target.OrgID != db.OrgID()` (`TIER1_PLAN.md` §D2 "Changes") is
untouched and still necessary, because the channel comparison only proves the
*client asked politely*; the row comparison proves the *target actually belongs
to the tenant*. They fail on different mistakes.

Two composition notes for whoever implements T1:

- **Do not "simplify" D2 away as redundant once channels are org-scoped.** That
  is the single most likely regression in Tier-2. `driver:events:*` and
  `manager:notifications:*` are staying unscoped by design (§1.1), and any future
  channel family added without an org segment would land on a role-only check
  again. Add a comment at `canViewDriverLocation` saying so.
- **D2's `db.OrgID() != ""` dev guard must not be copied to the channel-org
  comparison as written.** With a passthrough handle, `db.OrgID() == ""`, so
  `parsed.OrgID != db.OrgID()` denies *everything* — safe in production
  (unreachable: `proxyOrgDB` only returns passthrough when
  `!orgdb.Migrated()`), but it breaks every local realtime test. T1 must skip
  the channel-org check under passthrough, exactly as D2 skips the row check.

### 1.5 Misfiled: three deferrals that bite the day org #2 exists

#### 1.5.1 `KEYS` → `SCAN` belongs in Tier-1 (inside D3), not as a recommendation

`TIER1_PLAN.md` §3.4 gets the analysis right and then hedges: *"Separable if you
want a minimal diff."* It should not be separable. Three reasons:

1. **D3 is what makes the problem scale with tenants.** `KEYS` is called from
   `internal/services/location_batch_writer.go:82`, inside the
   `orgdb.ForEachActiveOrg` loop at `:68`. D3 ships that loop as the permanent
   shape and gives each pass its own prefix — so post-D3 the number of blocking
   full-keyspace commands per 30 s *is* the tenant count. Shipping D3 without
   SCAN means the one thing tenancy adds becomes the multiplier on the one
   blocking command aimed at the live message broker.
2. **It is free here and never again.** `GetAllDriverLocations`
   (`internal/services/redis/client.go:67-95`) is being rewritten wholesale, the
   pattern string is moving anyway, and all three read-all callers are already
   being edited for the signature change. Doing it later costs a second visit to
   the same function plus a second Redis gate.
3. **Shared fate with Centrifugo is a Tier-1-shaped risk.** `KEYS` is O(keyspace)
   and single-threaded; the keyspace is 93 % Centrifugo's stream metadata. A busy
   day for Centrifugo makes *our* worker stall *its* pub/sub. That is not a
   "later" property.

**Change to Tier-1:** move §3.4 from "Recommended" into D3's required changes,
and add its gate (below) to D3's gate list.

#### 1.5.2 The AirTag org resolver is exploitable — one half of it fires by itself

Verified in `internal/handlers/airtag_org.go` and
`internal/services/airtag_monitor.go`. Both halves are silent.

**(a) `resolveAirtagOrg` cache short-circuits the ambiguity guard.**
`airtag_org.go:96-102` returns the cached org **before** any probe runs. The
ambiguity guard — `switch len(owners) { … default: log + fail closed }` at
`:126-136` — is the only cross-tenant defense on this path and it executes
**only on a miss**. The cache is a process-global `map[string]airtagOrgEntry`
(`:43-46`) with a 1 h TTL (`:36`), no invalidation hook, and no dependency on the
org set.

The probe that gets skipped is the one that can change answer:
`airtagLocationProbe` (`:143-152`) claims ownership if the org has a
`bins` row with the reported `bin_number` — and `bin_number` is **not unique**
(prod holds duplicates 56, 86, 116; `TENANCY_BACKLOG.md` records this, and the
comment at `airtag_monitor.go:141-144` acknowledges it). So: tag `T` resolves to
org A while A is the only claimant → cached under `airtag_loc:T`
(`airtag_locations_internal.go:79`) → org B later provisions the colliding bin →
for up to an hour, `T`'s GPS is written into **A's** `airtag_locations` scope with
no log line at all. The rows persist after the window closes, and A's drift
monitor then alerts on them.

*Fix (~6 lines, matches an idiom already used twice in this codebase):* skip the
cache entirely while more than one organization is active — the same org-count
gate as `soleActiveOrgID` (`handlers/centrifugo.go:137-152`) and
`CentrifugoProxyAuth`'s `isMultiTenant` (`middleware/centrifugo_proxy.go:70`).
Steady-state cost at 2 orgs is 2 `EXISTS` queries on indexed columns per bridge
row — `idx_airtag_locations_org_binnum` and `idx_airtag_keys_org` both exist.
Alternative, if the probe cost ever matters: keep the cache but store the org set
generation alongside and invalidate on change. The org-count gate is strictly
simpler and consistent with the rest of the system.

**(b) `airtag_monitor.fetchAirtagLocations` falls back to the global fleet on an
EMPTY read — silently. This is the worse of the two.**

```go
// internal/services/airtag_monitor.go:345-364
entries, err := GetAirtagLocationsFromDB(m.db)
if err == nil && len(entries) > 0 { return entries, nil }
if err != nil { log.Printf("⚠️  … DB read failed, falling back to bridge: %v", err) }
if m.bridgeURL == "" { return nil, … }
resp, err := FetchAirtagLocations(m.bridgeURL)   // ← the WHOLE FindMy account
return resp.Data, nil
```

`err == nil && len(entries) == 0` takes the fallback **without logging** — the
warning only fires on the error branch. And a newly provisioned organization has
zero `airtag_locations` rows *by definition*. So from the first sweep after org
#2 is created — the monitor ticks every 3 minutes (`:107`) and runs once
immediately at boot (`:120`) — org #2's drift sweep receives org #1's entire
AirTag fleet (72 `airtag_locations`, 76 `airtag_keys` today) and joins it against
org #2's bins **by `bin_number`**, which is precisely the non-unique key the
function's own doc comment warns about. Output is false drift alerts and FCM
pushes to org #2's admins carrying org #1's positions, street names and cities.
No attacker required. Fires by itself.

*Fix (2 lines):* treat a successful empty read as authoritative —
`if err == nil { return entries, nil }` — and gate the error-path fallback on
`!orgdb.Migrated()` (or the single-org count), because a global-fleet fetch has
no meaning once tenancy is live.

**(c) `airtag_accounts.email` is globally unique.** Verified live:
`airtag_accounts_email_key UNIQUE (email)` exists alongside
`uq_airtag_accounts_org_id UNIQUE (organization_id, id)`. An insert attempt
against an RLS-invisible row returns `23505`, which is a cross-tenant existence
oracle. Reachable only by an `INTERNAL_API_KEY` holder (the FindMy bridge), so
severity is medium-low — but the fix is one migration
(`DROP CONSTRAINT airtag_accounts_email_key; ADD CONSTRAINT … UNIQUE
(organization_id, email)`) and you are already editing this file for (a).

**Net verdict on the resolver cache: Tier-1.** Not because an attacker can reach
it, but because Tier-1's definition is "must be true before a second
organization exists", and (b) violates that automatically, three minutes after
provisioning, with no log. Ship (a), (b) and (c) as one commit alongside D2/D3.

**Gate for D5 — negative, provable with one org.** (a) Temporarily lower
`airtagOrgCacheTTL` and assert via log that a second bridge POST for the same tag
re-probes rather than reusing the cache; then assert the ambiguity path still
fires by inserting a second bin with a colliding `bin_number` **inside the same
org** — the probe's third leg (`airtag_org.go:149`) is an `EXISTS`, so an
intra-org duplicate does *not* trigger ambiguity, which is itself worth
confirming as the expected behavior rather than assuming it. (b) Point
`FINDMY_BRIDGE_URL` at a stub and assert that a successful **empty** DB read no
longer reaches it — this is the assertion that catches the silent branch. (c)
`INSERT` a duplicate email for a second org id in a scratch database and assert
no `23505`. **Positive:** the existing ropacal org's 72 `airtag_locations` rows
keep updating from the real bridge on the normal 3-minute cadence, and a real
drift alert still fires for a bin that has genuinely moved — the failure mode of
over-tightening here is a monitor that silently stops alerting.

**Rollback for D5.** (a) and (b) are pure Go, one file each, single-commit
revert. (c) is a migration: the reverse is
`DROP CONSTRAINT uq_airtag_accounts_org_email; ADD CONSTRAINT
airtag_accounts_email_key UNIQUE (email)`, which will **fail loudly** if any
duplicate email was created in the meantime — acceptable, and better than a
reverse migration that silently succeeds by dropping rows. With 3 rows in the
table today the risk is negligible.

#### 1.5.3 Pin the Centrifugo image tag before D1 Deploy 2

Not a security item — an ordering one. The Centrifugo service runs a **floating**
Docker tag and has already self-upgraded 6.6.0 → 6.9.1 on an unrelated redeploy.
`TIER1_PLAN.md`'s single unverified structural assumption ("Could not verify" #1:
that a 3-segment channel resolves to the `company` namespace and therefore still
hits `subscribe_proxy_enabled`) is a property of the running binary. A silent
minor-version bump between Deploy 1 and Deploy 3 can move it with no changelog
anyone reads.

**Change to Tier-1:** pin the image to an explicit version as a prerequisite of
D1 Deploy 2, in the same config visit that adds the proxy secret (Phase A). It
costs one field and it converts "we verified this once" into "this stays
verified".

### 1.6 Is anything in Tier-1 premature or wasted given Tier-2's shape?

Very little. Four notes, in descending importance:

1. **The inline `case "company"` parse is the only genuinely wasted work** — it
   will be rewritten by T1. §1.2 replaces it with something Tier-2 extends
   instead of replaces. Everything else in D1 survives verbatim.
2. **D1's `orgID == ""` → publish-legacy-only fallback becomes a silent
   event-loss path after Deploy 3.** `TIER1_PLAN.md` §D1 "Changes" guards
   `orgID == ""` by falling back to `company:events`, for non-migrated dev
   databases. Correct through Deploys 1–2. After Deploy 3 nothing subscribes to
   `company:events`, so the same fallback silently drops the event. Make it
   `if orgID == "" { if !orgdb.Migrated() { legacy } else { log.Printf("🚨 …
   PublishCompanyEvent with empty orgID"); return error } }`. Same treatment for
   D3's `driverLocationKey("")` — under live tenancy an empty org can only mean a
   passthrough handle reached the writer, which is a bug, and writing a flat key
   hides it behind a reader that no longer looks there.
3. **D3's decision not to implement read-both is right and should be kept.**
   Tier-2 confirms the reasoning from the other end: T1's transition needs a
   *dual-subscribe* window on the Centrifugo side (§2.1), and adding a
   Redis-side read-both on top of that would give two independent legacy paths to
   remember to delete. One is enough.
4. **D4a (24 h → 1 h connection tokens) interacts mildly with T1, in the helpful
   direction.** T1 changes only channel *names*; a shorter token TTL means stale
   subscriptions are re-authorized sooner, so the T1 legacy branch retires
   faster. Nothing wasted.

Also worth recording: Tier-1's §0.4 insistence on **JWT-derived** client org
state (not login-response-derived) is what makes T1's client work a two-line
change instead of another migration. That decision is load-bearing for Tier-2 and
must not be softened.

### 1.7 Required changes to `TIER1_PLAN.md` before implementation

| # | Change | Where | Why |
|---|---|---|---|
| 1 | Extract `parseChannel` and route `authorizeSubscription`, `authorizePublication` and `CentrifugoLocationPublishProxy` through it. Discriminate on `uuid.Parse(parts[1])`, not `len(parts)`. | §D1 "Subscribe parser" | §1.2 — otherwise Tier-2 rewrites 4 switch cases in 2 files |
| 2 | Promote `KEYS`→`SCAN` from "Recommended" to required inside D3, and add its gate. | §3.4, §D3 gates | §1.5.1 |
| 3 | Add an item **D5 — AirTag tenancy** covering the cache-vs-guard bypass, the empty-read bridge fallback and the `email` unique index. | new | §1.5.2 |
| 4 | Make `orgID == ""` loud under live tenancy in both `PublishCompanyEvent` and `driverLocationKey`, instead of silently falling back to the legacy shape. | §D1, §D3 | §1.6.2 |
| 5 | Add "pin the Centrifugo image tag" to D1 Deploy 2's prerequisites. | §D1 deploy sequence | §1.5.3 |
| 6 | Delete `PublishToChannel` (`services/centrifugo/client.go:114`); convert its 4 callers to `PublishShiftUpdate`. | §D1 "Changes" | §1.2b — otherwise D1's compile-break guarantee is true by luck, and T1 silently misses 4 sites |

Everything else in `TIER1_PLAN.md` should ship unchanged.

---

## 2. Tier-2 items

### 2.1 T1 — Org-scope `driver:location:*` and `shift:updates:*`

**Why it is Tier-2 and not Tier-1.** With D2 shipped, a foreign admin
subscribing to `driver:location:{victim}` is denied by
`canViewDriverLocation`'s target-org check before Centrifugo ever delivers the
`force_recovery: "cache"` last-known fix. The channel rename is defense in depth
plus operational hygiene (a flat channel name is a foot-gun for the next feature
that authorizes by role). It requires a coordinated store release, so it waits.

**Why it is not merely cosmetic.** The `driver` namespace has
`presence: true`, `history_size: 10`, `history_ttl: 300s` and
`force_recovery_mode: "cache"`. Presence and history are per-*channel*, not
per-namespace — the live Redis confirms this
(`centrifugo.stream.meta.driver:location:{UUID}`, one key per driver) — so the
exposure is not a shared bucket. It is that **the channel name is a bare,
enumerable identifier with no tenant in it**, which makes every channel-keyed
Centrifugo surface (presence, `presence_stats`, `history`, and the
`force_recovery: "cache"` last-value replay) reachable by anyone who knows a
driver UUID. Today the *only* thing standing between a foreign admin and those
surfaces is the subscribe-proxy authorization decision — i.e. D2. Putting the org
in the name means the channel a foreign tenant would have to name does not exist,
which is a structurally stronger position than "one function returns false".
Nothing reads presence today, and "nothing reads it yet" is exactly the argument
for scoping the name before something does.

**The migration is shaped differently from D1 and this is the crux.** For
`company:events` the backend publishes and clients subscribe, so the backend can
dual-publish. For `driver:location` **the mobile app is the publisher** and the
dashboard plus the backend proxy are the consumers. The backend cannot
dual-publish something it does not originate, and the dashboard cannot know
which form a given driver's build is using.

Resolution: **the dashboard dual-subscribes** during the window (old form and
new form, deduped by driver id), the app flips last, and the legacy form is then
dropped. This is safe *because D2 has already shipped* — the legacy branch is
org-checked at the row level, so leaving it open for one release is not an
isolation hole. (The alternative, having the location proxy mirror legacy
publishes onto the new channel via the Centrifugo HTTP API, costs one extra
publish per GPS fix per driver at 1–3 s intervals; keep it in reserve only if
dual-subscribe proves awkward in `use-active-drivers.ts`.)

**Files and functions.**

Backend:
- `internal/handlers/centrifugo.go` — `authorizeSubscription` `case "driver"`
  (`:289-301`) and `case "shift"` (`:303-309`): accept both forms via
  `parseChannel` (§1.2), and when `parsed.OrgID != ""` require
  `parsed.OrgID == db.OrgID()`.
- `internal/handlers/centrifugo.go` — `authorizePublication` `case "driver"`
  (`:480-486`): same, keeping `userID == driverID`.
- `internal/handlers/centrifugo_location_proxy.go:94-95` — replace the hard
  `len(parts) != 3 || parts[0] != "driver" || parts[1] != "location"` assert with
  `parseChannel`; `driverID` then comes from `parsed.ResourceID` (today
  `driverID := parts[2]` at `:107`, consumed by the Redis write at `:196`).
- `internal/services/centrifugo/client.go` — `PublishDriverLocation` (`:61-62`)
  and `PublishShiftUpdate` (`:90-91`) take `orgID` and build the scoped channel,
  same signature-change discipline as D1's `PublishCompanyEvent`. **19 publish
  call sites**: 13 `PublishShiftUpdate`, 2 `PublishDriverLocation`
  (`driver_location.go:154`, `:618`), and 4 hand-built `shift:updates:` strings
  routed through `PublishToChannel` — **which will not break at compile time**
  unless §1.2b deleted that helper in D1. If §1.2b was skipped, grep for
  `fmt.Sprintf("shift:updates` and `fmt.Sprintf("driver:location` before
  shipping; it is the only defense left.
- `internal/services/centrifugo/client.go:159-160` (`PublishDriverEvent`, 6 call
  sites) and `:133-134` (`PublishManagerNotification`, 0 call sites) need no
  change — those two families stay unscoped by design (§1.1).

Dashboard (`binly-dashboard`):
- `lib/hooks/use-active-drivers.ts:100` (subscribe) and `:113` (unsubscribe
  channel string) — the only two `driver:location` literals in the repo.
  Dual-subscribe here, keyed off the `orgId` the D1 client work already added.

Flutter (`ropacalapp`) — five literals, all reachable from one helper each:
- `lib/core/services/location_tracking_service.dart:577` (publish; log at `:563`)
- `lib/core/services/centrifugo_service.dart:183` (`driver:location:` subscribe)
- `lib/core/services/centrifugo_service.dart:244` (`shift:updates:`)
- `lib/features/manager/manager_map_page.dart:324` (unsubscribe string)
- `lib/providers/shift_provider.dart:161` is a log line only — update for
  accuracy, no behavior.

**Deploy sequence.**

| # | Ships | Position rationale |
|---|---|---|
| 1 | backend: all three parse sites accept both forms | Inert — nothing publishes or subscribes to the new form yet. Independently revertible. |
| 2 | Centrifugo: image tag pinned (if not already done for D1) | Freezes namespace/history semantics for the rest of the migration. |
| 3 | dashboard: dual-subscribe old + new | Zero-risk: it sees whichever form each app build publishes. Instant rollback. |
| 4 | Flutter: publish and subscribe the new form only | The irreversible step. Requires 1 and 3. |
| 5 | backend: drop the legacy branch from all three parse sites; dashboard drops the legacy subscribe | Gated on adoption evidence, not a calendar. |

**Adoption evidence for Deploy 5.** Same instrument as D1: the subscribe proxy
logs `channel=%s` on every subscribe (`centrifugo.go:216`) and the location proxy
sees every GPS publish. Require a full business day with zero
`channel=driver:location:` (3-part) lines and non-zero
`channel=driver:<uuid>:location:` lines. Unlike D1, presence is available here
(`driver` has `presence: true`), so Centrifugo's `presence_stats` on the legacy
channel is a second, independent signal — use both.

**What breaks if the order is violated.**

| Violation | Presentation |
|---|---|
| Flutter (4) before backend (1) | **LOUD.** Publish is rejected at the location proxy with `invalid channel format` (400) and subscribe 403s; the app logs from `subscription.error.listen` (`centrifugo_service.dart:377`). But a shipped build cannot be recalled — this is the one ordering error with no fast fix, which is why 1 must be days ahead. |
| Flutter (4) before dashboard (3) | **SILENT on the dashboard only.** Drivers publish to the new channel; the dashboard is still subscribed to the old one and every marker freezes while the map renders normally and `CentrifugoStatus` stays `connected`. GPS still reaches Redis (the proxy accepts both after Deploy 1), so `/api/manager/drivers` is *correct* — the map and the API disagree, which is the tell. |
| Legacy branch dropped (5) before adoption | **SEMI-LOUD.** Stale builds' GPS publishes 400 at the proxy — logged loudly server-side — but the driver-visible symptom is only that tracking stopped. Worse than D1's equivalent because it breaks the *driver's* core function, not a manager's view. Do not compress this gate. |
| Location proxy left on the 3-part assert while subscribe accepts both | **SILENT AND WORST.** Subscriptions succeed on the new channel, so the dashboard looks alive, but the proxy rejects the publish → no Redis write → no `driver_current_location` row → `resolveDriverStartLocation` (`driver_location.go:232-249`) degrades to its stale durable fallback hours later, and the warehouse-proximity auto-end never arms. Prevented by routing all three sites through one `parseChannel` (§1.2). |

**Gate — negative.** Provable with one org, exactly as D2's is:

```
POST /api/centrifugo/subscribe
{"user":"<real admin id>",
 "channel":"driver:11111111-1111-1111-1111-111111111111:location:<real driver id>",
 "meta":{"org_id":"00000000-0000-0000-0000-000000000001"}}
```
must 403 with the cross-org log line, because the channel org ≠ the meta org.
Repeat with the ids swapped, and repeat for
`shift:11111111-…:updates:<real shift id>`. After Deploy 5, add: the 3-part
`driver:location:<real driver id>` must **also** 403.

**Gate — positive.** Three assertions, because "authorized" is not the property
under test:
1. A driver on an active shift publishes and
   `redis-cli --scan --pattern 'ropacal:org:<orgA>:driver:*'` is non-empty and
   the value's timestamp advances.
2. The dashboard fleet map marker **moves** (not merely renders).
3. `SELECT driver_id, updated_at FROM driver_current_location ORDER BY updated_at
   DESC LIMIT 5` advances within 60 s — proves the proxy → Redis → batch-writer
   chain survived the channel rename.
Plus: the Flutter manager map (`manager_map_page.dart:235`) still tracks a
driver, and a driver subscribing to their **own** location channel still
authorizes.

**Rollback.** Deploys 1, 3 and 5 are single-commit reverts. Deploy 4 is not
recallable; its safety net is that Deploy 1's both-forms parser and Deploy 3's
dual-subscribe keep a new build fully functional against a backend rolled back to
Deploy 1. Keep Deploy 5 as its own commit touching only the three parse sites and
the dashboard's legacy subscribe.

### 2.2 T2 — Delete the `websocket` package (do this LAST)

**Scope is larger than the backlog implies.** Verified: 40 `BroadcastToRole`
call sites across 11 files (`internal/handlers/shifts.go` 11,
`potential_locations.go` 6, `shift_cancel.go` 6, `shift_complete.go` 4,
`bins.go` 3, `config.go` 2, `route_tasks.go` 2, `driver_location.go` 1,
`test_osrm.go` 1, plus 4 inside the package itself), and `wsHub` appears **31
times in `cmd/server/main.go`** because it is a constructor parameter on ~30
handlers. The package is 3 files (`client.go`, `handler.go`, `hub.go`).

So the diff is: delete `internal/websocket/`, remove the `wsHub` parameter from
~30 handler constructors and their 31 registration sites, delete 40 call sites,
and remove `wsHub := websocket.NewHub()` / `go wsHub.Run()`
(`cmd/server/main.go:258-259`) and the commented-out route at `:342`.

**Order: after T1, after D1.** Not for correctness — because it touches
`shifts.go`, `shift_complete.go`, `driver_location.go` and `bins.go`, which are
also where D1's 36 `PublishCompanyEvent` sites and T1's publish changes live. A
30-signature refactor interleaved with a security migration turns every merge
into a review of the wrong thing.

**What breaks if it goes early.** Nothing at runtime — `/ws` has been closed
since 2026-07-29, so all 40 broadcasts are already no-ops. That is the useful
framing: **anything that appears to break was already broken and invisible.**
Deleting makes it visible. The one real risk is reviewer fatigue on a
1000-line mechanical diff shipped next to a security change.

**Gate — negative.** `grep -rn "BroadcastToRole\|websocket\." --include='*.go' .`
returns zero outside `go.sum`; `go build ./...` succeeds; `go vet ./...` reports
no unused parameters. **Gate — positive.** Full smoke: create a bin, start a
shift, complete a task, cancel a move, end a shift — each must still produce its
Centrifugo `company:{org}:events` publication and its FCM push. Watch for the
one class of latent bug this can expose: a handler where the `wsHub` argument was
the only reason a constructor had a non-nil dependency.

**Rollback.** Single revert. Keep it in exactly one commit with no other content.

### 2.3 T3b — Per-org driver index (conditional)

Only if measurement says so. See §1.3: after D3+SCAN the 30 s sweep is
non-blocking but still walks a keyspace dominated by Centrifugo's stream
metadata. Trigger to build: Redis `INFO commandstats` shows the sweep's
`usec_per_call` growing with shift volume, or `slowlog` picks it up.

Shape: `SADD ropacal:org:{orgID}:drivers {driverID}` on each save; the read-all
becomes `SMEMBERS` + `MGET`. The set needs its own expiry discipline since
`MGET` will return `nil` for expired location keys — treat a `nil` as "prune this
member", which makes the set self-healing and needs no invalidation hook.

**Gate.** Negative: the set for a foreign org id is empty. Positive:
`GetOrgDriverLocations` returns byte-identical output to the SCAN
implementation for the same live state (run both, diff) before the SCAN path is
removed.

**Rollback.** Keep the SCAN implementation behind a boolean for one release so a
revert is a config flip, not a redeploy; the index sets then expire or are
`DEL`eted with a single prefix sweep. Do not delete the SCAN path until the index
has survived a full week of live shifts.

### 2.4 T4 residue — AirTag items *not* promoted

§1.5.2 moves (a) the cache/guard bypass, (b) the bridge fallback and (c) the
`email` index to Tier-1. What stays in Tier-2:

- **Namespace `airtag_locations` on `(organization_id, bin_number)` rather than
  resolving by `bin_number`.** The probe's third leg
  (`airtag_org.go:149`) is what makes ownership ambiguous at all. Removing the
  need for it means the bridge must carry an org hint — a real API change to a
  separate service. Genuinely Tier-2.
- **`airtag_accounts.id` / `airtag_locations.id` / `airtag_keys.id` remain
  globally unique primary keys.** Verified. These are externally-issued Apple
  identifiers, so global uniqueness is arguably correct; two tenants sharing one
  iCloud account is a business question, not a bug. Record the decision, do not
  change it silently.

### 2.5 T5 + T6 — the worker passthrough hazard, correctly scoped

**The backlog's premise is wrong in the safe direction, and the correction
changes the fix.** The claim is that a method called on the original receiver
"reads unscoped". It does not. Verified in `internal/orgdb/orgdb.go`:

- A passthrough handle bypasses the transaction wrapper entirely and calls
  `d.root` directly (`:135`, `:151`, and the `if d.passthrough` early return in
  every method).
- So no `set_config('app.org_id', …)` runs.
- The RLS policy is `organization_id = NULLIF(current_setting('app.org_id',
  true), '')`, which with the GUC unset evaluates to `organization_id = NULL` →
  `NULL` → **no rows**.
- `binly_app` is non-superuser with `FORCE ROW LEVEL SECURITY` on 40 tables, so
  this holds for writes too (and `organization_id` is `NOT NULL` on 35 tables, so
  an insert fails loudly on top).
- The org-scoped GUC is set with `set_config(…, is_local => true)` inside a
  transaction (`:137`, `:153`), so it **cannot** persist onto a pooled
  connection. The classic "stale GUC on a recycled connection" leak does not
  exist here. This is good design and worth not disturbing.

**Therefore an original-receiver call is a fail-closed availability bug — a
worker silently doing nothing — not a confidentiality leak.** With one
exception, and it is exactly the item the backlog files separately:

**`census_income_cache` is the only table in `public` without RLS.** Verified
live: `relrowsecurity = f`, `relforcerowsecurity = f`; every other table is
`t`/`t`. Primary key is `zip` alone; there is no `organization_id` column. So it
is the one surface a passthrough handle can read across tenants. Severity is
bounded by the fact that **no runtime write path exists** — all four Go
references are `SELECT` (`chat_tools.go:443`, `:461`, `chat_locations.go:614`,
`placement_opportunities.go:89`); the only write is the boot seed
(`database/database.go:1016`). It is shared *public* reference data (59 rows of
ZIP-level census income), so sharing it is correct; what is wrong is that it is
indistinguishable from a table where RLS was forgotten.

**Fix — one item, not two:**

1. Add an explicit allow-list assertion at boot, beside
   `database.AssertRLSEnforced` (`internal/database/tenancy.go:47`): enumerate
   `public` tables with `relrowsecurity = false` and fail unless every one is in
   a named `sharedReferenceTables` set containing exactly
   `census_income_cache`. This converts "we remembered" into "the process
   refuses to start if someone adds an unprotected table". It also makes the
   *next* shared-reference table a deliberate decision.
2. Add a doc comment on the table's DDL saying it is deliberately global, and
   that adding a write path requires either RLS or org-keyed rows.
3. Audit the six workers for original-receiver calls mechanically rather than by
   eye: make the original's `db` field a handle whose methods **panic** with a
   clear message when tenancy is live (`orgdb.Poisoned(root)` — same shape as
   `Passthrough`, `passthrough: true` swapped for a `poisoned` flag checked in
   `begin`). The shallow-copy rebind (`airtag_monitor.go:145-149` and the four
   siblings) already replaces `db` per pass, so a correct worker never touches
   it. This turns a silent no-op into a loud panic that `runForOrg`'s recover
   (`orgdb/state.go:151-160`) logs per org without stopping the loop.

**Gate — negative.** With a deliberately mis-wired local worker (call a method
on the original receiver), the boot assertion or the poisoned handle must produce
a log line naming the worker. Add a table with RLS disabled in a scratch DB and
confirm boot refuses. **Gate — positive.** All six workers complete a full pass
per org with their normal output; `census_income_cache` reads still return 59
rows through an org-bound handle (proving the allow-listed table stays readable,
which is the assertion the negative gate does not make).

**Rollback.** Additive and independent; revert either piece alone.

### 2.6 T7 — In-process caches: not a tenancy problem, but one real bug

Verified, and the tenancy framing does not hold for either:

- **`internal/services/roads/cache.go`** — keyed by `md5` of a coordinate
  sequence (`:54-77`), 24 h TTL, and it *does* have bounded growth:
  `evictOldest` (`:128`) and `cleanupExpired` (`:147`). The cached value is
  snapped road geometry — public data, identical for every tenant. Sharing it is
  correct and desirable. The only cross-tenant channel is a cache-timing
  inference (whether some tenant has snapped a particular coordinate sequence),
  which is not worth code. **No action.**
- **`internal/services/roads/optimizer.go`** — `lastPositions
  map[string]*LastPosition` keyed by driver UUID (`:14`, `:61`). Driver ids are
  v4 UUIDs, globally unique, so there is no cross-tenant collision. The real bug
  is unbounded growth: no TTL, no eviction, and `ClearDriver` (`:139-143`) has
  **zero callers**. Every driver who ever publishes leaves a permanent entry.
  Small (a few dozen bytes each) but monotonic across the process lifetime.

**Fix.** Either call `ClearDriver` where it was meant to be called — the shift-end
path (`handlers/shift_complete.go`) — or, better, give `ShouldProcessByDelta` a
staleness check that drops entries older than the Redis TTL (10 min) during its
existing map access, so no new call site is needed and no dead code survives.
Prefer the second: it cannot be forgotten by a future shift-end refactor.

**Gate.** Negative: after a driver's shift ends and 10+ minutes pass,
`GetStats()` (`:180`) reports a `lastPositions` size that has decreased.
Positive: mid-shift delta suppression still works — publish two fixes 1 m apart
within a second and confirm the second is counted as skipped-by-delta in
`GetStats`, i.e. the cleanup did not evict live entries.

**Rollback.** Single-commit revert of one file. Worst case of a bad staleness
threshold is that delta suppression stops suppressing — more OSRM calls and more
Redis writes, no correctness loss — so this is the lowest-risk item in the plan.

**Note for the record:** the backlog's observation that *"no cache in the
codebase has an invalidation hook"* is accurate and is the real lesson. Of the
four caches examined, three are safe for structural reasons (public data, or
globally unique keys) and exactly one — the AirTag org resolver — is unsafe
*because* it caches a tenant decision. The generalisation to draw is not "add
invalidation hooks everywhere"; it is **never cache a tenant resolution**, which
is what §1.5.2's org-count gate encodes.

### 2.7 T8 — Per-org rate limits

Availability, not isolation, and there is no rate limiting anywhere today. The
shared resources one tenant can exhaust for another:

- the single Postgres pool (`DB_MAX_OPEN_CONNS` default 20, and per `CLAUDE.md`
  this process owns the only pool against a ~100-connection Railway instance, no
  PgBouncer);
- paid third-party quota — HERE geocoding, OSRM, ESRI;
- OR-Tools optimization, which runs **in-process** and is CPU-bound, so one
  tenant's large re-optimize adds latency to every other tenant's request;
- Centrifugo API publish throughput.

Order: **after** the isolation work. A rate limiter that fires before isolation
is complete just adds a second explanation for "the dashboard is stale".

Shape: token bucket keyed on `orgdb.From(r).OrgID()`, applied as chi middleware
after `middleware.Org` (so the org is already resolved), with a separate and much
tighter bucket on the optimize endpoints. Return `429` with `Retry-After`; both
clients currently treat non-2xx generically, so confirm neither turns a 429 into
a logout — `binly-dashboard/lib/api/client.ts:63-80` redirects on 401 only,
which is correct, but Flutter's handler should be checked before shipping.

**Gate.** Negative: a scripted burst from org A's token gets 429s while a
concurrent normal request from org B's token succeeds within its usual latency
band — that concurrency is the assertion, not the 429 itself. Positive: a normal
admin session and a driver on shift (GPS every 1–3 s, which is the highest
legitimate rate in the system) never see a 429 over a full shift.

**Rollback.** Ship the limits as env-configurable with an off switch
(`RATE_LIMIT_ENABLED=false`) so a bad threshold is a variable change, not a
redeploy — this is the one Tier-2 item whose failure mode is denying *legitimate*
traffic, so a code-only rollback path is too slow.

### 2.8 T9 — Drop the dead tables (ordered; the first step is irreversible)

Verified live row counts:

| Table | Rows | Notes |
|---|---|---|
| `driver_location_history` | 94,647 | dead code path |
| `driver_locations` | 6,244 | dead; superseded by `driver_current_location` |
| `shift_bins` | 0 | superseded by `route_tasks` |
| `zone_risk_overrides` | 0 | |
| `zz_backup_overnight_20260710_bins` | 14 | 16 kB |
| `zz_backup_overnight_20260710_route_tasks` | 7 | 16 kB |
| `zz_backup_overnight2_20260710_bins` | 10 | 16 kB |
| `zz_backup_overnight2_20260710_route_tasks` | 5 | 16 kB |

**The `zz_backup_*` tables are 36 rows and 64 kB total, so this frees nothing —
the value is removing four tables that carry tenant-shaped data with no
`organization_id` and no RLS-visible owner.** But they are the only in-database
record of the pre-correction state of the 2026-07-10 overnight-shift fix.
`TENANCY_BACKLOG.md` notes the pre-migration `pg_dump` went to a session
scratchpad, i.e. it is probably gone.

**Order, and this is the whole item:**

1. `pg_dump` the eight tables to durable storage. Use the **17** client — the 15
   client silently produces a 0-byte file against this 17.7 server
   (`TENANCY_BACKLOG.md`), which is the single most dangerous silent failure in
   this plan because it makes step 2 unrecoverable while looking successful.
   **Verify the dump is non-empty and restores into a scratch database before
   proceeding.**
2. `ALTER TABLE … RENAME TO zzz_pending_drop_…` and leave for one full business
   cycle. A rename surfaces any hidden reader as a loud `relation does not
   exist` 500 rather than as a `DROP`-time surprise. (Note from prior
   experience: SQLite rewrites FKs on rename — Postgres does not, so the FK
   graph is preserved and the rename is genuinely reversible.)
3. `DROP TABLE`.

**What breaks if the order is violated.** Skipping step 1: **silent,
irreversible data loss** — nothing errors, the rows are simply gone and the
0-byte-dump failure mode means you may believe otherwise. Skipping step 2: a
`DROP` on a table with an undiscovered reader is a hard 500 at some later
moment, attributed to the wrong change.

**Gate.** Negative: `grep -rn` for each table name across
`ropacal-backend`, `binly-dashboard`, `ropacalapp` and `ropacal-placement`
returns nothing outside migrations and this document; after the rename, a full
business cycle produces zero `relation "…" does not exist` log lines. Positive:
the restore rehearsal in step 1 produces the same row counts as the table above.

**Rollback.** Between steps 2 and 3, `RENAME` back — instant and complete.
After step 3, restore from the step-1 dump into a scratch database and
`INSERT … SELECT` (a straight restore would recreate the tables without their
`organization_id`/RLS treatment, since these predate the migration). **After step
3 with no valid dump there is no rollback** — which is the entire reason step 1
is a gate and not a suggestion.

### 2.9 T10 — Centrifugo configuration hardening

Three separate things, of which one is on Tier-1's critical path.

1. **Pin the Docker image tag.** Promoted to a D1 prerequisite — §1.5.3.
2. **`client.allowed_origins: ["*"]`.** Real severity is lower than it looks:
   authentication is a Bearer token in `localStorage` (origin-scoped), not a
   cookie, so a hostile page cannot ride an existing session — it would need the
   token first. Set it to the real dashboard origin anyway, together with the
   backend's unset `ALLOWED_ORIGINS`. **The blocker is informational, not
   technical: the production dashboard domain is not recorded anywhere in the
   repos.** Establish it, then set both in one visit. Do not set only one:
   Centrifugo restricted while the backend stays `*` leaves the API open, and the
   reverse breaks the dashboard's WebSocket loudly.
3. **Disable or lock down anything else the config exposes.** The admin UI is
   already disabled as of 2026-07-30; record that and re-check after any image
   upgrade, since a floating tag can reintroduce defaults.

**Gate.** Negative: a `wscat`/browser connection from an unlisted `Origin` is
rejected by Centrifugo, and a `curl -H 'Origin: https://evil.example'` against
the backend gets no permissive `Access-Control-Allow-Origin`. Positive: the real
dashboard connects, subscribes and receives a `company:{org}:events` publication
from a live bin edit — run this immediately, because an origin misconfiguration
presents as a WebSocket that connects and then silently never subscribes.

**Rollback.** All three are config fields on two services: revert
`allowed_origins` to `["*"]` and unset `ALLOWED_ORIGINS`. Note the asymmetry —
the tag pin is the one change you should **never** roll back, because rolling it
back restores the floating tag and with it the possibility of an unannounced
semantics change mid-migration.

---

## 3. Consolidated deploy sequence

```
── Tier-1 (see TIER1_PLAN.md, with §1.7's five changes applied) ──
D2   org-equality in subscribe auth (+ parseChannel extracted)   ← unblocks T1's window
D3   Redis org-prefixed keys + SCAN (T3a folded in)
D5   AirTag tenancy: cache gate, bridge fallback, email index     ← new, from §1.5.2
D4b  cached membership check in middleware.Org
D4a  connection token 24h → 1h
D1   company:{org}:events   (3 deploys; Centrifugo tag pinned before Deploy 2)

── Tier-2 ──
T1   driver/shift channel org-scoping        (5 deploys, incl. a store release)
T10  allowed_origins + backend ALLOWED_ORIGINS
T5   boot allow-list assertion + poisoned worker handle
T7   lastPositions staleness eviction
T8   per-org rate limits
T9   dump → rename → drop the 8 dead tables
T2   delete the websocket package             ← LAST
T3b  per-org driver index                     ← only if measured
```

Hard constraints (everything else is preference):

- **D2 before T1.** D2 is what makes T1's dual-subscribe window safe; without it,
  the legacy `driver:location:{id}` branch T1 must keep open for one release is a
  live cross-tenant read.
- **D3 before T3b**, trivially, and D3+SCAN before any decision about T3b.
- **Centrifugo tag pinned before D1 Deploy 2 and before T1 Deploy 4.** Both
  migrations depend on namespace/history semantics staying put.
- **T2 last.** It edits the same handler files as D1 and T1.
- **T9 step 1 (verified dump) before T9 steps 2–3.** The only irreversible step
  in either tier.
- **D5 before org #2 is provisioned** — that is what makes it Tier-1.

---

## 4. Failure presentation, ranked by how hard it is to attribute

| Rank | Mistake | Loud or silent | Why it ranks here |
|---|---|---|---|
| 1 | T9 dump skipped, or taken with the `pg_dump` 15 client | **SILENT**, irreversible | 0-byte dump looks like success; the rows are gone with no error at any point |
| 2 | T1: location proxy left on the 3-part assert while subscribe accepts both | **SILENT** | Subscriptions succeed → dashboard looks alive; no Redis write → `driver_current_location` stalls → preflight/StartShift degrade **hours later**, far from the cause |
| 2= | T1: the 4 hand-built `shift:updates:` publishes missed because they do not break compilation (§1.2b) | **SILENT** | An upgraded app subscribed to `shift:{org}:updates:{id}` receives nothing from those four paths — mid-shift task edits, reassignments and re-optimizations stop arriving while everything else works. Removed as a possibility if D1 deletes `PublishToChannel` |
| 3 | D5 not shipped before org #2 (`airtag_monitor` empty-read fallback) | **SILENT** | No log on the `err == nil && len == 0` branch; presents as org #2 receiving inexplicable drift alerts about bins it recognises by number |
| 4 | D5 not shipped before org #2 (`resolveAirtagOrg` cache) | **SILENT** | Cross-tenant write, no log, ≤1 h window, and the written rows persist afterwards |
| 5 | T1: Flutter flips before the dashboard dual-subscribes | **SILENT on one surface** | Map freezes while the API is correct — the two disagreeing is the only signal |
| 6 | D1/D3 `orgID == ""` fallback left silent after Deploy 3 (§1.6.2) | **SILENT** | Events published to a channel with no subscribers; Redis keys written where no reader looks |
| 7 | D2 "simplified away" as redundant after T1 | **SILENT** | Reopens the original hole for any channel family without an org segment |
| 8 | T5: worker calls a method on the original receiver | **silent no-op** (not a leak) | Worker does nothing for that org; reads zero rows by RLS. Loud once the poisoned handle lands |
| 9 | T1: legacy branch dropped before adoption | SEMI-LOUD | Server logs 400s; the driver just sees tracking stop |
| 10 | T1: Flutter ships before the backend accepts both forms | LOUD, but unrecallable | Errors on both sides immediately; the build cannot be pulled back |
| 11 | T10: origins tightened on one side only | LOUD | Dashboard WebSocket fails to connect, or CORS blocks the API |
| 12 | T2 shipped early | nothing at runtime | `/ws` already closed; all 40 broadcasts are already no-ops |

---

## 5. Could not verify

Stated plainly rather than assumed.

1. **The `location_publish` granular proxy's binding in the live Centrifugo
   config.** `TIER1_PLAN.md` records the deployment as mixed-shape: unified
   `channel.proxy.subscribe` / `publish` **plus** a granular named
   `location_publish` proxy. If that proxy is attached by *namespace*
   (`publish_proxy_name` on `driver`), T1's 4-segment channel keeps hitting it
   and all is well. **If it is attached by a channel pattern or `channel_regex`
   matching `driver:location:*`, T1 Deploy 4 silently stops routing GPS through
   the backend** — no Redis write, no OSRM snap, no proximity auto-end, while
   Centrifugo still fans the raw fix out to the dashboard so the map keeps
   moving. That is failure #2 in §4 arriving from the config side instead of the
   code side. **This must be read out of `tapplerdev/binly-centrifugo-service`
   before T1 Deploy 1 is written**, and it is the single highest-value unknown in
   this document. I could not read the repo (private) or the service's Railway
   env vars from here.
2. **Whether any namespace carries a `channel_regex`.** Same repo, same
   inaccessibility. A regex anchored to today's segment count would reject every
   org-scoped channel at Centrifugo before the proxy is ever called — which is
   *loud*, so it is a lower risk than (1), but it would block T1 Deploy 3 with a
   confusing client-side error.
3. **Centrifugo namespace resolution for 3- and 4-segment channels.** Inherited
   unchanged from `TIER1_PLAN.md` "Could not verify" #1. One empirical test in
   D1 Deploy 1 discharges it for both tiers; do not run T1 before it passes.
4. **The production dashboard origin.** Not recorded in any of the three repos.
   T10 item 2 is blocked on a human answering this, not on code.
5. **Whether the running Centrifugo is on the config commit that was read.**
   `TIER1_PLAN.md` "Could not verify" #2, compounded by the floating image tag
   (§1.5.3) — which is exactly why pinning it is a prerequisite rather than
   hygiene.
6. **Whether the pre-migration `pg_dump` still exists anywhere durable.** T9
   step 1 assumes it does not and re-takes one. If a durable copy is found, step
   1 shortens to verifying it.
7. **Real-world Flutter manager-mode usage.** Inherited from `TIER1_PLAN.md`
   #4, and it matters more for T1 than for D1: T1's Deploy 5 gate can lean on
   Centrifugo presence on the `driver` namespace (`presence: true`), which D1
   could not, so the adoption question is answerable here even though the usage
   question is not.
8. **Nothing was executed against production for this plan.** DB access was
   read-only `SELECT` against `pg_class`, `pg_constraint`, `pg_indexes`,
   `pg_stat_user_tables` and eight `count(*)` queries. No Redis command, no
   Railway or Centrifugo change, no deploy, no source file modified.

## Resolved: location_publish proxy binding (2026-07-30)

TIER2_PLAN §5 lists this as the highest-value open unknown, blocking T1 Deploy 1:
whether the granular `location_publish` proxy is bound by NAMESPACE or by a
CHANNEL PATTERN. If it were a pattern matching `driver:location:*`, T1's rename
would silently stop routing GPS through the backend while the map kept moving.

**ANSWERED: it is namespace-bound, and the feared failure does not apply.**
Read directly from `config.json` in the (now private) `binly-centrifugo-service`
repo:

    proxies[0]: name=location_publish, keys = endpoint, http,
                include_connection_meta, name, timeout
                -> NO channel_regex, NO pattern, NO channels key
    namespace driver   -> subscribe_proxy_enabled, publish_proxy_enabled,
                          publish_proxy_name: location_publish
    namespace shift    -> subscribe_proxy_enabled
    namespace manager  -> subscribe_proxy_enabled
    namespace company  -> subscribe_proxy_enabled

No namespace defines a `channel_regex`. Centrifugo splits the namespace at the
FIRST colon, so `driver:{org}:location:{id}` still resolves to the `driver`
namespace and still routes through `location_publish`. The rename is safe on the
Centrifugo side.

Residual: pin the Centrifugo image tag before relying on this (it self-upgraded
6.6.0 -> 6.9.1 on a redeploy), since namespace-resolution behaviour is the one
assumption both plans rest on.
