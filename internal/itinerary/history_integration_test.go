//go:build pgintegration

// Real-Postgres tests for the shift edit timeline.
//
// The sqlmock tests prove the Go ISSUES the right statements. These prove the
// statements actually WORK, which sqlmock cannot: that the data-modifying CTE
// logs exactly the rows it removed, that each add writer logs the row it
// inserted, that the read returns one shift's events in order, that row-level
// security keeps one tenant out of another's timeline, and that the composite
// foreign key refuses a cross-tenant row outright.
//
// Fixtures are seeded as a superuser (who bypasses RLS); everything under test
// runs as the app role with app.org_id set, exactly as in production.
//
// Not part of the normal suite; see internal/pgtest for how to run it.
package itinerary

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/pgtest"
)

type fixture struct {
	app, admin   *sqlx.DB
	orgA, orgB   string
	shiftA       string
	shiftA2      string // a second shift in the same org: timelines must not bleed
	mgrA, drvA   string
	drvA2        string
	collA, pickA string
	whA          string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	app, admin := pgtest.Connect(t)
	f := &fixture{
		app: app, admin: admin,
		// UUIDs, as in production: orgdb rejects anything else, and the login
		// path loops over every active org.
		orgA: uuid.NewString(), orgB: uuid.NewString(),
		shiftA: uuid.NewString(), shiftA2: uuid.NewString(),
		mgrA: uuid.NewString(), drvA: uuid.NewString(), drvA2: uuid.NewString(),
	}
	a := f.admin
	for _, org := range []string{f.orgA, f.orgB} {
		a.MustExec(`INSERT INTO organizations (id, name, slug) VALUES ($1, $1, $1)`, org)
	}
	for _, u := range []struct{ id, name, role string }{{f.mgrA, "Manager Ann", "admin"}, {f.drvA, "Driver Dee", "driver"}, {f.drvA2, "Driver Eve", "driver"}} {
		a.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $1 || '@x', 'x', $2, $3, $4)`, u.id, u.name, u.role, f.orgA)
	}
	for _, s := range []string{f.shiftA, f.shiftA2} {
		a.MustExec(`INSERT INTO shifts (id, driver_id, status, organization_id) VALUES ($1, $2, 'ready', $3)`, s, f.drvA, f.orgA)
	}
	ins := func(typ string, seq int) string {
		var id string
		a.Get(&id, `INSERT INTO route_tasks (shift_id, sequence_order, task_type, latitude, longitude, bin_number, organization_id)
			VALUES ($1, $2, $3, 37.3, -121.9, $4, $5) RETURNING id::text`, f.shiftA, seq, typ, 40+seq, f.orgA)
		return id
	}
	f.collA, f.pickA, f.whA = ins("collection", 1), ins("pickup", 2), ins("warehouse_stop", 3)
	return f
}

// as runs fn in a transaction bound to org, like orgdb does per request.
func (f *fixture) as(t *testing.T, org string, fn func(tx *sqlx.Tx) error) error {
	t.Helper()
	tx := f.app.MustBegin()
	tx.MustExec(`SELECT set_config('app.org_id', $1, true)`, org)
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// must is as for steps the test is not about: any error fails the test, so no
// assertion downstream can pass vacuously on a step that silently failed.
func (f *fixture) must(t *testing.T, org string, fn func(tx *sqlx.Tx) error) {
	t.Helper()
	if err := f.as(t, org, fn); err != nil {
		t.Fatalf("step failed: %v", err)
	}
}

func (f *fixture) edits(t *testing.T, org, shift string) []Edit {
	t.Helper()
	var out []Edit
	f.must(t, org, func(tx *sqlx.Tx) (err error) { out, err = ListEdits(tx, shift); return })
	return out
}

// raw is one history row as stored, read past RLS.
type raw struct {
	Event  string  `db:"event_type"`
	TaskID *string `db:"task_id"`
	Actor  *string `db:"actor_id"`
	Reason *string `db:"reason"`
}

func (f *fixture) rows(t *testing.T, shift string) []raw {
	t.Helper()
	var out []raw
	if err := f.admin.Select(&out, `SELECT event_type, task_id, actor_id, reason FROM shift_edit_history WHERE shift_id = $1 ORDER BY seq`, shift); err != nil {
		t.Fatal(err)
	}
	return out
}

// seedMove seeds a bin with one open relocation move. route_tasks has real
// composite FKs to bins and bin_move_requests, and a bin may hold only one
// open move, so every move gets its own bin.
func (f *fixture) seedMove(t *testing.T, binNumber int) (binID, moveID string) {
	t.Helper()
	binID, moveID = uuid.NewString(), uuid.NewString()
	f.admin.MustExec(`INSERT INTO bins (id, bin_number, current_street, city, zip, status, organization_id)
		VALUES ($1, $2, '1 Main', 'San Jose', '95112', 'active', $3)`, binID, binNumber, f.orgA)
	f.admin.MustExec(`INSERT INTO bin_move_requests (id, bin_id, scheduled_date, urgency, requested_by,
		original_latitude, original_longitude, original_address, move_type, created_at, updated_at, organization_id)
		VALUES ($1, $2, 1, 'scheduled', $3, 37.31, -121.91, '1 Main', 'relocation', 1, 1, $4)`, moveID, binID, f.mgrA, f.orgA)
	return binID, moveID
}

func str(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestIntegration_RemovalLogsExactlyWhatItRemoved(t *testing.T) {
	f := setup(t)

	// Remove a collection, a pickup and a warehouse stop together.
	f.must(t, f.orgA, func(tx *sqlx.Tx) error {
		return RemoveByIDs(tx, []string{f.collA, f.pickA, f.whA}, f.mgrA, "move_request_cancelled", 100)
	})

	got := map[string]Edit{}
	for _, e := range f.edits(t, f.orgA, f.shiftA) {
		if e.EventType != "task_removed" {
			t.Errorf("unexpected event %q", e.EventType)
		}
		got[str(e.TaskID)] = e
		if e.ActorName == nil || *e.ActorName != "Manager Ann" {
			t.Errorf("actor not resolved to a name: %v", e.ActorName)
		}
		if e.CreatedAt != 100 {
			t.Errorf("created_at = %d, want the edit's own time 100", e.CreatedAt)
		}
	}
	if len(got) != 2 {
		t.Fatalf("logged %d removals, want 2 (collection + pickup)", len(got))
	}
	if e := got[f.collA]; e.BinNumber == nil || *e.BinNumber != 41 || str(e.TaskType) != "collection" {
		t.Errorf("collection removal lost its task details: %+v", e)
	}
	// The warehouse stop WAS removed — the CTE runs regardless — but not logged.
	if _, ok := got[f.whA]; ok {
		t.Error("warehouse stop logged; it is an optimizer artifact, not an edit")
	}
	var whDeleted bool
	f.admin.Get(&whDeleted, `SELECT is_deleted FROM route_tasks WHERE id::text = $1`, f.whA)
	if !whDeleted {
		t.Error("warehouse stop was not removed — the data-modifying CTE must run even when nothing is logged")
	}

	// Re-removing is a no-op that must NOT re-log.
	f.must(t, f.orgA, func(tx *sqlx.Tx) error {
		return RemoveByIDs(tx, []string{f.collA, f.pickA}, f.mgrA, "again", 200)
	})
	if n := len(f.rows(t, f.shiftA)); n != 2 {
		t.Errorf("history rows = %d after a repeat removal, want still 2", n)
	}
}

// A system removal has no actor and, here, no reason. Both are stored as NULL,
// and the event is still LISTED — the read joins users without dropping rows
// that have no user.
func TestIntegration_SystemRemovalIsListedWithNoActorOrReason(t *testing.T) {
	f := setup(t)
	f.must(t, f.orgA, func(tx *sqlx.Tx) error {
		return RemoveByIDs(tx, []string{f.collA}, SystemActor, "", 7)
	})

	r := f.rows(t, f.shiftA)
	if len(r) != 1 || r[0].Actor != nil || r[0].Reason != nil {
		t.Fatalf("stored %+v, want one row with NULL actor and NULL reason", r)
	}
	e := f.edits(t, f.orgA, f.shiftA)
	if len(e) != 1 || e[0].ActorName != nil || e[0].ActorID != nil {
		t.Errorf("listed %+v, want the system event with no actor", e)
	}
}

// A removal keeps the move and bin it concerned, so the timeline can say which
// move request a cancelled leg belonged to.
func TestIntegration_RemovalCarriesTheMoveAndTheBin(t *testing.T) {
	f := setup(t)
	binID, moveID := f.seedMove(t, 77)
	f.must(t, f.orgA, func(tx *sqlx.Tx) error {
		_, err := AddMove(tx, f.shiftA, MovePlacement{ // born with the shift: not logged
			InsertSeq: 10, MoveRequestID: moveID, BinID: binID, BinNumber: 77, MoveType: "redeployment",
			DropoffLat: 37.4, DropoffLng: -121.8, DropoffAddress: "there", Now: 300,
		})
		return err
	})
	var placement string
	if err := f.admin.Get(&placement, `SELECT id::text FROM route_tasks WHERE move_request_id = $1`, moveID); err != nil {
		t.Fatal(err)
	}
	f.must(t, f.orgA, func(tx *sqlx.Tx) error {
		return RemoveByIDs(tx, []string{placement}, f.mgrA, "move_request_cancelled", 400)
	})

	e := f.edits(t, f.orgA, f.shiftA)
	if len(e) != 1 {
		t.Fatalf("got %d events, want the one removal", len(e))
	}
	if str(e[0].MoveRequestID) != moveID || e[0].BinNumber == nil || *e[0].BinNumber != 77 || str(e[0].TaskType) != "placement" {
		t.Errorf("removal lost the move/bin: %+v", e[0])
	}
}

// Every mid-shift add writer logs exactly the row it inserted; a system actor
// and an empty reason are stored as NULL, like on removals.
func TestIntegration_EveryAddWriterLogsTheRowItInserted(t *testing.T) {
	f := setup(t)
	binID, moveID := f.seedMove(t, 55)
	plID := uuid.NewString()
	f.admin.MustExec(`INSERT INTO potential_locations (id, address, street, city, zip, requested_by_user_id, requested_by_name, created_at, updated_at, organization_id)
		VALUES ($1, '9 Place Blvd', '9 Place Blvd', 'San Jose', '95112', $2, 'Manager Ann', 1, 1, $3)`, plID, f.mgrA, f.orgA)

	var coll, place, leg string
	f.must(t, f.orgA, func(tx *sqlx.Tx) (err error) {
		if coll, err = AddCollection(tx, f.shiftA, NewCollection{Seq: 20, BinID: binID, BinNumber: 55, Lat: 37.3, Lng: -121.9,
			AddedBy: f.mgrA, AdditionReason: "customer called", Now: 500}); err != nil {
			return err
		}
		if place, err = AddPlacement(tx, f.shiftA, NewPlacement{Seq: 21, PotentialLocationID: plID, Lat: 37.2, Lng: -121.8,
			Address: "9 Place Blvd", AddedBy: SystemActor, Now: 500}); err != nil {
			return err
		}
		n := 55
		leg, err = AddMoveLeg(tx, f.shiftA, NewMoveLeg{Seq: 22, Type: Pickup, MoveRequestID: moveID, BinID: binID, BinNumber: &n,
			MoveType: "relocation", Lat: 37.31, Lng: -121.91, AddedBy: f.mgrA, AdditionReason: "", Now: 500})
		return err
	})

	byTask := map[string]raw{}
	for _, r := range f.rows(t, f.shiftA) {
		if r.Event != "task_added" {
			t.Errorf("unexpected %q", r.Event)
		}
		byTask[str(r.TaskID)] = r
	}
	if len(byTask) != 3 {
		t.Fatalf("logged %d adds, want 3: %+v", len(byTask), byTask)
	}
	if r, ok := byTask[coll]; !ok || str(r.Actor) != f.mgrA || str(r.Reason) != "customer called" {
		t.Errorf("AddCollection logged %+v (found=%v)", r, ok)
	}
	if r, ok := byTask[place]; !ok || r.Actor != nil {
		t.Errorf("AddPlacement by the system must log a NULL actor: %+v (found=%v)", r, ok)
	}
	if r, ok := byTask[leg]; !ok || r.Reason != nil {
		t.Errorf("AddMoveLeg with no reason must log a NULL reason: %+v (found=%v)", r, ok)
	}
}

func TestIntegration_WarehouseStopsAreNeverLoggedAsAdds(t *testing.T) {
	f := setup(t)
	f.must(t, f.orgA, func(tx *sqlx.Tx) error { return logTasksAdded(tx, []string{f.whA}, f.mgrA, nil, 1) })
	if r := f.rows(t, f.shiftA); len(r) != 0 {
		t.Errorf("a warehouse stop was logged: %+v", r)
	}
}

// The read returns ONE shift's events, ordered by when they happened and,
// within a second, by the order they were written.
func TestIntegration_TimelineIsOneShiftInOrder(t *testing.T) {
	f := setup(t)
	if e := f.edits(t, f.orgA, f.shiftA2); e == nil || len(e) != 0 {
		t.Fatalf("an empty timeline must read as [], got %#v", e)
	}

	// Same second: reassigned, then the finished work cleared.
	f.must(t, f.orgA, func(tx *sqlx.Tx) error {
		if err := LogDriverReassigned(tx, f.shiftA, f.mgrA, f.drvA, f.drvA2, 500); err != nil {
			return err
		}
		return RemoveByIDs(tx, []string{f.pickA}, f.mgrA, "completed_before_reassign", 500)
	})
	// Written LAST but happened FIRST: the order must follow created_at.
	f.must(t, f.orgA, func(tx *sqlx.Tx) error { return LogShiftCreated(tx, f.shiftA, f.mgrA, 100) })
	// Another shift in the same org: must not appear on shift A's timeline.
	f.must(t, f.orgA, func(tx *sqlx.Tx) error { return LogShiftCreated(tx, f.shiftA2, f.mgrA, 300) })

	var got []string
	for _, e := range f.edits(t, f.orgA, f.shiftA) {
		got = append(got, e.EventType)
	}
	if want := "created driver_reassigned task_removed"; strings.Join(got, " ") != want {
		t.Errorf("shift A timeline = %v, want [%s]", got, want)
	}
}

func TestIntegration_AddMoveLogsOnlyEdits(t *testing.T) {
	f := setup(t)
	lat, lng := 37.31, -121.91 // the bin's current location (route_tasks.latitude is NOT NULL)
	place := func(by *string) {
		binID, moveID := f.seedMove(t, 77)
		f.must(t, f.orgA, func(tx *sqlx.Tx) error {
			_, err := AddMove(tx, f.shiftA, MovePlacement{
				InsertSeq: 10, MoveRequestID: moveID, BinID: binID, BinNumber: 77,
				MoveType: "relocation", PickupLat: &lat, PickupLng: &lng, PickupAddress: "here", DropoffLat: 37.4, DropoffLng: -121.8,
				DropoffAddress: "there", AddedBy: by, Now: 300,
			})
			return err
		})
	}
	place(nil) // shift birth
	if n := len(f.rows(t, f.shiftA)); n != 0 {
		t.Fatalf("shift birth logged %d rows, want 0 — birth is one 'created' event", n)
	}
	place(&f.mgrA) // a manager assigning a move
	if r := f.rows(t, f.shiftA); len(r) != 2 || r[0].Event != "task_added" || r[1].Event != "task_added" || str(r[0].TaskID) == str(r[1].TaskID) {
		t.Errorf("manager-assigned move logged %+v, want 2 distinct task_added rows (pickup + dropoff)", r)
	}
}

func TestIntegration_ReassignAndCreatedResolveNames(t *testing.T) {
	f := setup(t)
	// The created event counts bins from the shift row, which the real writer
	// has just recomputed in the same transaction (the database package's
	// CreateShiftWithTasks test covers that path end to end).
	f.admin.MustExec(`UPDATE shifts SET total_bins = 3 WHERE id = $1`, f.shiftA)
	f.must(t, f.orgA, func(tx *sqlx.Tx) error { return LogShiftCreated(tx, f.shiftA, f.mgrA, 1) })
	f.must(t, f.orgA, func(tx *sqlx.Tx) error { return LogDriverReassigned(tx, f.shiftA, f.mgrA, f.drvA, f.drvA2, 2) })

	edits := f.edits(t, f.orgA, f.shiftA)
	if len(edits) != 2 {
		t.Fatalf("got %d edits, want 2", len(edits))
	}
	if edits[0].EventType != "created" || edits[0].BinCount == nil || *edits[0].BinCount != 3 {
		t.Errorf("created event wrong: %+v", edits[0])
	}
	r := edits[1]
	if r.FromDriverName == nil || *r.FromDriverName != "Driver Dee" || r.ToDriverName == nil || *r.ToDriverName != "Driver Eve" {
		t.Errorf("reassign names not resolved: from=%v to=%v", r.FromDriverName, r.ToDriverName)
	}
}

// The tenancy guarantees, proven against real RLS rather than asserted.
func TestIntegration_AnotherTenantCannotSeeOrForgeTheTimeline(t *testing.T) {
	f := setup(t)
	f.must(t, f.orgA, func(tx *sqlx.Tx) error { return LogShiftCreated(tx, f.shiftA, f.mgrA, 1) })

	// READ: org B sees nothing of org A's timeline — and the read itself
	// succeeds, so "nothing" is RLS filtering, not an error.
	if e := f.edits(t, f.orgB, f.shiftA); len(e) != 0 {
		t.Errorf("org B read %d of org A's events", len(e))
	}

	// WRITE via the real writer: org B cannot see A's shift, so it logs nothing,
	// without erroring — the INSERT ... SELECT simply finds no shift.
	f.must(t, f.orgB, func(tx *sqlx.Tx) error { return LogDriverReassigned(tx, f.shiftA, "x", "y", "z", 2) })
	for _, r := range f.rows(t, f.shiftA) {
		if r.Event == "driver_reassigned" {
			t.Errorf("org B wrote an event onto org A's shift: %+v", r)
		}
	}

	// FORGE, bypassing the writers: a raw insert naming org B's tenancy against
	// org A's shift. RLS WITH CHECK passes (it IS org B's row) — it is the
	// composite foreign key that must refuse it, because (org B, shift A) does
	// not exist in shifts.
	err := f.as(t, f.orgB, func(tx *sqlx.Tx) error {
		_, err := tx.Exec(`INSERT INTO shift_edit_history (organization_id, shift_id, event_type, created_at)
			VALUES ($1, $2, 'created', 1)`, f.orgB, f.shiftA)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "shift_edit_history_shift_fkey") {
		t.Errorf("cross-tenant forge was not refused by the composite FK: %v", err)
	}
}
