package itinerary

import (
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"ropacal-backend/internal/models"
)

// reuses mockExt + the "postgres" rebind path from writers_test.go.

func whTask(id string, lat, lng float64) models.RouteTask {
	addr := "warehouse"
	return models.RouteTask{ID: id, TaskType: "warehouse_stop", Latitude: lat, Longitude: lng, Address: &addr}
}

// Happy path: existing tasks stamped in order (ascending seq), then warehouse stops
// inserted after them (continuing the seq), then a single Resequence — all in order.
func TestApplyOrder_StampsThenInsertsThenResequences(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	// UPDATE task "a" → seq 1
	mock.ExpectExec(`(?s)UPDATE route_tasks SET sequence_order = \$1, updated_at = \$2 WHERE id = \$3`).
		WithArgs(1, sqlmock.AnyArg(), "a").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// UPDATE task "b" → seq 2
	mock.ExpectExec(`(?s)UPDATE route_tasks SET sequence_order = \$1, updated_at = \$2 WHERE id = \$3`).
		WithArgs(2, sqlmock.AnyArg(), "b").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// INSERT warehouse "wh" → seq 3 (after the two bins)
	mock.ExpectExec(`(?s)INSERT INTO route_tasks.*VALUES`).
		WithArgs("wh", "shift-1", "warehouse_stop", 3, 37.1, -121.2, sqlmock.AnyArg(), 0, false, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// trailing Resequence
	mock.ExpectExec(`(?s)UPDATE route_tasks AS rt.*ROW_NUMBER\(\).*WHERE shift_id = \$1`).
		WithArgs("shift-1").
		WillReturnResult(sqlmock.NewResult(0, 3))

	err := ApplyOrder(db, "shift-1", []string{"a", "b"}, []models.RouteTask{whTask("wh", 37.1, -121.2)}, false)
	if err != nil {
		t.Fatalf("ApplyOrder: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// No existing tasks (all stops were warehouse/unmatched): only the warehouse INSERT
// (seq starts at 1) and Resequence fire — no UPDATEs.
func TestApplyOrder_EmptyOrderedIDsInsertsWarehouseOnly(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)INSERT INTO route_tasks.*VALUES`).
		WithArgs("wh", "shift-1", "warehouse_stop", 1, 0.0, 0.0, sqlmock.AnyArg(), 0, false, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE route_tasks AS rt.*ROW_NUMBER\(\)`).
		WithArgs("shift-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := ApplyOrder(db, "shift-1", nil, []models.RouteTask{whTask("wh", 0, 0)}, false); err != nil {
		t.Fatalf("ApplyOrder: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// No warehouse stops: only the UPDATEs (in order) and Resequence — no INSERT.
func TestApplyOrder_NoNewTasksUpdatesThenResequences(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)UPDATE route_tasks SET sequence_order = \$1, updated_at = \$2 WHERE id = \$3`).
		WithArgs(1, sqlmock.AnyArg(), "only").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE route_tasks AS rt.*ROW_NUMBER\(\)`).
		WithArgs("shift-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := ApplyOrder(db, "shift-1", []string{"only"}, nil, false); err != nil {
		t.Fatalf("ApplyOrder: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// isFirst=true is not implemented yet — it must error (and issue no statements),
// so a future slice can't silently ship a half-built first-optimize path.
func TestApplyOrder_IsFirstNotYetImplemented(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	err := ApplyOrder(db, "shift-1", []string{"a"}, nil, true)
	if err == nil || !strings.Contains(err.Error(), "isFirst=true not yet implemented") {
		t.Fatalf("ApplyOrder(isFirst=true) = %v, want not-implemented error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("isFirst=true should issue no statements: %v", err)
	}
}
