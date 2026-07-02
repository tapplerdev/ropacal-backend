package itinerary

import (
	"errors"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
)

// mockExt returns an *sqlx.DB backed by sqlmock (satisfies sqlx.Ext) plus the
// mock to set expectations on. Driver name "postgres" so Rebind turns ? into $N,
// exercising the same placeholder path the real callers hit.
func mockExt(t *testing.T) (*sqlx.DB, sqlmock.Sqlmock) {
	t.Helper()
	raw, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return sqlx.NewDb(raw, "postgres"), mock
}

// RemoveByIDs with no ids must not touch the DB at all.
func TestRemoveByIDs_EmptyIsNoOp(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	if err := RemoveByIDs(db, nil, "by", "reason", 1700000000); err != nil {
		t.Fatalf("RemoveByIDs(empty) = %v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("empty id list should issue no queries: %v", err)
	}
}

// RemoveByIDs soft-deletes the given ids, and the UPDATE carries the
// is_deleted=false guard that makes re-removal idempotent.
func TestRemoveByIDs_SoftDeleteCarriesIdempotencyGuard(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec("(?s)SET is_deleted = true.*WHERE id IN.*AND is_deleted = false").
		WithArgs(int64(1700000000), "mgr", "cancelled", int64(1700000000), "a", "b").
		WillReturnResult(sqlmock.NewResult(0, 2))

	if err := RemoveByIDs(db, []string{"a", "b"}, "mgr", "cancelled", 1700000000); err != nil {
		t.Fatalf("RemoveByIDs: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Resequence fires exactly one UPDATE carrying the ROW_NUMBER()/sequence_order
// renumber, scoped to the given shift.
func TestResequence_FiresRenumberUpdate(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec("(?s)UPDATE route_tasks.*sequence_order = sub.rn.*ROW_NUMBER\\(\\).*WHERE shift_id = \\$1").
		WithArgs("shift-1").
		WillReturnResult(sqlmock.NewResult(0, 5))

	if err := Resequence(db, "shift-1"); err != nil {
		t.Fatalf("Resequence: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A store move is two-leg like a relocation: pickup at the bin + dropoff at the WAREHOUSE
// (the caller resolves the current-warehouse coords into DropoffLat/Lng). This is what
// makes store finalize (completion fires on the dropoff) and optimize.
func TestAddMove_StoreInsertsPickupAndWarehouseDropoff(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec("(?s)UPDATE route_tasks SET sequence_order = sequence_order \\+ \\$1.*WHERE shift_id = \\$2 AND sequence_order >= \\$3").
		WithArgs(2, "shift-1", 3).
		WillReturnResult(sqlmock.NewResult(0, 4))

	// pickup INSERT — carries destination_* = the warehouse (store is optimizer-visible now).
	mock.ExpectExec("(?s)INSERT INTO route_tasks.*fill_percentage,\\s*destination_latitude.*VALUES").
		WithArgs(
			sqlmock.AnyArg(), "shift-1", "bin-1", 42, 3, string(Pickup),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "pickup addr", sqlmock.AnyArg(),
			37.6368, -122.1269, "Warehouse",
			"move-1", "store", sqlmock.AnyArg(), sqlmock.AnyArg(), int64(1700000000),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// dropoff INSERT at the warehouse (seq = InsertSeq+1 = 4)
	mock.ExpectExec("(?s)INSERT INTO route_tasks.*destination_latitude, destination_longitude, destination_address,.*VALUES").
		WithArgs(
			sqlmock.AnyArg(), "shift-1", "bin-1", 42, 4, string(Dropoff),
			37.6368, -122.1269, "Warehouse",
			37.6368, -122.1269, "Warehouse",
			"move-1", "store", sqlmock.AnyArg(), sqlmock.AnyArg(), int64(1700000000),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("(?s)SELECT sequence_order FROM route_tasks WHERE shift_id = \\$1 AND move_request_id = \\$2 AND task_type = 'pickup'").
		WithArgs("shift-1", "move-1").
		WillReturnRows(sqlmock.NewRows([]string{"sequence_order"}).AddRow(3))
	mock.ExpectQuery("(?s)SELECT sequence_order FROM route_tasks WHERE shift_id = \\$1 AND move_request_id = \\$2 AND task_type = 'dropoff'").
		WithArgs("shift-1", "move-1").
		WillReturnRows(sqlmock.NewRows([]string{"sequence_order"}).AddRow(4))

	n, err := AddMove(db, "shift-1", MovePlacement{
		InsertSeq:      3,
		MoveRequestID:  "move-1",
		BinID:          "bin-1",
		BinNumber:      42,
		MoveType:       "store",
		PickupAddress:  "pickup addr",
		DropoffLat:     37.6368,
		DropoffLng:     -122.1269,
		DropoffAddress: "Warehouse",
		Now:            1700000000,
	})
	if err != nil {
		t.Fatalf("AddMove(store) = %v, want nil", err)
	}
	if n != 2 {
		t.Fatalf("AddMove(store) returned %d, want 2", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// AddMove rejects a move with no destination (0,0) rather than routing the bin to null
// island — the caller must resolve the destination (warehouse for store/pickup_only).
func TestAddMove_RejectsMissingDestination(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()
	// No query expectations: the 0,0 guard fires before any DB write.
	_, err := AddMove(db, "shift-1", MovePlacement{
		InsertSeq:     3,
		MoveRequestID: "move-1",
		BinID:         "bin-1",
		BinNumber:     42,
		MoveType:      "store",
		PickupAddress: "pickup addr",
		Now:           1700000000,
	})
	if !errors.Is(err, ErrMissingDestination) {
		t.Fatalf("AddMove(no dest) = %v, want ErrMissingDestination", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A relocation opens room by 2, inserts pickup + dropoff, then verifies the two
// rows' sequence_order. With pickup<dropoff it returns 2, nil.
func TestAddMove_RelocationInsertsPickupAndDropoff(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec("(?s)UPDATE route_tasks SET sequence_order = sequence_order \\+ \\$1.*WHERE shift_id = \\$2 AND sequence_order >= \\$3").
		WithArgs(2, "shift-1", 3).
		WillReturnResult(sqlmock.NewResult(0, 4))

	// pickup INSERT — now ALSO carries destination_* (= the move's dropoff coords) so the
	// optimizer can model it as a shipment (#34). Distinguished from the dropoff INSERT by
	// the fill_percentage column that precedes destination_latitude.
	mock.ExpectExec("(?s)INSERT INTO route_tasks.*fill_percentage,\\s*destination_latitude.*VALUES").
		WithArgs(
			sqlmock.AnyArg(), "shift-1", "bin-1", 42, 3, string(Pickup),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "pickup addr", sqlmock.AnyArg(),
			37.3, -121.9, "dropoff addr",
			"move-1", "relocation", sqlmock.AnyArg(), sqlmock.AnyArg(), int64(1700000000),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// dropoff INSERT (seq = InsertSeq+1 = 4)
	mock.ExpectExec("(?s)INSERT INTO route_tasks.*destination_latitude, destination_longitude, destination_address,.*VALUES").
		WithArgs(
			sqlmock.AnyArg(), "shift-1", "bin-1", 42, 4, string(Dropoff),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "dropoff addr",
			sqlmock.AnyArg(), sqlmock.AnyArg(), "dropoff addr",
			"move-1", "relocation", sqlmock.AnyArg(), sqlmock.AnyArg(), int64(1700000000),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// verify pickup sequence_order
	mock.ExpectQuery("(?s)SELECT sequence_order FROM route_tasks WHERE shift_id = \\$1 AND move_request_id = \\$2 AND task_type = 'pickup'").
		WithArgs("shift-1", "move-1").
		WillReturnRows(sqlmock.NewRows([]string{"sequence_order"}).AddRow(3))

	// verify dropoff sequence_order
	mock.ExpectQuery("(?s)SELECT sequence_order FROM route_tasks WHERE shift_id = \\$1 AND move_request_id = \\$2 AND task_type = 'dropoff'").
		WithArgs("shift-1", "move-1").
		WillReturnRows(sqlmock.NewRows([]string{"sequence_order"}).AddRow(4))

	n, err := AddMove(db, "shift-1", MovePlacement{
		InsertSeq:      3,
		MoveRequestID:  "move-1",
		BinID:          "bin-1",
		BinNumber:      42,
		MoveType:       "relocation",
		PickupAddress:  "pickup addr",
		DropoffLat:     37.3,
		DropoffLng:     -121.9,
		DropoffAddress: "dropoff addr",
		Now:            1700000000,
	})
	if err != nil {
		t.Fatalf("AddMove(relocation) = %v, want nil", err)
	}
	if n != 2 {
		t.Fatalf("AddMove(relocation) returned %d, want 2", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Redeployment (warehouse/in_storage bin → placement) is a two-leg move like a
// relocation: it must insert BOTH a pickup and a dropoff so the dropoff completion
// finalizes the move (relocates the bin + converts the source potential_location).
func TestAddMove_RedeploymentInsertsPickupAndDropoff(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec("(?s)UPDATE route_tasks SET sequence_order = sequence_order \\+ \\$1.*WHERE shift_id = \\$2 AND sequence_order >= \\$3").
		WithArgs(2, "shift-1", 3).
		WillReturnResult(sqlmock.NewResult(0, 4))

	// pickup INSERT — carries destination_* (redeployment is two-leg, so the pickup is
	// optimizer-visible via #34, same as relocation).
	mock.ExpectExec("(?s)INSERT INTO route_tasks.*fill_percentage,\\s*destination_latitude.*VALUES").
		WithArgs(
			sqlmock.AnyArg(), "shift-1", "bin-1", 42, 3, string(Pickup),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "pickup addr", sqlmock.AnyArg(),
			37.3, -121.9, "dropoff addr",
			"move-1", "redeployment", sqlmock.AnyArg(), sqlmock.AnyArg(), int64(1700000000),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// dropoff INSERT (seq = InsertSeq+1 = 4)
	mock.ExpectExec("(?s)INSERT INTO route_tasks.*destination_latitude, destination_longitude, destination_address,.*VALUES").
		WithArgs(
			sqlmock.AnyArg(), "shift-1", "bin-1", 42, 4, string(Dropoff),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "dropoff addr",
			sqlmock.AnyArg(), sqlmock.AnyArg(), "dropoff addr",
			"move-1", "redeployment", sqlmock.AnyArg(), sqlmock.AnyArg(), int64(1700000000),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("(?s)SELECT sequence_order FROM route_tasks WHERE shift_id = \\$1 AND move_request_id = \\$2 AND task_type = 'pickup'").
		WithArgs("shift-1", "move-1").
		WillReturnRows(sqlmock.NewRows([]string{"sequence_order"}).AddRow(3))

	mock.ExpectQuery("(?s)SELECT sequence_order FROM route_tasks WHERE shift_id = \\$1 AND move_request_id = \\$2 AND task_type = 'dropoff'").
		WithArgs("shift-1", "move-1").
		WillReturnRows(sqlmock.NewRows([]string{"sequence_order"}).AddRow(4))

	n, err := AddMove(db, "shift-1", MovePlacement{
		InsertSeq:      3,
		MoveRequestID:  "move-1",
		BinID:          "bin-1",
		BinNumber:      42,
		MoveType:       "redeployment",
		PickupAddress:  "pickup addr",
		DropoffLat:     37.3,
		DropoffLng:     -121.9,
		DropoffAddress: "dropoff addr",
		Now:            1700000000,
	})
	if err != nil {
		t.Fatalf("AddMove(redeployment) = %v, want nil", err)
	}
	if n != 2 {
		t.Fatalf("AddMove(redeployment) returned %d, want 2", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// If the verify SELECTs report pickupSeq >= dropoffSeq the invariant is violated:
// AddMove returns 0 and an "invalid sequence order" error.
func TestAddMove_RelocationInvalidSequenceOrder(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec("(?s)UPDATE route_tasks SET sequence_order = sequence_order \\+ \\$1.*WHERE shift_id = \\$2 AND sequence_order >= \\$3").
		WithArgs(2, "shift-1", 3).
		WillReturnResult(sqlmock.NewResult(0, 4))

	mock.ExpectExec("(?s)INSERT INTO route_tasks.*fill_percentage,.*VALUES").
		WithArgs(
			sqlmock.AnyArg(), "shift-1", "bin-1", 42, 3, string(Pickup),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "pickup addr", sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"move-1", "relocation", sqlmock.AnyArg(), sqlmock.AnyArg(), int64(1700000000),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("(?s)INSERT INTO route_tasks.*destination_latitude.*VALUES").
		WithArgs(
			sqlmock.AnyArg(), "shift-1", "bin-1", 42, 4, string(Dropoff),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "dropoff addr",
			sqlmock.AnyArg(), sqlmock.AnyArg(), "dropoff addr",
			"move-1", "relocation", sqlmock.AnyArg(), sqlmock.AnyArg(), int64(1700000000),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// pickup verify returns a HIGHER seq than dropoff → guard trips.
	mock.ExpectQuery("(?s)SELECT sequence_order FROM route_tasks WHERE shift_id = \\$1 AND move_request_id = \\$2 AND task_type = 'pickup'").
		WithArgs("shift-1", "move-1").
		WillReturnRows(sqlmock.NewRows([]string{"sequence_order"}).AddRow(5))

	mock.ExpectQuery("(?s)SELECT sequence_order FROM route_tasks WHERE shift_id = \\$1 AND move_request_id = \\$2 AND task_type = 'dropoff'").
		WithArgs("shift-1", "move-1").
		WillReturnRows(sqlmock.NewRows([]string{"sequence_order"}).AddRow(4))

	n, err := AddMove(db, "shift-1", MovePlacement{
		InsertSeq:      3,
		MoveRequestID:  "move-1",
		BinID:          "bin-1",
		BinNumber:      42,
		MoveType:       "relocation",
		PickupAddress:  "pickup addr",
		DropoffLat:     37.3,
		DropoffLng:     -121.9,
		DropoffAddress: "dropoff addr",
		Now:            1700000000,
	})
	if err == nil {
		t.Fatalf("AddMove(invalid order) = nil error, want non-nil")
	}
	if !strings.Contains(err.Error(), "invalid sequence order") {
		t.Fatalf("AddMove error = %q, want it to mention %q", err.Error(), "invalid sequence order")
	}
	if n != 0 {
		t.Fatalf("AddMove(invalid order) returned %d, want 0", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
