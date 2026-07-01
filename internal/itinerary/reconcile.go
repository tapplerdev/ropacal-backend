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

// ErrMissingDestination is returned when a move has no destination (address + coords) to
// reconcile to — the caller maps it to a 400 instead of silently leaving a bad route.
var ErrMissingDestination = errors.New("move requires a destination (address + coordinates)")

// ReconcileMove brings a move's route_tasks in line with a move_type/address change on the
// move's shift — the itinerary domain owns these route_tasks writes so the handler doesn't
// hand-roll them. Every move is two-leg (a pickup + a drop-off at the destination); the
// destination is the new location (relocation/redeployment) or the current warehouse
// (store/pickup_only), which the CALLER resolves and passes as `dest`. Reconcile re-asserts
// the correct two-leg shape: ensure exactly one drop-off at `dest`, refresh the pickup's
// destination hint (#34), and sync move_type on both legs. A legacy one-leg move (created
// before every move became two-leg) gets its missing drop-off added.
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

	// Every move is two-leg with a destination (#37): relocation/redeployment → the new
	// location; store/pickup_only → the current warehouse. The caller resolves `dest` for
	// the (possibly changed) move type. Reconcile runs only on a move_type/address change,
	// so it re-asserts the correct two-leg shape: exactly one dropoff at `dest`, the pickup's
	// destination hint (#34) refreshed, and move_type synced on both legs. oldType is no
	// longer needed for a leg decision (all moves are two-leg).
	_ = oldType
	_ = addressChanged
	if dest == nil {
		return out, ErrMissingDestination
	}

	pickupSeq, pickupFound := 0, false
	var binID *string
	var binNumber *int
	var dropoffs []string
	for _, t := range tasks {
		switch t.TaskType {
		case "pickup":
			pickupSeq, binID, binNumber, pickupFound = t.Seq, t.BinID, t.BinNumber, true
		case "dropoff":
			dropoffs = append(dropoffs, t.ID)
		}
	}
	if !pickupFound {
		// Move not on this shift's tasks — nothing to reconcile.
		return out, nil
	}

	if len(dropoffs) == 0 {
		// Legacy one-leg move (created before every move became two-leg): add the dropoff
		// right after the pickup, opening room first so sequence_order stays dense.
		if _, err := ext.Exec(ext.Rebind(`
			UPDATE route_tasks SET sequence_order = sequence_order + 1
			WHERE shift_id = ? AND is_deleted = false AND sequence_order >= ?`),
			shiftID, pickupSeq+1); err != nil {
			return out, fmt.Errorf("reconcile: open room for dropoff: %w", err)
		}
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
	} else {
		// Update the dropoff to the new destination + move_type. Both its latitude/longitude
		// (the nav target) AND destination_* move to `dest`, so an edit that changes where the
		// bin goes (relocation↔store: new location↔warehouse) reroutes the driver correctly.
		if _, err := ext.Exec(ext.Rebind(`
			UPDATE route_tasks
			SET latitude = ?, longitude = ?, address = ?,
			    destination_latitude = ?, destination_longitude = ?, destination_address = ?,
			    move_type = ?, updated_at = ?
			WHERE id = ?`),
			dest.Lat, dest.Lng, dest.Address,
			dest.Lat, dest.Lng, dest.Address,
			newType, now, dropoffs[0]); err != nil {
			return out, fmt.Errorf("reconcile: update dropoff: %w", err)
		}
		out.AddressUpdated = true
		// Dedupe safety: a move has exactly one dropoff; soft-delete any extras.
		if len(dropoffs) > 1 {
			if err := RemoveByIDs(ext, dropoffs[1:], by, "duplicate_dropoff_cleanup", now); err != nil {
				return out, err
			}
			out.DropoffRemoved = true
		}
	}

	// Sync the PICKUP: its destination hint (#34, so the optimizer routes to `dest`) and
	// move_type. Keyed by move_request_id + task_type so it hits the pickup row exactly.
	if _, err := ext.Exec(ext.Rebind(`
		UPDATE route_tasks
		SET destination_latitude = ?, destination_longitude = ?, destination_address = ?,
		    move_type = ?, updated_at = ?
		WHERE move_request_id = ? AND shift_id = ? AND task_type = 'pickup' AND is_completed = 0 AND is_deleted = false`),
		dest.Lat, dest.Lng, dest.Address, newType, now, moveReqID, shiftID); err != nil {
		return out, fmt.Errorf("reconcile: sync pickup: %w", err)
	}

	return out, nil
}
