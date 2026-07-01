package itinerary

import (
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func reconcileRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "task_type", "sequence_order", "bin_id", "bin_number"})
}

// relocation → store: soft-delete the move's dropoff(s).
func TestReconcileMove_RelocationToStoreRemovesDropoff(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery("(?s)SELECT id, task_type, sequence_order, bin_id, bin_number FROM route_tasks WHERE move_request_id").
		WithArgs("m1", "s1").
		WillReturnRows(reconcileRows().AddRow("p1", "pickup", 2, "b1", 5).AddRow("d1", "dropoff", 3, "b1", 5))
	mock.ExpectExec("(?s)SET is_deleted = true.*WHERE id IN.*AND is_deleted = false").
		WithArgs(int64(100), "mgr", "move_type_changed_to_store", int64(100), "d1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	out, err := ReconcileMove(db, "s1", "m1", "relocation", "store", false, nil, "mgr", 100)
	if err != nil {
		t.Fatalf("ReconcileMove: %v", err)
	}
	if !out.DropoffRemoved {
		t.Errorf("DropoffRemoved=false, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// store → relocation: open room after the pickup, then insert a complete dropoff
// row (bin_id/bin_number/lat/lng/move_type) at pickupSeq+1.
func TestReconcileMove_StoreToRelocationOpensRoomThenInserts(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery("(?s)SELECT id, task_type, sequence_order, bin_id, bin_number FROM route_tasks").
		WithArgs("m1", "s1").
		WillReturnRows(reconcileRows().AddRow("p1", "pickup", 2, "b1", 5))
	mock.ExpectExec("(?s)UPDATE route_tasks SET sequence_order = sequence_order \\+ 1.*WHERE shift_id = .*AND sequence_order >= ").
		WithArgs("s1", 3).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("(?s)INSERT INTO route_tasks.*bin_id, bin_number.*'dropoff'").
		WithArgs(
			sqlmock.AnyArg(), "s1", "b1", 5, 3,
			37.3, -121.9, "1 Dest",
			37.3, -121.9, "1 Dest",
			"m1", "relocation", int64(100),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// #34: the existing pickup gets the destination stamped so the now-two-leg move is optimizer-visible.
	mock.ExpectExec("(?s)UPDATE route_tasks.*destination_latitude.*task_type = 'pickup'").
		WithArgs(37.3, -121.9, "1 Dest", int64(100), "m1", "s1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	dest := &MoveDestination{Address: "1 Dest", Lat: 37.3, Lng: -121.9}
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

// store → relocation with NO destination → ErrMissingDestination.
func TestReconcileMove_StoreToRelocationRequiresDestination(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery("(?s)SELECT id, task_type, sequence_order, bin_id, bin_number FROM route_tasks").
		WithArgs("m1", "s1").
		WillReturnRows(reconcileRows().AddRow("p1", "pickup", 2, "b1", 5))

	_, err := ReconcileMove(db, "s1", "m1", "store", "relocation", false, nil, "mgr", 100)
	if !errors.Is(err, ErrMissingDestination) {
		t.Fatalf("ReconcileMove(no dest) = %v, want ErrMissingDestination", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// store → redeployment: redeployment is two-leg like relocation, so a dropoff must be
// added (move_type='redeployment'). Guards against reintroducing the pickup-only bug via
// the edit path.
func TestReconcileMove_StoreToRedeploymentAddsDropoff(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery("(?s)SELECT id, task_type, sequence_order, bin_id, bin_number FROM route_tasks").
		WithArgs("m1", "s1").
		WillReturnRows(reconcileRows().AddRow("p1", "pickup", 2, "b1", 5))
	mock.ExpectExec("(?s)UPDATE route_tasks SET sequence_order = sequence_order \\+ 1.*WHERE shift_id = .*AND sequence_order >= ").
		WithArgs("s1", 3).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("(?s)INSERT INTO route_tasks.*bin_id, bin_number.*'dropoff'").
		WithArgs(
			sqlmock.AnyArg(), "s1", "b1", 5, 3,
			37.3, -121.9, "1 Dest",
			37.3, -121.9, "1 Dest",
			"m1", "redeployment", int64(100),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// #34: existing pickup stamped with the destination (redeployment is two-leg like relocation).
	mock.ExpectExec("(?s)UPDATE route_tasks.*destination_latitude.*task_type = 'pickup'").
		WithArgs(37.3, -121.9, "1 Dest", int64(100), "m1", "s1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	dest := &MoveDestination{Address: "1 Dest", Lat: 37.3, Lng: -121.9}
	out, err := ReconcileMove(db, "s1", "m1", "store", "redeployment", false, dest, "mgr", 100)
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

// redeployment → store: the redeployment's dropoff must be soft-deleted (no orphan leg
// routing the driver to a stale destination).
func TestReconcileMove_RedeploymentToStoreRemovesDropoff(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery("(?s)SELECT id, task_type, sequence_order, bin_id, bin_number FROM route_tasks WHERE move_request_id").
		WithArgs("m1", "s1").
		WillReturnRows(reconcileRows().AddRow("p1", "pickup", 2, "b1", 5).AddRow("d1", "dropoff", 3, "b1", 5))
	mock.ExpectExec("(?s)SET is_deleted = true.*WHERE id IN.*AND is_deleted = false").
		WithArgs(int64(100), "mgr", "move_type_changed_to_store", int64(100), "d1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	out, err := ReconcileMove(db, "s1", "m1", "redeployment", "store", false, nil, "mgr", 100)
	if err != nil {
		t.Fatalf("ReconcileMove: %v", err)
	}
	if !out.DropoffRemoved {
		t.Errorf("DropoffRemoved=false, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// store → redeployment with NO destination → ErrMissingDestination (the missing-dest
// guard must cover redeployment too, not just relocation).
func TestReconcileMove_StoreToRedeploymentRequiresDestination(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery("(?s)SELECT id, task_type, sequence_order, bin_id, bin_number FROM route_tasks").
		WithArgs("m1", "s1").
		WillReturnRows(reconcileRows().AddRow("p1", "pickup", 2, "b1", 5))

	_, err := ReconcileMove(db, "s1", "m1", "store", "redeployment", false, nil, "mgr", 100)
	if !errors.Is(err, ErrMissingDestination) {
		t.Fatalf("ReconcileMove(no dest) = %v, want ErrMissingDestination", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Address change on a two-leg move (type unchanged): updates the dropoff's destination
// AND re-stamps the pickup's destination hint (#34) so a re-opt routes to the NEW place,
// not the stale one.
func TestReconcileMove_AddressChangeSyncsPickupDestination(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery("(?s)SELECT id, task_type, sequence_order, bin_id, bin_number FROM route_tasks WHERE move_request_id").
		WithArgs("m1", "s1").
		WillReturnRows(reconcileRows().AddRow("p1", "pickup", 2, "b1", 5).AddRow("d1", "dropoff", 3, "b1", 5))
	// dropoff destination updated (by id)
	mock.ExpectExec("(?s)UPDATE route_tasks\\s+SET destination_address = .*WHERE id = ").
		WithArgs("2 New", 38.0, -122.0, int64(100), "d1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// pickup destination re-stamped (by move_request_id + task_type='pickup')
	mock.ExpectExec("(?s)UPDATE route_tasks.*destination_latitude.*task_type = 'pickup'").
		WithArgs(38.0, -122.0, "2 New", int64(100), "m1", "s1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	dest := &MoveDestination{Address: "2 New", Lat: 38.0, Lng: -122.0}
	out, err := ReconcileMove(db, "s1", "m1", "relocation", "relocation", true, dest, "mgr", 100)
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
