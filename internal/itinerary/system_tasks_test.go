package itinerary

import (
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// Skipping a pickup cascade-skips its paired incomplete dropoff (2 skipped).
func TestSkip_PickupCascadesPairedDropoff(t *testing.T) {
	db, mock := mockExtCreate(t)
	defer db.Close()

	data := []byte(`{"skip_reason":"blocked"}`)
	mrID := "m1"

	mock.ExpectExec(`UPDATE route_tasks\s+SET skipped = true, is_completed = 1, completed_at = \$1, task_data = \$2, updated_at = \$3\s+WHERE id = \$4`).
		WithArgs(int64(1700000000), data, int64(1700000000), "t-pickup").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT id FROM route_tasks\s+WHERE shift_id = \$1 AND move_request_id = \$2 AND task_type = 'dropoff' AND is_completed = 0`).
		WithArgs("s1", "m1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("t-dropoff"))
	mock.ExpectExec(`UPDATE route_tasks\s+SET skipped = true`).
		WithArgs(int64(1700000000), data, int64(1700000000), "t-dropoff").
		WillReturnResult(sqlmock.NewResult(0, 1))

	n, err := Skip(db, "s1", "t-pickup", Pickup, &mrID, data, 1700000000)
	if err != nil || n != 2 {
		t.Fatalf("Skip pickup = (%d, %v), want (2, nil)", n, err)
	}
}

// Skipping a collection touches only that task (no pair lookup).
func TestSkip_CollectionNoCascade(t *testing.T) {
	db, mock := mockExtCreate(t)
	defer db.Close()

	mock.ExpectExec(`UPDATE route_tasks\s+SET skipped = true`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	n, err := Skip(db, "s1", "t1", Collection, nil, []byte(`{}`), 1)
	if err != nil || n != 1 {
		t.Fatalf("Skip collection = (%d, %v), want (1, nil)", n, err)
	}
}

// The sanctioned hard delete targets ONLY incomplete, non-deleted warehouse stops.
func TestPurgeIncompleteWarehouseStops(t *testing.T) {
	db, mock := mockExtCreate(t)
	defer db.Close()

	mock.ExpectExec(`DELETE FROM route_tasks\s+WHERE shift_id = \$1 AND task_type = 'warehouse_stop' AND is_completed = 0 AND is_deleted = false`).
		WithArgs("s1").
		WillReturnResult(sqlmock.NewResult(0, 3))

	n, err := PurgeIncompleteWarehouseStops(db, "s1")
	if err != nil || n != 3 {
		t.Fatalf("Purge = (%d, %v), want (3, nil)", n, err)
	}
}

// SyncPlacementRemoval selects the affected placements, batch-soft-deletes via
// RemoveByIDs, and returns them grouped by shift.
func TestSyncPlacementRemoval(t *testing.T) {
	db, mock := mockExtCreate(t)
	defer db.Close()

	mock.ExpectQuery(`SELECT rt\.shift_id, rt\.id as task_id\s+FROM route_tasks rt\s+JOIN shifts s`).
		WithArgs("pl1").
		WillReturnRows(sqlmock.NewRows([]string{"shift_id", "task_id"}).
			AddRow("sA", "t1").AddRow("sA", "t2").AddRow("sB", "t3"))
	mock.ExpectExec(`UPDATE route_tasks\s+SET is_deleted = true, deleted_at = \$1, deleted_by = \$2, deletion_reason = \$3, updated_at = \$4\s+WHERE id IN \(\$5, \$6, \$7\) AND is_deleted = false`).
		WithArgs(int64(9), "mgr", "potential_location_deleted", int64(9), "t1", "t2", "t3").
		WillReturnResult(sqlmock.NewResult(0, 3))

	m, err := SyncPlacementRemoval(db, "pl1", "mgr", "potential_location_deleted", 9)
	if err != nil {
		t.Fatalf("SyncPlacementRemoval: %v", err)
	}
	if len(m["sA"]) != 2 || len(m["sB"]) != 1 {
		t.Fatalf("grouping = %+v, want sA:2 sB:1", m)
	}
}
