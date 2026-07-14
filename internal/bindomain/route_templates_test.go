package bindomain

import (
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
)

func mockExt(t *testing.T) (*sqlx.DB, sqlmock.Sqlmock) {
	t.Helper()
	mdb, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return sqlx.NewDb(mdb, "sqlmock"), mock
}

// A deactivated bin is deleted from every template it belongs to, and each affected
// template's bin_count is recomputed from the surviving rows (one recount UPDATE
// scoped to exactly the touched routes).
func TestPruneFromRouteTemplates_RemovesAndRecounts(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery(`(?s)SELECT DISTINCT route_id FROM route_bins WHERE bin_id = `).
		WithArgs("b1").
		WillReturnRows(sqlmock.NewRows([]string{"route_id"}).AddRow("r1").AddRow("r2"))
	mock.ExpectExec(`(?s)DELETE FROM route_bins WHERE bin_id = `).
		WithArgs("b1").
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(`(?s)UPDATE routes SET bin_count = .*WHERE id IN `).
		WithArgs(int64(100), "r1", "r2").
		WillReturnResult(sqlmock.NewResult(0, 2))

	n, err := PruneFromRouteTemplates(db, "b1", 100)
	if err != nil {
		t.Fatalf("PruneFromRouteTemplates: %v", err)
	}
	if n != 2 {
		t.Fatalf("pruned = %d, want 2 (distinct templates)", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A bin that is on no template is a clean no-op — no DELETE, no recount UPDATE.
func TestPruneFromRouteTemplates_NoOpWhenAbsent(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery(`(?s)SELECT DISTINCT route_id FROM route_bins WHERE bin_id = `).
		WithArgs("b1").
		WillReturnRows(sqlmock.NewRows([]string{"route_id"}))
	// No further statements expected: absent bin short-circuits before the delete.

	n, err := PruneFromRouteTemplates(db, "b1", 100)
	if err != nil {
		t.Fatalf("PruneFromRouteTemplates: %v", err)
	}
	if n != 0 {
		t.Fatalf("pruned = %d, want 0", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
