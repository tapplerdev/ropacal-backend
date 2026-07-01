package itinerary

import (
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func reconcileRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "task_type", "sequence_order", "bin_id", "bin_number"})
}

// A move with an existing dropoff (the normal case, all moves are two-leg) gets its dropoff
// re-pointed to the new destination + move_type, and the pickup's destination hint synced.
// Covers relocation→store (dropoff moves to the warehouse), store→relocation, and address
// changes — all "update the dropoff to dest".
func TestReconcileMove_UpdatesExistingDropoff(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery("(?s)SELECT id, task_type, sequence_order, bin_id, bin_number FROM route_tasks WHERE move_request_id").
		WithArgs("m1", "s1").
		WillReturnRows(reconcileRows().AddRow("p1", "pickup", 2, "b1", 5).AddRow("d1", "dropoff", 3, "b1", 5))
	// dropoff re-pointed: latitude/longitude (nav target) AND destination_* both move to dest, move_type=store.
	mock.ExpectExec("(?s)UPDATE route_tasks\\s+SET latitude = .*move_type = .*WHERE id = ").
		WithArgs(37.6, -122.1, "WH", 37.6, -122.1, "WH", "store", int64(100), "d1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// pickup destination hint + move_type synced.
	mock.ExpectExec("(?s)UPDATE route_tasks\\s+SET destination_latitude = .*task_type = 'pickup'").
		WithArgs(37.6, -122.1, "WH", "store", int64(100), "m1", "s1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	dest := &MoveDestination{Address: "WH", Lat: 37.6, Lng: -122.1}
	out, err := ReconcileMove(db, "s1", "m1", "relocation", "store", false, dest, "mgr", 100)
	if err != nil {
		t.Fatalf("ReconcileMove: %v", err)
	}
	if !out.AddressUpdated {
		t.Errorf("AddressUpdated=false, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A legacy one-leg move (pickup only — created before every move became two-leg) gets its
// missing dropoff added at dest, then the pickup synced.
func TestReconcileMove_AddsDropoffForLegacyOneLeg(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery("(?s)SELECT id, task_type, sequence_order, bin_id, bin_number FROM route_tasks WHERE move_request_id").
		WithArgs("m1", "s1").
		WillReturnRows(reconcileRows().AddRow("p1", "pickup", 2, "b1", 5))
	mock.ExpectExec("(?s)UPDATE route_tasks SET sequence_order = sequence_order \\+ 1.*WHERE shift_id = .*AND sequence_order >= ").
		WithArgs("s1", 3).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("(?s)INSERT INTO route_tasks.*'dropoff'").
		WithArgs(
			sqlmock.AnyArg(), "s1", "b1", 5, 3,
			37.3, -121.9, "Dest",
			37.3, -121.9, "Dest",
			"m1", "relocation", int64(100),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("(?s)UPDATE route_tasks\\s+SET destination_latitude = .*task_type = 'pickup'").
		WithArgs(37.3, -121.9, "Dest", "relocation", int64(100), "m1", "s1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	dest := &MoveDestination{Address: "Dest", Lat: 37.3, Lng: -121.9}
	out, err := ReconcileMove(db, "s1", "m1", "store", "relocation", false, dest, "mgr", 100)
	if err != nil {
		t.Fatalf("ReconcileMove: %v", err)
	}
	if !out.DropoffAdded {
		t.Errorf("DropoffAdded=false, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// No destination → ErrMissingDestination (every move needs a destination to reconcile to).
func TestReconcileMove_RequiresDestination(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery("(?s)SELECT id, task_type, sequence_order, bin_id, bin_number FROM route_tasks WHERE move_request_id").
		WithArgs("m1", "s1").
		WillReturnRows(reconcileRows().AddRow("p1", "pickup", 2, "b1", 5).AddRow("d1", "dropoff", 3, "b1", 5))

	_, err := ReconcileMove(db, "s1", "m1", "relocation", "store", false, nil, "mgr", 100)
	if !errors.Is(err, ErrMissingDestination) {
		t.Fatalf("ReconcileMove(no dest) = %v, want ErrMissingDestination", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A move should have exactly one dropoff; extras are soft-deleted (dedupe safety).
func TestReconcileMove_DedupesExtraDropoffs(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery("(?s)SELECT id, task_type, sequence_order, bin_id, bin_number FROM route_tasks WHERE move_request_id").
		WithArgs("m1", "s1").
		WillReturnRows(reconcileRows().
			AddRow("p1", "pickup", 2, "b1", 5).
			AddRow("d1", "dropoff", 3, "b1", 5).
			AddRow("d2", "dropoff", 4, "b1", 5))
	// first dropoff updated
	mock.ExpectExec("(?s)UPDATE route_tasks\\s+SET latitude = .*move_type = .*WHERE id = ").
		WithArgs(37.3, -121.9, "Dest", 37.3, -121.9, "Dest", "relocation", int64(100), "d1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// extra dropoff soft-deleted via RemoveByIDs
	mock.ExpectExec("(?s)SET is_deleted = true.*WHERE id IN.*AND is_deleted = false").
		WithArgs(int64(100), "mgr", "duplicate_dropoff_cleanup", int64(100), "d2").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// pickup synced
	mock.ExpectExec("(?s)UPDATE route_tasks\\s+SET destination_latitude = .*task_type = 'pickup'").
		WithArgs(37.3, -121.9, "Dest", "relocation", int64(100), "m1", "s1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	dest := &MoveDestination{Address: "Dest", Lat: 37.3, Lng: -121.9}
	out, err := ReconcileMove(db, "s1", "m1", "relocation", "relocation", true, dest, "mgr", 100)
	if err != nil {
		t.Fatalf("ReconcileMove: %v", err)
	}
	if !out.DropoffRemoved {
		t.Errorf("DropoffRemoved=false, want true (extra dropoff deduped)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
