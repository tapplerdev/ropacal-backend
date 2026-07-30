# Platform admin — cross-tenant access for Binly operators

**Goal.** A Binly operator can see and act on every organization: the same views
and rights an individual org's admin has, plus blended views across all of them
(one map with every tenant's bins), plus creating new organizations.

**Constraint set by the owner:** do it without touching existing tables or
weakening RLS.

**That constraint is satisfiable.** No `BYPASSRLS`, no new policies on existing
tables, no changes to the 45 handler files. Everything below is additive.

---

## 0. Why this is cheaper than it sounds

Two properties of the existing code carry the whole feature.

**Handlers take their scope from the request context, not a parameter.** Every
one of the 45 handler files begins `db := orgdb.From(r)`. Bind a different org's
handle to the context and the *same handler* serves that tenant, unchanged. So
"the same views and rights as that org's admin" is not an approximation — it is
literally the same code path.

**`orgdb.ForEachActiveOrg` is a production-proven cross-org read path that RLS
still enforces.** Six background workers use it every day. It enumerates active
orgs and hands the callback a handle bound to one org at a time; each handle
wraps every statement in a transaction that first runs
`set_config('app.org_id', …)`, so the database filters the rows. A blended view
is therefore N ordinary scoped reads assembled in Go — never one privileged read.

The dangerous design (a `BYPASSRLS` connection, or hand-written unscoped
queries) is not needed and should not be built. It would also collide with
`database.AssertRLSEnforced`, which deliberately refuses to boot when the app's
role bypasses RLS with two or more orgs.

---

## 1. Two mechanisms

### 1a. Act-as-org — everything an org admin can do

```
GET /api/platform/act/bins
X-Act-As-Org: acme
```

`middleware.PlatformAuth` verifies the platform token, resolves `acme` to an org
id, calls `orgdb.System(root, orgID)`, stashes the handle with
`orgdb.NewContext`, and hands off to the existing handler. `orgdb.From(r)` finds
it and everything downstream behaves exactly as it does for that tenant's own
admin — including RLS.

This unlocks all 45 handler files at once with no per-handler work.

### 1b. Fan-out aggregate — the blended views

```
GET /api/platform/bins        → every tenant's bins, each row tagged with its org
GET /api/platform/shifts
GET /api/platform/summary     → per-org counts for the operator dashboard
```

Loop `orgdb.ActiveOrgIDs`, take a scoped handle per org, run the *same* SELECT
the tenant handler runs, tag the rows, merge. Every individual query is
RLS-filtered to one tenant.

---

## 2. What is genuinely new

| Thing | Why it cannot reuse what exists |
|---|---|
| `platform_admins` table | Cannot live in `users`: that table is org-scoped under FORCE RLS, so a row with no organization_id would be invisible to every query including its own login lookup. No `organization_id`, no RLS — it is not tenant data. |
| `POST /api/platform/auth/login` | Cannot use `handlers.Login`: that resolves an org slug and mints an `org_id` claim. A platform admin has no org. |
| `middleware.PlatformAuth` | Cannot use `middleware.Org`: that binds the caller's single org from the JWT. |
| `platform_audit_log` | New. One row per platform request, written by the middleware so a handler cannot forget it. |

Token shapes stay deliberately incompatible:

```
tenant:   {user_id, email, role, org_id: "0000…0001"}
platform: {admin_id, email, platform: true}          ← no org_id
```

---

## 3. Mutual exclusion — and how much is already free

**Platform token on a tenant route: already rejected today.** `middleware.Org`
401s when `claims.OrgID == ""` (org.go:50-56), and D4b's membership check would
independently reject it because there is no matching `users` row. Two existing
guards already fail closed on this. Worth an explicit `platform != true` check
anyway, so the intent is stated rather than inherited.

**Tenant token on a platform route: must be added.** `PlatformAuth` requires
`platform == true` AND a matching live row in `platform_admins`. A tenant token
lacks the claim and the row.

This direction is the one that matters. Get it wrong and any org admin becomes a
platform admin.

---

## 4. Phases

Each phase is independently shippable and independently revertible.

### Phase 1 — foundation (no user-visible change)
- `platform_admins` table (goose migration `00002_platform_admins.sql`)
- `platform_audit_log` table
- `POST /api/platform/auth/login` + `middleware.PlatformAuth`
- A `/api/platform/whoami` endpoint and nothing else

**Gate.** A platform token 401s on `/api/*` (both existing guards, verified
individually). A tenant token 401s on `/api/platform/*`. A deleted platform admin
401s. Every request appears in `platform_audit_log`.

### Phase 2 — act-as-org
- `X-Act-As-Org` resolution + context binding
- Mount the existing authed route group under `/api/platform/act/*`

**Gate.** With two orgs live (locally, as in the org-API test): acting as A
returns exactly A's bins; acting as B returns exactly B's; an unknown slug 404s;
a *missing* header 400s rather than silently falling back to any org. Compare
output byte-for-byte against what each org's own admin sees.

### Phase 3 — aggregate endpoints
- `/api/platform/summary`, `/api/platform/bins`, `/api/platform/shifts`
- Each row carries `organization_id` + slug

**Gate.** Locally with 3 orgs: aggregate bin count equals the sum of the three
per-org counts, and every row's org tag matches the org it was read from.

### Phase 4 — frontend
- Operator surface: org list, switcher, unified map
- Org creation moves here from the internal API

### Phase 5 — realtime (optional, deliberately last)
Everything shipped in D1 keys on `company:{orgID}:events`, and
`sameOrgAsSubscriber` rejects a platform identity. Extending
`authorizeSubscription` with a platform branch is where a cross-tenant bug would
be subtlest. **Poll in v1.**

---

## 5. Decisions taken

**Act-as-org is READ-WRITE; aggregates are READ-ONLY.** Support work needs
writes, and act-as-org is the same code path a tenant admin uses, so writes there
are no more dangerous than that admin's own. The aggregate endpoints are
purpose-built and have no reason to write. Reversible: gate act-as-org writes
behind a flag if it proves uncomfortable.

**Platform login needs stronger auth than a password.** This is every tenant's
data behind one credential. TOTP at minimum, and a short token TTL — an hour,
matching the Centrifugo TTL from D4a.

---

## 6. Risks

1. **A leaked platform token is every tenant's data.** Mitigated by TOTP, short
   TTL, and the audit log. This is the main reason not to fold platform access
   into the existing dashboard's auth.
2. **Fan-out is N queries per request.** Fine at 2–20 tenants; needs caching or
   pagination beyond that. Log the org count on aggregate endpoints so the
   growth is visible before it hurts.
3. **`X-Act-As-Org` must fail closed.** Missing or unknown header = error, never
   "default to the first org". A silent default is how an operator edits the
   wrong tenant's data.
4. **Audit must be written by middleware, not handlers.** 45 handlers will not
   remember to log.
5. **Do not let act-as-org reuse the tenant JWT path.** The temptation will be to
   mint a tenant token for the target org and reuse everything. That creates a
   real credential for a tenant the operator does not belong to, which then
   behaves like any other token — including surviving in a browser. Bind the
   handle per request instead.

---

## 7. Explicitly not doing

- No `BYPASSRLS` role, anywhere.
- No new policies on existing tables.
- No `organization_id` nullable-ing, or any "NULL means all orgs" trick — that
  would weaken every existing policy.
- No changes to the 45 handler files.
