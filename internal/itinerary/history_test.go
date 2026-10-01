package itinerary

import (
	"database/sql/driver"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// expectAddedLogged expects the task_added timeline write for nIDs task ids,
// as issued by logTasksAdded.
// expectAddedLogged expects the task_added timeline write for exactly the given
// task ids. Pass sameAs matchers so the test proves the ids logged are the ids
// the writer inserted.
func expectAddedLogged(mock sqlmock.Sqlmock, actor, reason any, now int64, ids ...driver.Value) {
	args := append([]driver.Value{string(EditTaskAdded), actor, reason, now}, ids...)
	mock.ExpectExec(`(?s)INSERT INTO shift_edit_history.*FROM route_tasks\s+WHERE id IN \(.*\) AND task_type <> 'warehouse_stop'`).
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, int64(len(ids))))
}

// insertArgs matches an INSERT's n arguments, capturing the first: the new id.
func insertArgs(id *capture, n int) []driver.Value {
	args := []driver.Value{id}
	for i := 1; i < n; i++ {
		args = append(args, sqlmock.AnyArg())
	}
	return args
}

// capture matches anything and remembers it; sameAs then matches only that
// value. Together they prove the id a writer LOGS is the id it INSERTED, which
// sqlmock.AnyArg on both sides cannot.
type capture struct{ v driver.Value }

func (c *capture) Match(v driver.Value) bool { c.v = v; return true }

type sameAs struct{ c *capture }

func (s sameAs) Match(v driver.Value) bool { return s.c.v != nil && v == s.c.v }

