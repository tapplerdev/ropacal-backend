# `internal/route` — RoutePlanner / route-domain design

Status: **scoped, not started.** Owner: backend. Successor initiative to
`internal/moverequest`. See that package's DESIGN.md for the conventions this
follows (consumer-defined seams, phased behavior-preserving migration, golden-diff
+ live-verify each slice).

## Why this domain exists
`route_tasks` is the most fragmented table in the codebase (investigated 2026-06-28):

- **~13 functions across 5 files** write it with hand-rolled SQL.
- **3 separate creation paths** (`CreateShiftWithTasks`, `UpdateShift.add_tasks`,
  `assignMoveToShift`) + the optimizer's own INSERT path, with **3 divergent INSERT
  column sets** (14 / 15 / 30 columns for the "same" row).
- **`sequence_order` is written/renumbered in ~7 independent places** — the top
  correctness hazard. `sequence_order` *is* the route order the driver follows; the
  `"duplicate sequence_order detected"` / `pickup>=dropoff` guard in
  `assignMoveToShift` exists *because* sequencing is scattered and fragile.
- **Unrelated domains reach in**: `bins.go` and `potential_locations.go` `UPDATE
  route_tasks` directly to keep tasks in sync when their own entities change.
- **No `CREATE TABLE route_tasks` DDL exists** — the schema lives only in scattered
  INSERTs + `models.RouteTask` + `ALTER TABLE ADD COLUMN` migrations.

A single owner (the route aggregate) makes the route consistent by construction and
lets every other domain express *intent* instead of writing route SQL.

## The wire contract (this work must NOT change it)
The backend sends `task_type` strings + coordinates; the Flutter app's `StopType`
enum exact-matches the 6 backend strings and renders/navigates off them (dropoff
uses `destination_latitude/longitude`). The app does **no** ordering — it trusts
backend `sequence_order`. So `RoutePlanner` becomes the single *producer* of the
`{type, coords, sequence}` rows the app already consumes. Delivery
(`GetShiftTasks(Detailed)` + Centrifugo `route_updated`) is unchanged.

`task_type`: `collection | placement | pickup | dropoff | warehouse_stop | service`
(CHECK-constrained).

## The seam: `RoutePlanner` (synchronous, tx-scoped — NOT events)
Route mutations must be atomic with the change that triggered them, so every method
takes the caller's `sqlx.Ext` (its transaction). This decouples *code* (callers
depend on intent, not the table) while keeping the operation synchronous +
consistent. (Async/event-bus would trade atomicity for an inconsistency window — wrong
tool here; see the moverequest discussion.)

```go
type RoutePlanner interface {
    // intent → the planner derives task_type, coordinates, and slots in sequence
    AddCollection(ext sqlx.Ext, shiftID, binID string) error
    AddPlacement(ext sqlx.Ext, shiftID string, src PlacementSource) error
    AddMove(ext sqlx.Ext, shiftID string, m *moverequest.MoveRequest) error // relocation→pickup+dropoff, store→pickup
    AddWarehouseStop(ext sqlx.Ext, shiftID string, action WarehouseAction) error
    AddService(ext sqlx.Ext, shiftID string, svc ServiceTask) error

    RemoveTasks(ext sqlx.Ext, sel Selector, reason, by string) error // the ONE audited soft-delete
    Resequence(ext sqlx.Ext, shiftID string) error                   // ORDER-PRESERVING dense renumber (close gaps / break collisions); never re-sorts
    ApplyOrder(ext sqlx.Ext, shiftID string, orderedTaskIDs []string) error // writes the OPTIMIZER'S decided order (1..N) — the only "imposes order" writer
    ReplaceRoute(ext sqlx.Ext, shiftID string, optimized []Task) error // post-optimization delete-all + re-insert, then ApplyOrder

    // Resequence + ApplyOrder are the ONLY writers of sequence_order. They are
    // distinct: Resequence keeps current relative order (for post-manual-edit
    // normalization); ApplyOrder stamps the optimizer's result (the optimizer
    // owns order — see invariants).

    SyncBinTasks(ext sqlx.Ext, binID string) error             // bins.go edge
    SyncPlacement(ext sqlx.Ext, potentialLocationID string) error // potential_locations.go edge
}
```
Internally backed by **one typed task writer** (one INSERT, one column list).

