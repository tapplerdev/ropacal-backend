package moverequest

import (
	"errors"
	"testing"

	"ropacal-backend/internal/models"
)

// fakeStore is a DB-free Store double for unit tests.
type fakeStore struct {
	active    []ActionableMove
	activeErr error
}

func (f *fakeStore) ByID(string) (*models.BinMoveRequest, error) { return nil, ErrNotFound }
func (f *fakeStore) ActiveWithBin() ([]ActionableMove, error)    { return f.active, f.activeErr }
func (f *fakeStore) ResponsibleDriver(*models.BinMoveRequest) (string, string, error) {
	return "", "", nil
}
func (f *fakeStore) Create(*models.BinMoveRequest, *string) error { return nil }

func ids(ms []ActionableMove) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func TestClassifyActionable(t *testing.T) {
	now := int64(1_000_000)
	moves := []ActionableMove{
		{ID: "overdue", ScheduledDate: now - 1*hour},
		{ID: "due-now", ScheduledDate: now},           // 0h → due soon (not overdue)
		{ID: "due-soon", ScheduledDate: now + 6*hour}, // within 12h window
		{ID: "later", ScheduledDate: now + 100*hour},  // beyond window → neither
	}
	overdue, dueSoon := ClassifyActionable(moves, now, 12)

	if got := ids(overdue); len(got) != 1 || got[0] != "overdue" {
		t.Errorf("overdue = %v, want [overdue]", got)
	}
	if got := ids(dueSoon); len(got) != 2 || got[0] != "due-now" || got[1] != "due-soon" {
		t.Errorf("dueSoon = %v, want [due-now due-soon]", got)
	}
}

func TestServiceFindActionable(t *testing.T) {
	now := int64(1_000_000)
	svc := &Service{
		store: &fakeStore{active: []ActionableMove{
			{ID: "a", ScheduledDate: now - 5*hour},
			{ID: "b", ScheduledDate: now + 2*hour},
		}},
		now: func() int64 { return now },
	}
	overdue, dueSoon, err := svc.FindActionable(12)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(overdue) != 1 || overdue[0].ID != "a" {
		t.Errorf("overdue = %v, want [a]", ids(overdue))
	}
	if len(dueSoon) != 1 || dueSoon[0].ID != "b" {
		t.Errorf("dueSoon = %v, want [b]", ids(dueSoon))
	}
}

func TestServiceFindActionablePropagatesError(t *testing.T) {
	wantErr := errors.New("db down")
	svc := &Service{store: &fakeStore{activeErr: wantErr}, now: func() int64 { return 0 }}
	if _, _, err := svc.FindActionable(12); !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}
