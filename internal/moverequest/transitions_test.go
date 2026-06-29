package moverequest

import (
	"errors"
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

// --- guardedTransition enforcement via the public transitions ---

// Cancel on a live move: one row updated, the UPDATE carries the terminal-state
// guard (status NOT IN ('completed','cancelled')), and err is nil.
func TestCancel_LiveMove_UpdatesAndCarriesTerminalGuard(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)UPDATE bin_move_requests SET status = 'cancelled'.*WHERE id = .* AND status NOT IN`).
		WithArgs(sqlmock.AnyArg(), "mr-1", "completed", "cancelled").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := Cancel(db, "mr-1", 1700000000); err != nil {
		t.Fatalf("Cancel(live) = %v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Cancel on an already-terminal move: zero rows affected → ErrInvalidTransition.
// This is the core enforcement test: an illegal transition out of a terminal
// state is blocked.
func TestCancel_TerminalMove_ReturnsErrInvalidTransition(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)UPDATE bin_move_requests SET.*status NOT IN`).
		WithArgs(sqlmock.AnyArg(), "mr-done", "completed", "cancelled").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := Cancel(db, "mr-done", 1700000000)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Cancel(terminal) = %v, want ErrInvalidTransition", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Complete on a live move: updates one row, carries the guard, err nil.
func TestComplete_LiveMove_UpdatesAndCarriesTerminalGuard(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)UPDATE bin_move_requests SET status = 'completed', completed_at = .*WHERE id = .* AND status NOT IN`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "mr-2", "completed", "cancelled").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := Complete(db, "mr-2", 1700000000); err != nil {
		t.Fatalf("Complete(live) = %v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Complete on an already-terminal move: zero rows → ErrInvalidTransition.
func TestComplete_TerminalMove_ReturnsErrInvalidTransition(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)UPDATE bin_move_requests SET.*status NOT IN`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "mr-done", "completed", "cancelled").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := Complete(db, "mr-done", 1700000000); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Complete(terminal) = %v, want ErrInvalidTransition", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// AssignToShift on a live move: shift id + status are bound literally, guard
// present, err nil.
func TestAssignToShift_LiveMove_BindsShiftAndStatus(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)UPDATE bin_move_requests SET assignment_type = 'shift', assigned_shift_id = .*status NOT IN`).
		WithArgs("shift-9", "in_progress", sqlmock.AnyArg(), "mr-3", "completed", "cancelled").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := AssignToShift(db, "mr-3", "shift-9", "in_progress", 1700000000); err != nil {
		t.Fatalf("AssignToShift(live) = %v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// AssignToShift on a terminal move: zero rows → ErrInvalidTransition.
func TestAssignToShift_TerminalMove_ReturnsErrInvalidTransition(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)UPDATE bin_move_requests SET.*status NOT IN`).
		WithArgs("shift-9", "in_progress", sqlmock.AnyArg(), "mr-done", "completed", "cancelled").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := AssignToShift(db, "mr-done", "shift-9", "in_progress", 1700000000); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("AssignToShift(terminal) = %v, want ErrInvalidTransition", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// AssignToDriver on a live move: user id bound literally, guard present.
func TestAssignToDriver_LiveMove_BindsUser(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)UPDATE bin_move_requests SET assignment_type = 'manual', assigned_user_id = .*status NOT IN`).
		WithArgs("user-7", sqlmock.AnyArg(), "mr-4", "completed", "cancelled").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := AssignToDriver(db, "mr-4", "user-7", 1700000000); err != nil {
		t.Fatalf("AssignToDriver(live) = %v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ClearAssignment on a live move: sets status='pending', carries guard.
func TestClearAssignment_LiveMove_SetsPending(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)UPDATE bin_move_requests SET assignment_type = NULL.*status = 'pending'.*status NOT IN`).
		WithArgs(sqlmock.AnyArg(), "mr-5", "completed", "cancelled").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := ClearAssignment(db, "mr-5", 1700000000); err != nil {
		t.Fatalf("ClearAssignment(live) = %v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// --- ReleaseFromShift ---

// Empty shiftIDs is a no-op: returns (nil, nil) and issues no queries.
func TestReleaseFromShift_EmptyIsNoOp(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	got, err := ReleaseFromShift(db, nil, 1700000000)
	if err != nil {
		t.Fatalf("ReleaseFromShift(empty) err = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("ReleaseFromShift(empty) = %v, want nil", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("empty shift list should issue no queries: %v", err)
	}
}

// Non-empty: first SELECTs affected moves (with assignee name), then runs the
// UPDATE ... FROM shifts. Returns the captured rows.
func TestReleaseFromShift_SelectsThenUpdatesAndReturnsCaptured(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	assignmentType := "shift"
	userID := "driver-1"
	shiftID := "shift-1"
	userName := "Alice"

	rows := sqlmock.NewRows([]string{
		"id", "assignment_type", "assigned_user_id", "assigned_shift_id", "assigned_user_name",
	}).AddRow("mr-1", assignmentType, userID, shiftID, userName)

	mock.ExpectQuery(`(?s)SELECT mr.id.*FROM bin_move_requests mr.*LEFT JOIN users u.*WHERE mr.assigned_shift_id IN.*AND mr.status IN`).
		WithArgs("shift-1").
		WillReturnRows(rows)

	mock.ExpectExec(`(?s)UPDATE bin_move_requests AS mr.*FROM shifts s.*WHERE mr.assigned_shift_id = s.id.*AND mr.assigned_shift_id IN.*AND mr.status IN`).
		WithArgs(sqlmock.AnyArg(), "shift-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := ReleaseFromShift(db, []string{"shift-1"}, 1700000000)
	if err != nil {
		t.Fatalf("ReleaseFromShift = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("ReleaseFromShift returned %d rows, want 1", len(got))
	}
	r := got[0]
	if r.ID != "mr-1" {
		t.Fatalf("captured ID = %q, want mr-1", r.ID)
	}
	if r.AssignmentType == nil || *r.AssignmentType != "shift" {
		t.Fatalf("captured AssignmentType = %v, want 'shift'", r.AssignmentType)
	}
	if r.AssignedUserID == nil || *r.AssignedUserID != "driver-1" {
		t.Fatalf("captured AssignedUserID = %v, want 'driver-1'", r.AssignedUserID)
	}
	if r.AssignedShiftID == nil || *r.AssignedShiftID != "shift-1" {
		t.Fatalf("captured AssignedShiftID = %v, want 'shift-1'", r.AssignedShiftID)
	}
	if r.AssignedUserName == nil || *r.AssignedUserName != "Alice" {
		t.Fatalf("captured AssignedUserName = %v, want 'Alice'", r.AssignedUserName)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
