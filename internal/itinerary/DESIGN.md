# `internal/itinerary` — the shift's executed itinerary (route_tasks owner)

Status: **in progress** (Phase 0/1). Successor to the `moverequest` domain; follows
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
    DisplayType   string             //  the string the app renders/navigates off
}
// a registry maps TaskType → TaskTraits; adding a type = register a descriptor (OCP)
```

## Interface (one owner; two sequence writers)
```go
type Itinerary interface {
    AddCollection(ext, shiftID, binID) ; AddPlacement(ext, shiftID, src)
    AddMove(ext, shiftID, move)        // pickup [+ dropoff], same move_request_id
    AddWarehouseStop(ext, shiftID, action) ; AddService(ext, shiftID, svc)

    RemoveTasks(ext, sel, reason, by) error   // the ONE audited soft-delete
    Resequence(ext, shiftID) error            // order-PRESERVING dense renumber (manual edits)
    ApplyOrder(ext, shiftID, orderedIDs []string, newTasks []Task, isFirst bool) error // optimizer order, in a tx

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
- **4 — ApplyOrder (centerpiece).** Extract the 360-line optimizer persist →
  `ApplyOrder(tx,…)`; optimizer becomes pure; **make `StartShift` first-opt
  transactional** (fixes torn-write); both entry points call it; **`lock_route_order`
  guard** at the boundary (**#19**). Highest value + risk.
- **5 — Creation.** `CreateShiftWithTasks` + `UpdateShift.add_tasks` → intent methods +
  shared coordinate resolver.
- **6 — Edges + tx gaps.** `bins.go`→`SyncBinTasks`, `potential_locations.go`→
  `SyncPlacement`; wrap `CompleteTask`'s task+bin writes in a tx (torn-write).

## Bugs this initiative closes
- **#19** lock_route_order ignored on mid-shift re-optimize → Phase 4.
- StartShift first-optimization persist non-transactional (torn-write on route order) → Phase 4.
- CompleteTask non-transactional (task+bin) → Phase 6.
- **#16** EndShift soft-delete stale subquery → Phase 3.
- **#20** SkipTask paired-dropoff placeholder smell → verify in Phase 3.
- O(N) `MAX` query (Phase 1/2), unchecked `TaskType` cast (Phase 1), 3 divergent INSERTs (Phase 1).

## Invariants
Driver task JSON + Centrifugo payloads byte-identical. Mutations synchronous, in the
caller's tx. Optimizer owns *order*; itinerary owns *numbering + persistence*. Two
`sequence_order` writers only.

## Out of scope (separate initiatives)
- OR-Tools "saved optimized route" cache / revive `route` templates (optimization layer; feeds `ApplyOrder`).
- Time windows for collections/moves (capability confirmed; product decision).
- PUT→PATCH verb sweep (#14, dashboard lockstep).
- moverequest (c) kernel — *unblocked* by Phases 2 & 4.
