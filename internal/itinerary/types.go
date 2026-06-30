// Package itinerary owns a shift's executed route_tasks: assembly, sequencing,
// and the task-type taxonomy. It is the writer for every route_tasks mutation an
// optimization or edit produces — audited removal (RemoveByIDs), move assembly
// (AddMove), reconciliation (ReconcileMove), renumbering (Resequence), and the
// optimizer-persist for BOTH entry points (shift-start first-optimize and
// mid-shift reopt) via ApplyOrder. The optimizer decides order and hands it an
// interleaved ordered list via ApplyOrder; manual edits tidy numbering without
// re-sorting (Resequence). sequence_order has exactly two writers: ApplyOrder
// (the optimizer's decided order) and Resequence (order-preserving renumber).
// Still outside the domain: initial shift creation (CreateShiftWithTasks, Phase 5)
// and a few task+bin edge writes (e.g. CompleteTask, Phase 6). See DESIGN.md.
package itinerary

import "fmt"

// TaskType is a route_task's kind. Validate inbound strings with ParseTaskType
// rather than casting them — the DB CHECK constraint is a last resort, not the
// boundary (an unchecked cast turns a bad value into a 500 at INSERT time).
type TaskType string

const (
	Collection    TaskType = "collection"
	Placement     TaskType = "placement"
	Pickup        TaskType = "pickup"
	Dropoff       TaskType = "dropoff"
	WarehouseStop TaskType = "warehouse_stop"
	Service       TaskType = "service"
)

// TaskTraits describe how the optimizer must treat a task type: its effect on
// truck load, whether it's half of a precedence pair, and whether it honors a
// time window. Adding a new task type = registering a descriptor here (OCP) —
// the single writer, Resequence, and ApplyOrder are all trait-agnostic.
//
// CapacityDelta mirrors the optimizer's shipment model (a move/placement occupies
// one bin slot; collection/service none). It is descriptive today; ApplyOrder /
// the optimizer-input mapping (Phase 4/5) are the load-bearing consumers.
type TaskTraits struct {
	CapacityDelta int  // +1 occupies a bin slot, -1 frees one, 0 no change
	Paired        bool // pickup→dropoff precedence pair (same move_request_id)
	HasTimeWindow bool // honors earliest/latest_arrival
}

// traits is the single source of truth for the task-type taxonomy, confirmed
// against the live optimizer mapping (capacity per move/placement, move pairing
// via one shipment, time windows for service tasks).
var traits = map[TaskType]TaskTraits{
	Collection:    {CapacityDelta: 0, Paired: false, HasTimeWindow: false},
	Placement:     {CapacityDelta: 1, Paired: false, HasTimeWindow: false},
	Pickup:        {CapacityDelta: 1, Paired: true, HasTimeWindow: false},
	Dropoff:       {CapacityDelta: -1, Paired: true, HasTimeWindow: false},
	WarehouseStop: {CapacityDelta: 0, Paired: false, HasTimeWindow: false},
	Service:       {CapacityDelta: 0, Paired: false, HasTimeWindow: true},
}

// ParseTaskType validates an inbound string and returns the typed value, or an
// error naming the offender. This is the boundary that turns a bad task_type
// into a clean 400 instead of a 500 at INSERT time.
func ParseTaskType(s string) (TaskType, error) {
	if _, ok := traits[TaskType(s)]; !ok {
		return "", fmt.Errorf("invalid task_type %q", s)
	}
	return TaskType(s), nil
}

// Traits returns the descriptor for a task type (zero value for unknown types).
func (t TaskType) Traits() TaskTraits { return traits[t] }

// Valid reports whether t is a known task type.
func (t TaskType) Valid() bool { _, ok := traits[t]; return ok }