// A CHECK violation inside a transaction aborts the whole EDIT, not just its
// log line. So the Go constants and the migration's CHECK list must agree in
// BOTH directions: a Go value the database rejects breaks real edits, and a
// database value Go cannot produce is dead weight that hides drift.
func TestEditEvents_MatchTheMigrationCheckConstraint(t *testing.T) {
	sqlText, err := os.ReadFile("../database/migrations/00009_shift_edit_history.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	m := regexp.MustCompile(`CHECK \(event_type IN \(([^)]*)\)\)`).FindSubmatch(sqlText)
	if m == nil {
		t.Fatal("event_type CHECK not found in the migration — did its shape change?")
	}
	var inSQL []string
	for _, v := range strings.Split(string(m[1]), ",") {
		inSQL = append(inSQL, strings.Trim(strings.TrimSpace(v), "'"))
	}
	var inGo []string
	for _, e := range editEvents {
		inGo = append(inGo, string(e))
	}
	sort.Strings(inSQL)
	sort.Strings(inGo)
	if strings.Join(inSQL, ",") != strings.Join(inGo, ",") {
		t.Errorf("CHECK allows %v but Go defines %v — they must match exactly", inSQL, inGo)
	}
}

// Removal and its timeline row are ONE statement, so a removal can never land
// without its log — even through SyncPlacementRemoval, which passes the bare
// pool and so has no transaction to lean on.
func TestRemoveByIDs_RemovalAndLogAreOneStatement(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)^\s*WITH removed AS \(\s*UPDATE route_tasks.*RETURNING.*\)\s*INSERT INTO shift_edit_history.*FROM removed`).
		WithArgs(int64(5), "mgr", "move_request_cancelled", int64(5), "t1",
			"task_removed", "mgr", "move_request_cancelled", int64(5)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := RemoveByIDs(db, []string{"t1"}, "mgr", "move_request_cancelled", 5); err != nil {
		t.Fatalf("RemoveByIDs: %v", err)
	}
	// Strict: a second, separate history statement would fail here.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// actor_id only ever holds a real users.id; system writes and empty reasons
// are NULL so the read side never renders a blank actor or a blank "because".
func TestRemoveByIDs_SystemActorAndEmptyReasonAreNull(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)WITH removed AS`).
		WithArgs(int64(5), SystemActor, "", int64(5), "t1",
			"task_removed", nil, nil, int64(5)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := RemoveByIDs(db, []string{"t1"}, SystemActor, "", 5); err != nil {
		t.Fatalf("RemoveByIDs: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// expectRelocationPair expects AddMove's pair-branch statements. The exact
// column contract is pinned by TestAddMove_RelocationInsertsPickupAndDropoff;
// this only needs the sequence, to reach the timeline write that follows it.
func expectRelocationPair(mock sqlmock.Sqlmock) (pickup, dropoff *capture) {
	pickup, dropoff = &capture{}, &capture{}
	mock.ExpectExec(`(?s)UPDATE route_tasks SET sequence_order = sequence_order \+`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`(?s)INSERT INTO route_tasks`).WithArgs(insertArgs(pickup, 18)...).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)INSERT INTO route_tasks`).WithArgs(insertArgs(dropoff, 17)...).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`task_type = 'pickup'`).WillReturnRows(sqlmock.NewRows([]string{"sequence_order"}).AddRow(3))
	mock.ExpectQuery(`task_type = 'dropoff'`).WillReturnRows(sqlmock.NewRows([]string{"sequence_order"}).AddRow(4))
	return pickup, dropoff
}

// A move assigned to a shift by a manager is an edit: BOTH legs go on the
// timeline. (Shift birth — AddedBy nil — writing nothing is proven by the
// existing TestAddMove_* tests, which pass nil and assert strictly.)
func TestAddMove_AssignedByAManagerLogsBothLegs(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	pickup, dropoff := expectRelocationPair(mock)
	expectAddedLogged(mock, "mgr-7", "assigned from backlog", 1700000000, sameAs{pickup}, sameAs{dropoff})

	mgr, why := "mgr-7", "assigned from backlog"
	if _, err := AddMove(db, "shift-1", MovePlacement{
		InsertSeq: 3, MoveRequestID: "move-1", BinID: "bin-1", BinNumber: 42,
		MoveType: "relocation", DropoffLat: 37.3, DropoffLng: -121.9,
		AddedBy: &mgr, AdditionReason: &why, Now: 1700000000,
	}); err != nil {
		t.Fatalf("AddMove: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestAddMove_RedeploymentAssignedByAManagerLogsThePlacement(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	placement := &capture{}
	mock.ExpectExec(`(?s)UPDATE route_tasks SET sequence_order = sequence_order \+ 1`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`(?s)INSERT INTO route_tasks.*'redeployment'`).WithArgs(insertArgs(placement, 17)...).WillReturnResult(sqlmock.NewResult(0, 1))
	expectAddedLogged(mock, "mgr-7", nil, 1700000000, sameAs{placement})

	mgr := "mgr-7"
	if _, err := AddMove(db, "shift-1", MovePlacement{
		InsertSeq: 3, MoveRequestID: "move-1", BinID: "bin-1", BinNumber: 42,
		MoveType: "redeployment", DropoffLat: 37.3, DropoffLng: -121.9,
		AddedBy: &mgr, Now: 1700000000,
	}); err != nil {
		t.Fatalf("AddMove: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// The count is read from the shift row, not passed in, so it is the same
// logical-bin count the dashboard shows for the shift. (The integration test
// proves the value against a real row.)
// Adds normalise exactly like removals: a "system" or empty actor and an
// empty reason are NULL, so the timeline never shows a blank "by" or "because".
func TestAdds_SystemActorAndEmptyReasonAreNull(t *testing.T) {
	t.Run("AddCollection", func(t *testing.T) {
		db, mock := mockExtCreate(t)
		defer db.Close()
		id := &capture{}
		mock.ExpectExec(addTasksCols).WithArgs(insertArgs(id, 18)...).WillReturnResult(sqlmock.NewResult(0, 1))
		expectAddedLogged(mock, nil, nil, 5, sameAs{id})
		if _, err := AddCollection(db, "s1", NewCollection{Seq: 1, BinID: "b1", BinNumber: 1, AddedBy: SystemActor, Now: 5}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("AddMove", func(t *testing.T) {
		db, mock := mockExt(t)
		defer db.Close()
		pickup, dropoff := expectRelocationPair(mock)
		expectAddedLogged(mock, "mgr-7", nil, 5, sameAs{pickup}, sameAs{dropoff})
		mgr, empty := "mgr-7", ""
		if _, err := AddMove(db, "shift-1", MovePlacement{
			InsertSeq: 3, MoveRequestID: "move-1", BinID: "bin-1", BinNumber: 42,
			MoveType: "relocation", DropoffLat: 37.3, DropoffLng: -121.9,
			AddedBy: &mgr, AdditionReason: &empty, Now: 5,
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLogShiftCreated_OneEventCountingFromTheShiftRow(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)INSERT INTO shift_edit_history.*jsonb_build_object\('bin_count', total_bins\).*FROM shifts WHERE id = `).
		WithArgs("created", "mgr", int64(9), "shift-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := LogShiftCreated(db, "shift-1", "mgr", 9); err != nil {
		t.Fatalf("LogShiftCreated: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestLogDriverReassigned_RecordsBothDrivers(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectExec(`(?s)INSERT INTO shift_edit_history.*FROM shifts WHERE id = `).
		WithArgs("driver_reassigned", "mgr", `{"from_driver_id":"d-old","to_driver_id":"d-new"}`, int64(9), "shift-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := LogDriverReassigned(db, "shift-1", "mgr", "d-old", "d-new", 9); err != nil {
		t.Fatalf("LogDriverReassigned: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Within one second the timeline must keep the order events were written (a
// reassignment, then the finished work it clears). No black-box test can force
// Postgres to reorder same-second rows, so the ORDER BY itself is the contract,
// pinned here: oldest first, ties broken by seq, nothing after it.
func TestListEdits_OrdersByTimeThenWriteOrder(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()
	mock.ExpectQuery(`(?s)FROM shift_edit_history h.*WHERE h\.shift_id = \$1\s+ORDER BY h\.created_at, h\.seq\s*$`).
		WithArgs("shift-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if _, err := ListEdits(db, "shift-1"); err != nil {
		t.Fatalf("ListEdits: %v", err)
	}
}

func TestListEdits_EmptyTimelineIsAnEmptyListNotNull(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()
	mock.ExpectQuery(`(?s)FROM shift_edit_history h`).WithArgs("shift-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	got, err := ListEdits(db, "shift-1")
	if err != nil {
		t.Fatalf("ListEdits: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("got %#v, want a non-nil empty slice (serialises as [], not null)", got)
	}
}
