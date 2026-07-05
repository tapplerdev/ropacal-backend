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

// ── Shift-birth writer (Slice 3a — CreateShiftWithTasks migration) ───────────
//
// CreatedTask carries the 30-column contract of a task born WITH its shift
// (CreateShiftWithTasks): every column is BOUND for every row — absent values
// are explicit NULLs that BYPASS the DDL defaults (time_window_type 'soft',
// service_duration_seconds 300, photo_required FALSE) — that is the historical
// create-path semantic and it differs from the add_tasks path on purpose.
//
// Fields are interface{} because this path is client-first: it binds raw JSON
// payload values (nil-safe getters) merged with DB enrichment. Typing these up
// is the shared-Resolver work (Slice 4), not this byte-identical relocation.
type CreatedTask struct {
	ID       string
	Seq      int
	TaskType string
	Lat, Lng float64

	Address, BinID, BinNumber, FillPercentage interface{}
	PotentialLocationID, NewBinNumber         interface{}
	PlacementSource, MoveRequestID            interface{}
	DestLat, DestLng, DestAddress, MoveType   interface{}
	WarehouseAction, BinsToLoad, RouteID      interface{}
	TaskLabel, TaskDescription                interface{}
	EarliestArrival, LatestArrival            interface{}
	TimeWindowType, ServiceDurationSeconds    interface{}

	TaskData      interface{} // marshaled JSON; callers default []byte("{}") — never NULL (prevents scan errors)
	PhotoRequired bool
	CreatedAt     int64
}

// InsertCreatedTask inserts one shift-birth task with create semantics
// (all 30 columns bound; added_by stays absent → NULL = "created with shift"
// in the edit-history contract).
func InsertCreatedTask(ext sqlx.Ext, shiftID string, t CreatedTask) error {
	return insertTask(ext, []taskCol{
		{"id", t.ID}, {"shift_id", shiftID}, {"sequence_order", t.Seq}, {"task_type", t.TaskType},
		{"latitude", t.Lat}, {"longitude", t.Lng}, {"address", t.Address},
		{"bin_id", t.BinID}, {"bin_number", t.BinNumber}, {"fill_percentage", t.FillPercentage},
		{"potential_location_id", t.PotentialLocationID}, {"new_bin_number", t.NewBinNumber}, {"placement_source", t.PlacementSource},
		{"move_request_id", t.MoveRequestID}, {"destination_latitude", t.DestLat},
		{"destination_longitude", t.DestLng}, {"destination_address", t.DestAddress}, {"move_type", t.MoveType},
		{"warehouse_action", t.WarehouseAction}, {"bins_to_load", t.BinsToLoad},
		{"route_id", t.RouteID}, {"task_data", t.TaskData}, {"created_at", t.CreatedAt},
		{"task_label", t.TaskLabel}, {"task_description", t.TaskDescription}, {"photo_required", t.PhotoRequired},
		{"earliest_arrival", t.EarliestArrival}, {"latest_arrival", t.LatestArrival}, {"time_window_type", t.TimeWindowType},
		{"service_duration_seconds", t.ServiceDurationSeconds},
	})
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
// add_tasks path — distinct from AddMove, which assembles both legs at once.
// The caller resolves coordinates per the historical rules (pickup ← move
// original_*, dropoff ← new_* or the shift's warehouse snapshot).
//
// Slice 2b (#34 fix on the PATCH path): legs now also carry bin_number,
// move_type and the DESTINATION coordinates. The OR-Tools request builder
// reads a move off the PICKUP row alone (latitude/longitude +
// destination_latitude/longitude — there is no case "dropoff"), so a pickup
// without DestLat/DestLng is silently excluded from optimization. This brings
// PATCH-added legs to parity with CreateShiftWithTasks and AddMove.
type NewMoveLeg struct {
	Seq                int
	Type               TaskType // Pickup or Dropoff
	MoveRequestID      string
	BinID              string
	BinNumber          *int
	FillPercentage     *int // pickup carries the bin's fill snapshot; dropoff nil
	MoveType           string
	Lat, Lng           float64
	Address            *string  // pickup: origin address; dropoff: the destination address
	DestLat, DestLng   *float64 // where the bin is headed (both legs, per convention)
	DestinationAddress *string
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
		{"bin_number", l.BinNumber}, {"latitude", l.Lat}, {"longitude", l.Lng},
		{"address", l.Address}, {"destination_address", l.DestinationAddress},
		{"destination_latitude", l.DestLat}, {"destination_longitude", l.DestLng},
		{"move_type", l.MoveType},
		{"fill_percentage", l.FillPercentage}, {"sequence_order", l.Seq},
		{"is_completed", 0}, {"created_at", l.Now}, {"updated_at", l.Now},
		{"added_by", l.AddedBy}, {"addition_reason", l.AdditionReason},
	}); err != nil {
		return "", fmt.Errorf("insert %s: %w", l.Type, err)
	}
	return id, nil
}
