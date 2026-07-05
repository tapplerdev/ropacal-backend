package itinerary

import (
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"ropacal-backend/internal/models"
)

// reuses mockExt + the "postgres" rebind path from writers_test.go.

func updateStop(id string) OrderedStop { return OrderedStop{Task: models.RouteTask{ID: id}} }

func warehouseStop(id string, lat, lng float64) OrderedStop {
	addr := "warehouse"
	return OrderedStop{Task: models.RouteTask{ID: id, TaskType: "warehouse_stop", Latitude: lat, Longitude: lng, Address: &addr}, Insert: true}
}

// Reopt (isFirst=false): existing rows get a sequence-only UPDATE in order, warehouse stops are
// inserted after, then Resequence (the warehouse-last renumber) runs — never the first-opt one.
func TestApplyOrder_Reopt_SeqOnlyUpdatesInsertThenResequence(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)UPDATE route_tasks SET sequence_order = \$1, updated_at = \$2 WHERE id = \$3`).
		WithArgs(1, sqlmock.AnyArg(), "a").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE route_tasks SET sequence_order = \$1, updated_at = \$2 WHERE id = \$3`).
		WithArgs(2, sqlmock.AnyArg(), "b").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)INSERT INTO route_tasks.*VALUES`).
		WithArgs("wh", "shift-1", "warehouse_stop", 3,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Resequence = warehouse-last renumber
	mock.ExpectExec(`(?s)ROW_NUMBER\(\) OVER \(\s*ORDER BY is_completed DESC.*warehouse_stop.*WHERE shift_id = \$1`).
		WithArgs("shift-1").WillReturnResult(sqlmock.NewResult(0, 3))

	stops := []OrderedStop{updateStop("a"), updateStop("b"), warehouseStop("wh", 37.1, -121.2)}
	if err := ApplyOrder(db, "shift-1", stops, false); err != nil {
		t.Fatalf("ApplyOrder(reopt): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// First-opt (isFirst=true): existing rows get sequence + COORDINATE refresh, warehouse inserted,
// then the first-opt renumber (sequence_order ASC, created_at ASC — NO warehouse-last). The
// distinct UPDATE shape (latitude=) and distinct renumber are the behavior we must preserve.
func TestApplyOrder_FirstOpt_CoordUpdateInsertThenFirstOptRenumber(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	addr := "123 Main"
	// coord-refresh UPDATE (note: latitude/longitude/address present)
	mock.ExpectExec(`(?s)UPDATE route_tasks SET sequence_order = \$1, latitude = \$2, longitude = \$3, address = \$4, updated_at = \$5 WHERE id = \$6`).
		WithArgs(1, 12.5, -98.0, &addr, sqlmock.AnyArg(), "x").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)INSERT INTO route_tasks.*VALUES`).
		WithArgs("wh", "shift-1", "warehouse_stop", 2,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// first-opt renumber = sequence_order/created_at order, NO is_completed/warehouse-last
	mock.ExpectExec(`(?s)ROW_NUMBER\(\) OVER \(ORDER BY sequence_order ASC, created_at ASC, id ASC\).*WHERE shift_id = \$1`).
		WithArgs("shift-1").WillReturnResult(sqlmock.NewResult(0, 2))

	stops := []OrderedStop{
		{Task: models.RouteTask{ID: "x", Latitude: 12.5, Longitude: -98.0, Address: &addr}},
		warehouseStop("wh", 0, 0),
	}
	if err := ApplyOrder(db, "shift-1", stops, true); err != nil {
		t.Fatalf("ApplyOrder(first): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// No stops on reopt → no UPDATE/INSERT, just Resequence.
func TestApplyOrder_Reopt_EmptyStopsResequencesOnly(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)ROW_NUMBER\(\) OVER \(\s*ORDER BY is_completed DESC`).
		WithArgs("shift-1").WillReturnResult(sqlmock.NewResult(0, 0))

	if err := ApplyOrder(db, "shift-1", nil, false); err != nil {
		t.Fatalf("ApplyOrder(empty reopt): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
