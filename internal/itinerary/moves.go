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
	MoveType       string // "relocation" → pickup + dropoff; otherwise pickup only

	PickupLat, PickupLng *float64 // bin's current location
	PickupAddress        string

	DropoffLat, DropoffLng float64 // relocation destination
	DropoffAddress         string

	Now int64
}

// AddMove assembles a move's route_tasks on a shift: a pickup at the bin's current
// location, plus — for a relocation — a dropoff at the destination immediately
// after it. Both rows share the move_request_id (the pickup→dropoff pair the
// optimizer treats as one shipment). Downstream tasks are shifted to open room at
// InsertSeq. Runs inside the caller's transaction (ext). Returns the number of
// tasks added (1 pickup, or 2 for a relocation) so the caller can adjust
// shift.total_bins.
func AddMove(ext sqlx.Ext, shiftID string, p MovePlacement) (int, error) {
	binsAdded := 1
	if p.MoveType == "relocation" {
		binsAdded = 2
	}

	// Open room at the insert position.
	if _, err := ext.Exec(ext.Rebind(`
		UPDATE route_tasks SET sequence_order = sequence_order + ?
		WHERE shift_id = ? AND sequence_order >= ?`), binsAdded, shiftID, p.InsertSeq); err != nil {
		return 0, fmt.Errorf("shift sequence order: %w", err)
	}

	// Pickup at the bin's current location.
	if _, err := ext.Exec(ext.Rebind(`
		INSERT INTO route_tasks (
			id, shift_id, bin_id, bin_number, sequence_order, task_type,
			latitude, longitude, address, fill_percentage,
			move_request_id, move_type, is_completed, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`),
		uuid.New().String(), shiftID, p.BinID, p.BinNumber, p.InsertSeq, string(Pickup),
		p.PickupLat, p.PickupLng, p.PickupAddress, p.FillPercentage,
		p.MoveRequestID, p.MoveType, p.Now); err != nil {
		return 0, fmt.Errorf("insert pickup: %w", err)
	}

	if p.MoveType != "relocation" {
		return binsAdded, nil
	}

	// Dropoff at the destination, immediately after the pickup.
	dropoffSeq := p.InsertSeq + 1
	if _, err := ext.Exec(ext.Rebind(`
		INSERT INTO route_tasks (
			id, shift_id, bin_id, bin_number, sequence_order, task_type,
			latitude, longitude, address,
			destination_latitude, destination_longitude, destination_address,
			move_request_id, move_type, is_completed, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`),
		uuid.New().String(), shiftID, p.BinID, p.BinNumber, dropoffSeq, string(Dropoff),
		p.DropoffLat, p.DropoffLng, p.DropoffAddress,
		p.DropoffLat, p.DropoffLng, p.DropoffAddress,
		p.MoveRequestID, p.MoveType, p.Now); err != nil {
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
