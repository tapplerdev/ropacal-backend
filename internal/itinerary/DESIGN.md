# `internal/itinerary` — the shift's executed itinerary (route_tasks owner)

Status: **in progress** (Phases 0–4 done + live-verified; Phases 5–6 remain). Successor to the `moverequest` domain; follows
the same conventions (consumer-defined seams, phased behavior-preserving migration,
golden-diff + live-verify each slice). Supersedes the earlier `internal/route`
scoping — renamed to `itinerary` because "route" is overloaded (see Boundary).

## Boundary
The **shift's executed itinerary** — the single owner of every `route_tasks`
read/write for a shift. Owned by `shift_id`, born in the shift-creation tx, never
structurally tied to a route *template*.

- **The optimizer becomes a pure function** (`request → ordered task IDs`); it no
  longer writes `route_tasks`.
- **`route` (the `routes`/`route_bins` template tables) stays a separate, legacy/
  optional feeder** — and the future home of the OR-Tools "saved optimized route"
  cache idea. Not part of this domain.
- **The app wire-contract is sacred**: the backend sends `task_type` strings +
  coordinates; the app's `StopType` enum renders/navigates off them (dropoff uses
  `destination_lat/lng`). Every slice keeps `GetShiftTasksDetailed` + Centrifugo
  `route_updated` payloads byte-identical (golden-diff).

## Model (data-driven types)
```go
type TaskType string                 // validated at the boundary (not an unchecked cast)
const (Collection, Placement, Pickup, Dropoff, WarehouseStop, Service)

type TaskTraits struct {             // exactly what the optimizer consumes (confirmed in code):
    CapacityDelta int                //  move/placement = +1, collection/service = 0
    Paired        bool               //  move = pickup→dropoff shipment (precedence + same vehicle)
    HasTimeWindow bool               //  service today; OR-Tools supports it for any type
}
// a registry maps TaskType → TaskTraits; adding a type = register a descriptor (OCP).
// (The app renders/navigates off the task_type STRING directly — no display field needed.)
```

## Interface (one owner; two sequence writers)
```go
type Itinerary interface {
    AddCollection(ext, shiftID, binID) ; AddPlacement(ext, shiftID, src)
    AddMove(ext, shiftID, move)        // pickup [+ dropoff], same move_request_id
    AddWarehouseStop(ext, shiftID, action) ; AddService(ext, shiftID, svc)

    RemoveTasks(ext, sel, reason, by) error   // the ONE audited soft-delete
    Resequence(ext, shiftID) error            // order-PRESERVING dense renumber (manual edits)
    ApplyOrder(ext, shiftID, stops []OrderedStop, isFirst bool) error // optimizer order (interleaved), in a tx

    TasksForShift(shiftID) (...)              // read seam
}
```
`sequence_order` has **exactly two writers**: `ApplyOrder` (stamps the optimizer's
order) and `Resequence` (renumbers dense 1..N, never re-sorts). `lock_route_order`
→ take the `Resequence` (append) path, never `ApplyOrder`.

## Optimize → persist seam
```
optimizer.Optimize(req) → orderedTaskIDs        (PURE, no DB)
        ▼
itinerary.ApplyOrder(tx, shiftID, orderedIDs, newWarehouseStops, isFirst)   (all writes, one tx)
```

## Confirmed optimizer semantics (investigated 2026-06-28)
- **Capacity**: vehicle cap = `shift.TruckBinCapacity` (default 4); each move/placement
  is a shipment `size{bins:1}`; collection = 0. Enforced by OR-Tools (active) + Mapbox (comparison).
- **Pickup↔dropoff**: same `move_request_id`; modeled as ONE shipment `{from,to}` →
  precedence + same-vehicle guaranteed.
- **Time windows**: plumbed but only populated for `service` tasks today. OR-Tools
  supports time windows (Time dimension + `CumulVar.SetRange`, soft via
  `SetCumulVarSoftUpperBound`) for any type — turning it on for bins/moves is
  "populate the window", not a rewrite.

## Phases (each: behavior-preserving · golden-diff byte-identical · live shift exercise · deploy · soak)
- **0 — Spec + schema.** This DESIGN; add idempotent `CREATE TABLE route_tasks` DDL
  (canonical schema; currently only ALTERs exist); validated `TaskType` + `TaskTraits` registry.
- **1 — Foundation.** Package + **single typed writer** (1 INSERT replaces the 3
  divergent variants); `Resequence` (pure, unit-tested); **validate `TaskType` at the
  boundary** (clean 400, not a 500-at-insert). Wire into one low-risk site; `MAX` once.
- **2 — AddMove.** `assignMoveToShift` → `AddMove`; replace the `+binsAdded` bump with
  `Resequence`. (Fixes O(N) `MAX`.) Unblocks moverequest (c) kernel.
- **3 — RemoveTasks.** One audited soft-delete → migrate the ~5 sites. Fix **#16**
  (EndShift stale `status='pending'` subquery) + verify **#20** (SkipTask placeholder).
