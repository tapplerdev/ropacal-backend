package moverequest

import (
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
)

func mockExtEdit(t *testing.T) (*sqlx.DB, sqlmock.Sqlmock) {
	t.Helper()
	raw, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return sqlx.NewDb(raw, "postgres"), mock
}

func TestFieldEdits_HasChanges(t *testing.T) {
	if (FieldEdits{}).HasChanges() {
		t.Error("empty FieldEdits.HasChanges() = true, want false")
	}
	r := "x"
	if !(FieldEdits{Reason: &r}).HasChanges() {
		t.Error("FieldEdits{Reason}.HasChanges() = false, want true")
	}
}

// A reason-only edit issues one guarded UPDATE (carrying the terminal guard) and
// does NOT touch scheduled_date/urgency.
func TestEditFields_ReasonOnly(t *testing.T) {
	db, mock := mockExtEdit(t)
	defer db.Close()

	reason := "because"
	mock.ExpectExec("(?s)UPDATE bin_move_requests SET .*reason = .*WHERE id = .*AND status NOT IN").
		WithArgs(int64(1700000000), "because", "m1", "completed", "cancelled").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := EditFields(db, "m1", FieldEdits{Reason: &reason}, 1700000000); err != nil {
		t.Fatalf("EditFields: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A scheduled_date edit also writes a recomputed urgency.
func TestEditFields_RescheduleRecomputesUrgency(t *testing.T) {
	db, mock := mockExtEdit(t)
	defer db.Close()

	sched := int64(1700000000 + 5*86400)
	mock.ExpectExec("(?s)UPDATE bin_move_requests SET .*scheduled_date = .*urgency = .*WHERE id = .*AND status NOT IN").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := EditFields(db, "m1", FieldEdits{ScheduledDate: &sched}, 1700000000); err != nil {
		t.Fatalf("EditFields: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// No fields set → no query at all.
func TestEditFields_NoChangesIsNoOp(t *testing.T) {
	db, mock := mockExtEdit(t)
	defer db.Close()

	if err := EditFields(db, "m1", FieldEdits{}, 1700000000); err != nil {
		t.Fatalf("EditFields(empty) = %v, want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no fields should issue no query: %v", err)
	}
}

// Editing a terminal move (0 rows affected by the guard) → ErrInvalidTransition.
func TestEditFields_TerminalMoveRefused(t *testing.T) {
	db, mock := mockExtEdit(t)
	defer db.Close()

	reason := "nope"
	mock.ExpectExec("(?s)UPDATE bin_move_requests SET .*WHERE id = .*AND status NOT IN").
		WillReturnResult(sqlmock.NewResult(0, 0)) // guard matched nothing → terminal/absent

	err := EditFields(db, "m1", FieldEdits{Reason: &reason}, 1700000000)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("EditFields(terminal) = %v, want ErrInvalidTransition", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
