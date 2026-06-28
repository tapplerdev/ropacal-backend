# `internal/moverequest` — domain extraction design

Status: **scoped / in progress** (Phase 1). Owner: backend.

## Why this domain exists
Move-request logic is currently smeared across `handlers/bin_move_requests.go`
(3,370 lines), the shift-side reconciliation in `handlers/shifts.go`,
`helpers/move_request_history.go`, `services/move_request_monitor.go`, the
`bin_move_requests` schema, and the urgency calc. That smearing is exactly where
this session's bugs lived (monitor ignoring `assigned`; orphaned moves on task
removal; 5×-duplicated + drifted release logic; schema CHECK missing `assigned`).
A single domain with an explicit state machine makes that bug-class structurally
impossible.

Idiomatic-Go note: the package boundary is justified by **proven cohesion**, not
tidiness. We migrate **incrementally** (one slice per commit, golden-diff +
live-verify, deploy, soak) — never big-bang.

## State machine (the contract)

States: `pending` · `assigned` · `in_progress` · `completed` · `cancelled`

```
schedule ─────────────────────────► pending
assign-to-driver (manual) ────────► assigned   (assigned_user_id set, no shift)
assign-to-shift ──────────────────► assigned   (shift set; in_progress if shift already active)
shift starts ─────────────────────► in_progress (its assigned moves)
complete-task ────────────────────► completed
RELEASE (shift end/cancel/task-removed, automatic)
        ──────────────────────────► assigned   (driver BACKLOG: keep/derive driver, clear shift, type=manual)
clear-assignment (explicit human) ─► pending    (POOL: drop driver + shift)
cancel ───────────────────────────► cancelled
```

Key rules (already shipped this session):
- **Release preserves ownership** → driver's backlog (`assigned`), not the pool.
  The driver is derived from the shift at release time. Both `assigned` and
  `in_progress` moves are released (a ready-shift cancel must not orphan).
- **Only `clear-assignment` drops to the pool** (`pending`, no driver) — the
  deliberate escape hatch.
- Backlog moves are kept alive by the monitor (now watches `assigned`) + urgency
  tiers (`overdue`/`urgent`/`soon`/`scheduled`).

## Target package shape
```
internal/moverequest/
  move.go     // MoveRequest + typed Status (+ transition helpers) — later phase
  urgency.go  // Urgency(status,date,now) + ScheduledUrgency(date,now) — DONE
  store.go    // Store interface (consumer-defined) + sqlStore — ByID DONE
  service.go  // Service: Schedule, AssignToShift, AssignToDriver, ReleaseFromShift,
              //          ClearAssignment, Cancel, CompleteManually, FindOverdue/NotifyOverdue
  history.go  // unassignment/assignment audit (from helpers/move_request_history.go)
  reconciliation.go // ReleaseFromShift (seeded from handlers/shift_move_reconciliation.go)
```
- `Store` consumer-defined; `Service` is a struct (accept interfaces, return structs).
- The **MoveRequestMonitor splits**: its domain logic → `Service.FindOverdue/NotifyOverdue`;
  its 15-min scheduling → a thin watcher on a shared `internal/worker.Periodic`
  (the lifecycle mechanism — NOT a "monitoring" domain).

## Phased plan (each shippable + verified)
- **Phase 0 — spec.** This document.
- **Phase 1 — package + safest residents.** Create the package; move
  `ReleaseFromShift` (already isolated) in and point the 3 shift callers at it;
  add the typed `Status`. Behavior-preserving.
- **Phase 2 — `Store` seam + urgency.** Consumer-defined `Store`, DB-free tests;
  consolidate `Urgency`.
- **Phase 3 — `Service` + thin handlers.** Migrate handlers one-per-commit to
  delegate; switch the monitor to `Service.FindOverdue/NotifyOverdue`.
- **Phase 4 — history + ride-alongs.** Fold in history logging; do the
  move-request PUT→PATCH/POST verb fixes (lockstep with the dashboard).

## Related, separate initiatives (not part of this package)
- **Graceful shutdown** (cross-cutting bug): all worker `Stop()`s are dead code;
  add `signal.NotifyContext` + `http.Server.Shutdown` + context-threaded loops.
- **`internal/worker.Periodic`**: extract the 6×-copy-pasted ticker/start/stop
  lifecycle into one context-aware runner.
- **`total_bins` → `total_stops`** + final-stop flag (shift domain).

## Invariant for every slice
HTTP routes and response JSON stay **byte-identical** (golden-diff). Only internal
structure moves. Consumers (`ropacalapp`, `binly-dashboard`) see no change.
