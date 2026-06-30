package itinerary

import (
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

// A store move (non-relocation) opens room by 1 then inserts only the pickup row.
func TestAddMove_StoreInsertsPickupOnly(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec("(?s)UPDATE route_tasks SET sequence_order = sequence_order \\+ \\$1.*WHERE shift_id = \\$2 AND sequence_order >= \\$3").
		WithArgs(1, "shift-1", 3).
		WillReturnResult(sqlmock.NewResult(0, 4))

	mock.ExpectExec("(?s)INSERT INTO route_tasks.*task_type.*VALUES").
		WithArgs(
			sqlmock.AnyArg(), "shift-1", "bin-1", 42, 3, string(Pickup),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "pickup addr", sqlmock.AnyArg(),
			"move-1", "store", sqlmock.AnyArg(), sqlmock.AnyArg(), int64(1700000000),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	n, err := AddMove(db, "shift-1", MovePlacement{
		InsertSeq:     3,
		MoveRequestID: "move-1",
		BinID:         "bin-1",
		BinNumber:     42,
		MoveType:      "store",
		PickupAddress: "pickup addr",
		Now:           1700000000,
	})
	if err != nil {
		t.Fatalf("AddMove(store) = %v, want nil", err)
	}
	if n != 1 {
		t.Fatalf("AddMove(store) returned %d, want 1", n)
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

	// pickup INSERT (fill_percentage column, no destination_* columns)
	mock.ExpectExec("(?s)INSERT INTO route_tasks.*fill_percentage,\\s*move_request_id.*VALUES").
		WithArgs(
			sqlmock.AnyArg(), "shift-1", "bin-1", 42, 3, string(Pickup),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "pickup addr", sqlmock.AnyArg(),
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
