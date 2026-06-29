package itinerary

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// MoveDestination is a relocation move's drop-off point.
type MoveDestination struct {
	Address string
	Lat     float64
	Lng     float64
}

// ReconcileOutcome reports what ReconcileMove changed, so the caller can notify
// the driver appropriately (after commit).
type ReconcileOutcome struct {
	DropoffRemoved bool
	DropoffAdded   bool
	AddressUpdated bool
}

// ErrMissingDestination is returned when a move becomes a relocation but no
// destination (address + coords) was supplied — the caller maps it to a 400
// instead of silently leaving the route without a drop-off.
var ErrMissingDestination = errors.New("relocation requires a destination (address + coordinates)")

// ReconcileMove brings a move's route_tasks in line with a move_type/address
// change on the move's shift — the itinerary domain owns these route_tasks writes
// so the handler doesn't hand-roll them:
//   - newType "store" (was relocation): soft-delete the drop-off(s) via RemoveByIDs.
//   - "store" → "relocation": add a drop-off right after the pickup, opening room
//     first so sequence_order stays dense (the old raw insert at pickupSeq+1 could
//     collide with the next task).
//   - address change (still relocation): update the drop-off's destination.
//
// Only incomplete, non-deleted tasks for this move on this shift are touched.
// Runs in the caller's ext (tx). Returns what changed.
func ReconcileMove(ext sqlx.Ext, shiftID, moveReqID, oldType, newType string, addressChanged bool, dest *MoveDestination, by string, now int64) (ReconcileOutcome, error) {
	var out ReconcileOutcome

	var tasks []struct {
		ID       string `db:"id"`
		TaskType string `db:"task_type"`
		Seq      int    `db:"sequence_order"`
	}
	if err := sqlx.Select(ext, &tasks, ext.Rebind(`
		SELECT id, task_type, sequence_order
		FROM route_tasks
		WHERE move_request_id = ? AND shift_id = ? AND is_completed = 0 AND is_deleted = false`),
		moveReqID, shiftID); err != nil {
		return out, fmt.Errorf("reconcile: fetch tasks: %w", err)
	}

	dropoffIDs := func() []string {
		var ids []string
		for _, t := range tasks {
			if t.TaskType == "dropoff" {
				ids = append(ids, t.ID)
			}
		}
		return ids
	}

	switch {
	case newType == "store" && oldType == "relocation":
		ids := dropoffIDs()
		if err := RemoveByIDs(ext, ids, by, "move_type_changed_to_store", now); err != nil {
			return out, err
		}
		out.DropoffRemoved = len(ids) > 0

	case newType == "relocation" && oldType == "store":
		if dest == nil {
			return out, ErrMissingDestination
		}
		pickupSeq, found := 0, false
		for _, t := range tasks {
			if t.TaskType == "pickup" {
				pickupSeq, found = t.Seq, true
				break
			}
		}
		if !found {
			// No pickup on the route (move not on this shift's tasks) — nothing to attach to.
			return out, nil
		}
		// Open room after the pickup so the new drop-off slots in without colliding.
		if _, err := ext.Exec(ext.Rebind(`
			UPDATE route_tasks SET sequence_order = sequence_order + 1
			WHERE shift_id = ? AND is_deleted = false AND sequence_order >= ?`),
			shiftID, pickupSeq+1); err != nil {
			return out, fmt.Errorf("reconcile: open room for dropoff: %w", err)
		}
		if _, err := ext.Exec(ext.Rebind(`
			INSERT INTO route_tasks (
				id, shift_id, move_request_id, task_type, sequence_order,
				address, destination_address, destination_latitude, destination_longitude,
				is_completed, created_at, updated_at
			) VALUES (?, ?, ?, 'dropoff', ?, ?, ?, ?, ?, 0, ?, ?)`),
			uuid.New().String(), shiftID, moveReqID, pickupSeq+1,
			dest.Address, dest.Address, dest.Lat, dest.Lng, now, now); err != nil {
			return out, fmt.Errorf("reconcile: insert dropoff: %w", err)
		}
		out.DropoffAdded = true

	case addressChanged && dest != nil:
		ids := dropoffIDs()
		for _, did := range ids {
			if _, err := ext.Exec(ext.Rebind(`
				UPDATE route_tasks
				SET destination_address = ?, destination_latitude = ?, destination_longitude = ?, updated_at = ?
				WHERE id = ?`),
				dest.Address, dest.Lat, dest.Lng, now, did); err != nil {
				return out, fmt.Errorf("reconcile: update dropoff destination: %w", err)
			}
		}
		out.AddressUpdated = len(ids) > 0
	}

	return out, nil
}
