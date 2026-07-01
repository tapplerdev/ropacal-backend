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
// so the handler doesn't hand-roll them. "Two-leg" moves (relocation and
// redeployment) carry a pickup + a drop-off; "single-leg" moves (store, pickup_only)
// carry only a pickup:
//   - two-leg → single-leg: soft-delete the drop-off(s) via RemoveByIDs.
//   - single-leg → two-leg: add a drop-off right after the pickup, opening room
//     first so sequence_order stays dense (a raw insert at pickupSeq+1 could
//     collide with the next task); requires a destination.
//   - relocation ↔ redeployment (two-leg → two-leg): keep route_tasks.move_type in sync.
//   - address change (still two-leg): update the drop-off's destination.
//
// Only incomplete, non-deleted tasks for this move on this shift are touched.
// Runs in the caller's ext (tx). Returns what changed.
func ReconcileMove(ext sqlx.Ext, shiftID, moveReqID, oldType, newType string, addressChanged bool, dest *MoveDestination, by string, now int64) (ReconcileOutcome, error) {
	var out ReconcileOutcome

	var tasks []struct {
		ID        string  `db:"id"`
		TaskType  string  `db:"task_type"`
		Seq       int     `db:"sequence_order"`
		BinID     *string `db:"bin_id"`
		BinNumber *int    `db:"bin_number"`
	}
	if err := sqlx.Select(ext, &tasks, ext.Rebind(`
		SELECT id, task_type, sequence_order, bin_id, bin_number
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

	// relocation and redeployment both carry a drop-off leg; store/pickup_only do not.
	twoLeg := func(mt string) bool { return mt == "relocation" || mt == "redeployment" }

	updateDropoffDests := func() error {
		ids := dropoffIDs()
		for _, did := range ids {
			if _, err := ext.Exec(ext.Rebind(`
				UPDATE route_tasks
				SET destination_address = ?, destination_latitude = ?, destination_longitude = ?, updated_at = ?
				WHERE id = ?`),
				dest.Address, dest.Lat, dest.Lng, now, did); err != nil {
				return fmt.Errorf("reconcile: update dropoff destination: %w", err)
			}
		}
		out.AddressUpdated = len(ids) > 0
		return nil
	}

	// #34: a two-leg move's PICKUP carries the destination as the optimizer's hint (it
	// reads a move off the pickup row). Whenever the destination changes or a move becomes
	// two-leg, refresh the pickup's destination_* so a re-opt routes to the current
	// destination and the move stays optimizer-visible. Requires dest != nil.
	setPickupDestination := func() error {
		if _, err := ext.Exec(ext.Rebind(`
			UPDATE route_tasks
			SET destination_latitude = ?, destination_longitude = ?, destination_address = ?, updated_at = ?
			WHERE move_request_id = ? AND shift_id = ? AND task_type = 'pickup' AND is_completed = 0 AND is_deleted = false`),
			dest.Lat, dest.Lng, dest.Address, now, moveReqID, shiftID); err != nil {
			return fmt.Errorf("reconcile: sync pickup destination: %w", err)
		}
		return nil
	}

	switch {
	case twoLeg(oldType) && !twoLeg(newType):
		// two-leg → single-leg (relocation/redeployment → store/pickup_only): drop the drop-off(s).
		ids := dropoffIDs()
		if err := RemoveByIDs(ext, ids, by, "move_type_changed_to_"+newType, now); err != nil {
			return out, err
		}
		out.DropoffRemoved = len(ids) > 0

	case !twoLeg(oldType) && twoLeg(newType):
		// single-leg → two-leg (store/pickup_only → relocation/redeployment): add a drop-off.
		if dest == nil {
			return out, ErrMissingDestination
		}
		pickupSeq, found := 0, false
		var binID *string
		var binNumber *int
		for _, t := range tasks {
			if t.TaskType == "pickup" {
				pickupSeq, binID, binNumber, found = t.Seq, t.BinID, t.BinNumber, true
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
		// Mirror AddMove's drop-off columns so every NOT NULL column is satisfied
		// (bin_id/bin_number from the pickup; latitude/longitude = the destination;
		// move_type = newType so the leg matches the move's actual two-leg kind).
		if _, err := ext.Exec(ext.Rebind(`
			INSERT INTO route_tasks (
				id, shift_id, bin_id, bin_number, sequence_order, task_type,
				latitude, longitude, address,
				destination_latitude, destination_longitude, destination_address,
				move_request_id, move_type, is_completed, created_at
			) VALUES (?, ?, ?, ?, ?, 'dropoff', ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`),
			uuid.New().String(), shiftID, binID, binNumber, pickupSeq+1,
			dest.Lat, dest.Lng, dest.Address,
			dest.Lat, dest.Lng, dest.Address,
			moveReqID, newType, now); err != nil {
			return out, fmt.Errorf("reconcile: insert dropoff: %w", err)
		}
		out.DropoffAdded = true
		// Stamp the destination onto the existing pickup so the now-two-leg move is
		// optimizer-visible (#34) — the optimizer models the move off the pickup row.
		if err := setPickupDestination(); err != nil {
			return out, err
		}

	default:
		// Leg shape unchanged. If the type changed between the two two-leg kinds
		// (relocation ↔ redeployment), keep route_tasks.move_type in sync so callers
		// that key on the task's move_type stay consistent with the move.
		if twoLeg(newType) && oldType != newType {
			if _, err := ext.Exec(ext.Rebind(`
				UPDATE route_tasks SET move_type = ?, updated_at = ?
				WHERE move_request_id = ? AND shift_id = ? AND is_completed = 0 AND is_deleted = false`),
				newType, now, moveReqID, shiftID); err != nil {
				return out, fmt.Errorf("reconcile: sync move_type: %w", err)
			}
		}
		// Apply any destination/address change to the existing drop-off(s), and keep the
		// two-leg move's pickup destination hint in sync so a re-opt after an address edit
		// routes to the NEW destination, not the stale one (#34).
		if addressChanged && dest != nil {
			if err := updateDropoffDests(); err != nil {
				return out, err
			}
			if twoLeg(newType) {
				if err := setPickupDestination(); err != nil {
					return out, err
				}
			}
		}
	}

	return out, nil
}
