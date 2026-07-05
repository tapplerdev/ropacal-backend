package moverequest

import (
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"ropacal-backend/internal/models"
)

func mockStore(t *testing.T) (Store, sqlmock.Sqlmock, *sqlx.DB) {
	t.Helper()
	raw, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	db := sqlx.NewDb(raw, "postgres")
	return NewSQLStore(db), mock, db
}

// A violation of the one-open-move partial unique index surfaces as the typed
// ErrOpenMoveExists (handlers map it to 409), never as a raw pq error.
func TestCreate_OpenMoveIndexViolation(t *testing.T) {
	store, mock, db := mockStore(t)
	defer db.Close()

	mock.ExpectExec("INSERT INTO bin_move_requests").
		WillReturnError(&pq.Error{Code: "23505", Constraint: oneOpenMoveIndex})

	err := store.Create(&models.BinMoveRequest{ID: "m1", BinID: "b1"}, nil)
	if !errors.Is(err, ErrOpenMoveExists) {
		t.Errorf("Create with one-open index violation = %v, want ErrOpenMoveExists", err)
	}
}

// Unique violations on OTHER constraints (e.g. the primary key) must pass
// through untranslated — only the one-open-move index means "duplicate open move".
func TestCreate_OtherUniqueViolationPassesThrough(t *testing.T) {
	store, mock, db := mockStore(t)
	defer db.Close()

	pkErr := &pq.Error{Code: "23505", Constraint: "bin_move_requests_pkey"}
	mock.ExpectExec("INSERT INTO bin_move_requests").WillReturnError(pkErr)

	err := store.Create(&models.BinMoveRequest{ID: "m1", BinID: "b1"}, nil)
	if errors.Is(err, ErrOpenMoveExists) {
		t.Errorf("Create with pkey violation translated to ErrOpenMoveExists; want passthrough")
	}
	if !errors.Is(err, error(pkErr)) {
		t.Errorf("Create with pkey violation = %v, want the original pq error", err)
	}
}

// ActiveForBin selects only the bin's non-terminal moves, newest first.
func TestActiveForBin(t *testing.T) {
	store, mock, db := mockStore(t)
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"id", "move_type", "status", "new_address",
		"disposal_action", "assigned_shift_id", "assigned_driver_name",
	}).AddRow("m2", "store", "assigned", "", nil, "s1", "Omar")

	mock.ExpectQuery(`(?s)SELECT mr\.id, mr\.move_type, mr\.status.*WHERE mr\.bin_id = .* AND mr\.status NOT IN \('completed', 'cancelled'\).*ORDER BY mr\.created_at DESC`).
		WithArgs("b1").
		WillReturnRows(rows)

	moves, err := store.ActiveForBin("b1")
	if err != nil {
		t.Fatalf("ActiveForBin: %v", err)
	}
	if len(moves) != 1 || moves[0].ID != "m2" || moves[0].Status != "assigned" || moves[0].AssignedDriverName != "Omar" {
		t.Errorf("ActiveForBin = %+v, want [m2 assigned Omar]", moves)
	}
}
