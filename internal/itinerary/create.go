package itinerary

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// ── The single typed writer (Phase 5) ────────────────────────────────────────
//
// insertTask is THE insert path for route_tasks births. Intent methods assemble
// an explicit column list; the writer turns it into one INSERT. The per-column
// policy is the load-bearing design point of the creation migration:
//
//   - a column NOT in the list is OMITTED from the INSERT → the DDL default
//     lands (e.g. time_window_type 'soft', service_duration_seconds 300);
//   - a column in the list with a nil value binds a REAL NULL, bypassing the
//     DDL default (CreateShiftWithTasks semantics — Slice 3).
//
// Unifying the two blindly silently flips stored values between paths, so each
// intent method owns its exact column contract and pins it with a sqlmock test.
// Column names are package-internal literals — never user input.

type taskCol struct {
	name string
	val  interface{}
}

func insertTask(ext sqlx.Ext, cols []taskCol) error {
	names := make([]string, len(cols))
	marks := make([]string, len(cols))
	vals := make([]interface{}, len(cols))
	for i, c := range cols {
		names[i] = c.name
		marks[i] = "?"
		vals[i] = c.val
	}
	q := "INSERT INTO route_tasks (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
	_, err := ext.Exec(ext.Rebind(q), vals...)
	return err
}

// ── Manager add_tasks intent methods (Slice 2a — byte-identical migration) ───
//
// These carry the EXACT 18-column contract of the historical UpdateShift
// add_tasks INSERT (shift_tasks_edit.go): every column below is bound for every
// type (nils → NULL), matching the legacy single-statement behavior, so rows
// are indistinguishable from pre-migration ones. Type-specific quirks that are
// PRESERVED deliberately (candidates for the flagged Slice-2b/4 fixes, not
// silent changes here):
//
//   - move legs carry NO bin_number / fill_percentage (reads COALESCE it);
//   - the PATCH-added pickup carries NO destination_latitude/longitude —
//     invisible to the OR-Tools shipment builder (the #34 regression on this
//     path; the create-with-tasks and AddMove paths do stamp it);
//   - dropoffs are born with NULL address (only destination_address is set);
//   - a pickup whose move request has NULL origin coords inserts at 0,0.
//
// The caller resolves all data (bin/potential-location/move lookups stay at the
// boundary where their 400-vs-500 semantics live) and assigns sequence_order.

// NewCollection is a resolved collection task for the add_tasks path.
type NewCollection struct {
	Seq            int
	BinID          string
	BinNumber      int
	Lat, Lng       float64
	Address        string // composed "street, city zip" by the caller
	FillPercentage int
	AddedBy        string
	AdditionReason string
	Now            int64
}

// AddCollection inserts one mid-shift collection task. Returns the new task id.
func AddCollection(ext sqlx.Ext, shiftID string, c NewCollection) (string, error) {
	id := uuid.New().String()
	if err := insertTask(ext, []taskCol{
		{"id", id}, {"shift_id", shiftID}, {"task_type", string(Collection)},
		{"bin_id", c.BinID}, {"potential_location_id", nil}, {"move_request_id", nil},
		{"bin_number", c.BinNumber}, {"latitude", c.Lat}, {"longitude", c.Lng},
		{"address", c.Address}, {"destination_address", nil},
		{"fill_percentage", c.FillPercentage}, {"sequence_order", c.Seq},
		{"is_completed", 0}, {"created_at", c.Now}, {"updated_at", c.Now},
		{"added_by", c.AddedBy}, {"addition_reason", c.AdditionReason},
	}); err != nil {
		return "", fmt.Errorf("insert collection: %w", err)
	}
	return id, nil
}

// NewPlacement is a resolved placement task for the add_tasks path.
type NewPlacement struct {
	Seq                 int
	PotentialLocationID string
	Lat, Lng            float64
	Address             string // the potential_locations.address column
	AddedBy             string
	AdditionReason      string
	Now                 int64
}

// AddPlacement inserts one mid-shift placement task. Returns the new task id.
func AddPlacement(ext sqlx.Ext, shiftID string, p NewPlacement) (string, error) {
	id := uuid.New().String()
	if err := insertTask(ext, []taskCol{
		{"id", id}, {"shift_id", shiftID}, {"task_type", string(Placement)},
		{"bin_id", nil}, {"potential_location_id", p.PotentialLocationID}, {"move_request_id", nil},
		{"bin_number", nil}, {"latitude", p.Lat}, {"longitude", p.Lng},
		{"address", p.Address}, {"destination_address", nil},
		{"fill_percentage", nil}, {"sequence_order", p.Seq},
		{"is_completed", 0}, {"created_at", p.Now}, {"updated_at", p.Now},
		{"added_by", p.AddedBy}, {"addition_reason", p.AdditionReason},
	}); err != nil {
		return "", fmt.Errorf("insert placement: %w", err)
	}
	return id, nil
}

// NewMoveLeg is ONE resolved leg (pickup or dropoff) of a move for the
// add_tasks path — distinct from AddMove, which assembles both legs at once
// with the richer create-parity columns. The caller resolves coordinates per
// the historical rules (pickup ← move original_*, dropoff ← new_* or the
// shift's warehouse snapshot).
type NewMoveLeg struct {
	Seq                int
	Type               TaskType // Pickup or Dropoff
	MoveRequestID      string
	BinID              string
	Lat, Lng           float64
	Address            *string // pickup: origin address or nil; dropoff: nil (legacy quirk)
	DestinationAddress *string // dropoff: destination or nil; pickup: nil (legacy quirk)
	AddedBy            string
	AdditionReason     string
	Now                int64
}

// AddMoveLeg inserts one mid-shift move leg. Returns the new task id.
func AddMoveLeg(ext sqlx.Ext, shiftID string, l NewMoveLeg) (string, error) {
	if l.Type != Pickup && l.Type != Dropoff {
		return "", fmt.Errorf("AddMoveLeg: task type %q is not a move leg", l.Type)
	}
	id := uuid.New().String()
	if err := insertTask(ext, []taskCol{
		{"id", id}, {"shift_id", shiftID}, {"task_type", string(l.Type)},
		{"bin_id", l.BinID}, {"potential_location_id", nil}, {"move_request_id", l.MoveRequestID},
		{"bin_number", nil}, {"latitude", l.Lat}, {"longitude", l.Lng},
		{"address", l.Address}, {"destination_address", l.DestinationAddress},
		{"fill_percentage", nil}, {"sequence_order", l.Seq},
		{"is_completed", 0}, {"created_at", l.Now}, {"updated_at", l.Now},
		{"added_by", l.AddedBy}, {"addition_reason", l.AdditionReason},
	}); err != nil {
		return "", fmt.Errorf("insert %s: %w", l.Type, err)
	}
	return id, nil
}