## What it consolidates (the duplication today)
1. The INSERT column set (3+ variants → 1).
2. `task_type` derivation (move_type branch vs caller-supplied vs optimizer) → 1 resolver.
3. Coordinate resolution (bin / potential-location / move original+new / store→warehouse override) → 1 resolver.
4. `sequence_order` assignment/renumber (~7 sites → `Resequence`, single-writer).
5. Soft-delete-with-audit (EndShift / RemoveTasks / UpdateShift / Cancel / Clear → `RemoveTasks`).

## Phased plan (each shippable, golden-diff + live-verify, deploy, soak)
- **Phase 0 — spec.** This doc. Also add an idempotent `CREATE TABLE route_tasks`
  DDL to `database.go` so the schema is canonical (no behavior change).
- **Phase 1 — package + single writer + the two sequence_order primitives.**
  Introduce `internal/route`, the one typed INSERT, plus `Resequence`
  (order-preserving) and `ApplyOrder` (optimizer-order). **Ground-truth site map
  (investigated 2026-06-28) — 5 `sequence_order` writers:**
  - `shifts.go:3031`, `:3074` (ReoptimizeActiveShift), `:6723` (segment optimizer),
    `:7588` (Mapbox path) — all **impose the optimizer's order** → migrate to
    `ApplyOrder` (these are NOT `Resequence`; the earlier "route renumber loops
    through Resequence" wording was wrong — the optimizer owns order).
  - `bin_move_requests.go:649` (`assignMoveToShift` `+binsAdded` bump to make room
    for a manual insert) — the manual-edit path; normalize with `Resequence`.
  The `"duplicate sequence_order detected"` guard fires from the **collision**
  between the manual bump and the optimizer numbering; making both go through the
  two single-writers is the correctness win.
- **Phase 2 — `AddMove`.** Migrate `assignMoveToShift`'s relocation→pickup+dropoff +
  the `UpdateBinMoveRequest` move-type cascade. (This is the clean home the
  moverequest kernel was missing.)
- **Phase 3 — `RemoveTasks`.** One audited soft-delete; migrate the 5 copy-pasted sites.
- **Phase 4 — `AddCollection`/`AddPlacement`/`AddWarehouseStop`.** Migrate
  `CreateShiftWithTasks` + `UpdateShift.add_tasks` onto the intent methods + the
  shared coordinate resolver.
- **Phase 5 — `ReplaceRoute`.** Migrate the optimizer's delete-all + re-insert.
  **Highest regression surface — do last.**
- **Phase 6 — conversion edges.** `bins.go` → `SyncBinTasks`; `potential_locations.go`
  → `SyncPlacement`. Those domains stop importing `route_tasks` knowledge.

## Invariants
- **Driver-facing task JSON (`GetShiftTasksDetailed`) + Centrifugo payloads stay
  byte-identical** (golden-diff every slice). The app contract is sacred.
- Route mutations stay **synchronous + in the caller's tx**. No event bus.
- **`Resequence` is an order-*preserving* renumber, never a re-sort.** It takes the
  tasks in their current relative order and normalizes the integers to dense `1..N`
  — closing soft-delete gaps and breaking duplicate/collision deterministically —
  **without ever moving one task past another.** It owns the *numbering*, not the
  *order*.
- **The optimizer owns order; `RoutePlanner` owns persistence.** OR-Tools is fed the
  pickup→dropoff precedence constraint, so its returned sequence already guarantees
  pickup-before-dropoff and *is authoritative* — we persist it `1..N` and ship it to
  the driver as-is; we never re-sort it. Between optimizations, manual inserts keep
  pickup<dropoff by placing the dropoff **adjacent after** its pickup (insertion
  logic, not `Resequence`).

## Boundary with optimization (keep it)
The optimizer (OR-Tools is the *active* one; Mapbox is comparison — see
[[ropacal-active-optimizer]]) decides task **order**; `RoutePlanner` **persists**
tasks. The optimizer calls `Resequence`/`ReplaceRoute`; it does not hand-write
`route_tasks`. RoutePlanner sits *below* optimization, not inside it.

## Risks
- The optimizer is the heaviest writer (delete-all + re-insert + OSRM segmenting);
  `ReplaceRoute` must reproduce it faithfully → Phase 5, most care.
- Bigger than moverequest (5 files + the optimizer + conversion edges). Multi-session.

## Relationship to moverequest
This unblocks finishing moverequest's deliberately-left kernels: `assign-to-shift`
route insertion and `UpdateBinMoveRequest`'s route cascade become
`planner.AddMove(...)` / `planner.RemoveTasks(...)` calls with the handler's tx.
