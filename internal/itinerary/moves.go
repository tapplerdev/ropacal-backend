package itinerary

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// MovePlacement is the data needed to assemble a move's stops on a shift.
type MovePlacement struct {
	InsertSeq      int
	MoveRequestID  string
	BinID          string
	BinNumber      int
	FillPercentage *int   // bin fields are nullable; nil → NULL (matches prior behavior)
	MoveType       string // relocation/redeployment/store/pickup_only — all two-leg (pickup + dropoff)

	PickupLat, PickupLng *float64 // bin's current location
	PickupAddress        string

	// The move's destination: the new location (relocation/redeployment) or the current
	// warehouse (store/pickup_only). The caller resolves it (never trust a possibly-stale
	// move.new_latitude for store — re-resolve the current warehouse config).
	DropoffLat, DropoffLng float64
	DropoffAddress         string

	// Audit: who added this move to the shift mid-shift, and why. nil for tasks
	// created with the shift (no specific adder). Surfaced in the shift edit-history.
	AddedBy        *string
	AdditionReason *string

	Now int64
}

// AddMove assembles a move's route_tasks on a shift: a pickup at the bin's current
// location + a dropoff at the destination immediately after it. Every move type is
// two-leg (relocation/redeployment → the new location; store/pickup_only → the current
// warehouse). Both rows share the move_request_id (the pickup→dropoff pair the optimizer
// treats as one shipment, and CountStops treats as one logical bin). Downstream tasks are
// shifted to open room at InsertSeq. Runs inside the caller's transaction (ext). Returns
// the number of tasks added (always 2) so the caller can adjust shift.total_bins.
func AddMove(ext sqlx.Ext, shiftID string, p MovePlacement) (int, error) {
	// Every move relocates a bin from A→B, so all move types are two-leg: a pickup at the
	// bin's current location + a dropoff at the destination. The destination is the move's
	// new location (relocation/redeployment) or the current warehouse (store/pickup_only);
	// the caller resolves it into DropoffLat/Lng. A 0,0 destination means the caller failed
	// to resolve it — reject rather than route the bin to null island.
	if p.DropoffLat == 0 && p.DropoffLng == 0 {
		return 0, fmt.Errorf("AddMove: move %s (%s): %w", p.MoveRequestID, p.MoveType, ErrMissingDestination)
	}
	binsAdded := 2

	// Open room at the insert position.
	if _, err := ext.Exec(ext.Rebind(`
		UPDATE route_tasks SET sequence_order = sequence_order + ?
		WHERE shift_id = ? AND sequence_order >= ?`), binsAdded, shiftID, p.InsertSeq); err != nil {
		return 0, fmt.Errorf("shift sequence order: %w", err)
	}

	// The PICKUP also carries the DESTINATION (where the bin is headed) in its
	// destination_* columns so the optimizer can model the move as a pickup→dropoff
	// shipment (#34): it reads a move off the pickup row alone (its case "pickup" builds
	// the shipment from the pickup's latitude/longitude + destination_latitude/longitude —
	// there is no case "dropoff"). CreateShiftWithTasks already stamps this for moves
	// created at shift-start; this keeps AddMove (assign-to-shift / reopt) at parity.
	pickupDestLat, pickupDestLng, pickupDestAddr := p.DropoffLat, p.DropoffLng, p.DropoffAddress

	// Pickup at the bin's current location.
	if _, err := ext.Exec(ext.Rebind(`
		INSERT INTO route_tasks (
			id, shift_id, bin_id, bin_number, sequence_order, task_type,
			latitude, longitude, address, fill_percentage,
			destination_latitude, destination_longitude, destination_address,
			move_request_id, move_type, added_by, addition_reason, is_completed, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`),
		uuid.New().String(), shiftID, p.BinID, p.BinNumber, p.InsertSeq, string(Pickup),
		p.PickupLat, p.PickupLng, p.PickupAddress, p.FillPercentage,
		pickupDestLat, pickupDestLng, pickupDestAddr,
		p.MoveRequestID, p.MoveType, p.AddedBy, p.AdditionReason, p.Now); err != nil {
		return 0, fmt.Errorf("insert pickup: %w", err)
	}

	// Dropoff at the destination, immediately after the pickup.
	dropoffSeq := p.InsertSeq + 1
	if _, err := ext.Exec(ext.Rebind(`
		INSERT INTO route_tasks (
			id, shift_id, bin_id, bin_number, sequence_order, task_type,
			latitude, longitude, address,
			destination_latitude, destination_longitude, destination_address,
			move_request_id, move_type, added_by, addition_reason, is_completed, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`),
		uuid.New().String(), shiftID, p.BinID, p.BinNumber, dropoffSeq, string(Dropoff),
		p.DropoffLat, p.DropoffLng, p.DropoffAddress,
		p.DropoffLat, p.DropoffLng, p.DropoffAddress,
		p.MoveRequestID, p.MoveType, p.AddedBy, p.AdditionReason, p.Now); err != nil {
		return 0, fmt.Errorf("insert dropoff: %w", err)
	}

	// Invariant: pickup precedes dropoff (deterministic here; guard preserved from
	// the original to catch any future sequencing regression). Capture the verify
	// errors — a read failure must surface as a real error, not leave ps=ds=0 and
	// trip the ps>=ds guard with a misleading "invalid sequence order".
	var ps, ds int
	if err := sqlx.Get(ext, &ps, ext.Rebind(`SELECT sequence_order FROM route_tasks WHERE shift_id = ? AND move_request_id = ? AND task_type = 'pickup'`), shiftID, p.MoveRequestID); err != nil {
		return 0, fmt.Errorf("verify pickup sequence: %w", err)
	}
	if err := sqlx.Get(ext, &ds, ext.Rebind(`SELECT sequence_order FROM route_tasks WHERE shift_id = ? AND move_request_id = ? AND task_type = 'dropoff'`), shiftID, p.MoveRequestID); err != nil {
		return 0, fmt.Errorf("verify dropoff sequence: %w", err)
	}
	if ps >= ds {
		return 0, fmt.Errorf("invalid sequence order: pickup at %d, dropoff at %d", ps, ds)
	}
	return binsAdded, nil
}