- **4 — ApplyOrder (centerpiece). ✅ DONE + LIVE-VERIFIED.** Optimizer persist extracted →
  `ApplyOrder(ext, shiftID, []OrderedStop, isFirst)`; both entry points (shift-start
  `optimizeRouteWithMapbox` + mid-shift `ReoptimizeActiveShift`) build an interleaved
  `[]OrderedStop` and delegate. Optimizer confirmed **already pure** (DB-free). `lock_route_order`
  guard added (**#19**, verified — locked shift takes the Resequence path, never ApplyOrder).
  Reopt warehouse hard-delete moved in-tx (torn-write fix, slice 1). `isFirst=true` refreshes
  coords + uses the legacy first-opt renumber (`sequence_order, created_at` — NOT warehouse-last)
  so a leading warehouse pickup + binsPreloaded auto-completed pickups keep their slots;
  `isFirst=false` does seq-only UPDATE + `Resequence`. (StartShift first-opt was already
  tx-wrapped — the DESIGN's torn-write claim was refuted by the code.) Verified live: first-opt
  ID-stability (collections + relocation pickup/dropoff), reopt ID-stability + warehouse regen,
  and a mid-shift sim proving reopt re-anchors on the driver's GPS.
- **5 — Creation.** `CreateShiftWithTasks` + `UpdateShift.add_tasks` → intent methods +
  shared coordinate resolver. **IN PROGRESS — add_tasks half DONE + golden-verified:**
  `insertTask` (the single INSERT assembler; per-column bind-NULL vs omit-for-DDL-default
  policy) + `AddCollection`/`AddPlacement`/`AddMoveLeg` intent methods (create.go), sqlmock-
  pinned. Slice 2a was byte-identical (18/18 golden artifacts); Slice 2b consciously fixed
  the PATCH-path #34 regression (added pickups now carry destination_*/move_type/bin_number;
  dropoffs born AT the destination with address set) — verified additive-only delta.
  **Slice 3a DONE + golden-verified (3/3 identical):** `InsertCreatedTask` carries the
  create 30-column bind-all contract (explicit NULLs bypass DDL defaults); both
  CreateShiftWithTasks call sites (request loop + deployments) migrated — pure INSERT
  relocation, enrichment/merge left in place. **INSERT census: zero route_tasks INSERTs
  outside this package.** Create golden harness: scratchpad/p5c_golden.sh (pins gap-seq
  via retired skip, the 0,0 clobbers, live-config store dest, explicit-NULL windows,
  string bin_number, deployment at len(tasks)+1).
  **Slice 4 conscious unifications DONE + golden-reviewed (1465315/87ca67f):**
  ParseTaskType→400 on create (was DB-CHECK 500); RecomputeShiftCounts in-tx at create
  (replaces hand-count + swallowed skip-decrement + dead deployment increment; counts
  carry the logical-bin semantic from birth; 201 task_count includes deployments);
  CLOBBER FIXED (pickup-0,0→bins and warehouse_stop→warehouse coords now persist —
  no more null-island rows); add_tasks store dropoffs re-resolve the LIVE config
  warehouse (snapshot retired); add_tasks move assignment via moverequest.AssignToShift
  (status flip, assignment_type 'shift', assigned_user_id NULL per #31, LogAssigned
  in-tx, terminal moves 400). Verified deltas: create task_count 13→14 + the two
  coordinate fixes ONLY; add_tasks 17/18 identical, delta confined to the move row.
  **REMAINING (structural, behavior-neutral):** extract the create loop's enrichment
  branches into typed domain Resolver functions (bins/potloc/move/warehouse-config),
  replacing CreatedTask's interface{} passthrough — pure refactor now that all
  behavioral deltas above have landed; gate with the same p5c golden harness.
- **6 — Edges + tx gaps.** `bins.go`→`SyncBinTasks`, `potential_locations.go`→
  `SyncPlacement`; wrap `CompleteTask`'s task+bin writes in a tx (torn-write).

## Bugs this initiative closes
- **#19** lock_route_order ignored on mid-shift re-optimize → Phase 4. ✅ fixed + live-verified.
- Reopt warehouse hard-delete ran pre-tx (torn-write on optimizer/no-route failure) → Phase 4 slice 1. ✅ fixed.
  (StartShift first-optimization persist was already tx-wrapped — the original non-transactional claim was refuted.)
- CompleteTask non-transactional (task+bin) → Phase 6.
- **#16** EndShift soft-delete stale subquery → Phase 3.
- **#20** SkipTask paired-dropoff placeholder smell → verify in Phase 3.
- O(N) `MAX` query (Phase 1/2), unchecked `TaskType` cast (Phase 1), 3 divergent INSERTs (Phase 1).

## Invariants
Driver task JSON + Centrifugo payloads byte-identical. Mutations synchronous, in the
caller's tx. Optimizer owns *order*; itinerary owns *numbering + persistence*. Two
`sequence_order` writers only.

## Future: `shift_edit_history` (unblocked by this domain)
Today shift edits are audited only **row-level** on `route_tasks` (`added_by`/
`addition_reason`/`deleted_by`/`deletion_reason`) + the move-side effect in
`move_request_history`; there is **no unified, queryable edit timeline** for a shift
(`shift_history` is the *completed-shift archive*; `shift_edited` is an ephemeral
Centrifugo event). Once itinerary is the **single writer** of `route_tasks`, every
`AddX` / `RemoveTasks` / `ApplyOrder` is one choke point that can also append a
`shift_edit_history` event (added/removed/reordered/reassigned, with actor + reason).
A clean follow-on feature this consolidation makes cheap — not part of the
behavior-preserving migration. Tracked separately.

## Out of scope (separate initiatives)
- OR-Tools "saved optimized route" cache / revive `route` templates (optimization layer; feeds `ApplyOrder`).
- Time windows for collections/moves (capability confirmed; product decision).
- PUT→PATCH verb sweep (#14, dashboard lockstep).
- moverequest (c) kernel — *unblocked* by Phases 2 & 4.
- `shift_edit_history` unified edit timeline (see Future above).
