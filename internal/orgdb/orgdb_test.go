package orgdb

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
)

const (
	orgA = "00000000-0000-0000-0000-000000000001"
	orgB = "00000000-0000-0000-0000-000000000002"
	orgC = "00000000-0000-0000-0000-000000000003"
)

func newMock(t *testing.T) (*sqlx.DB, sqlmock.Sqlmock) {
	t.Helper()
	raw, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	return sqlx.NewDb(raw, "sqlmock"), mock
}

func expectOrgTx(mock sqlmock.Sqlmock, orgID string) {
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT set_config\('app\.org_id', \$1, true\)`).
		WithArgs(orgID).
		WillReturnResult(sqlmock.NewResult(0, 0))
}

func TestNewValidatesOrgID(t *testing.T) {
	db, _ := newMock(t)
	if _, err := New(db, "not-a-uuid"); err == nil {
		t.Fatal("want error for invalid org id")
	}
	if _, err := New(db, orgA); err != nil {
		t.Fatalf("valid uuid rejected: %v", err)
	}
	if _, err := New(nil, orgA); err == nil {
		t.Fatal("want error for nil root")
	}
}

func TestBoundGetSetsGUCAndCommits(t *testing.T) {
	db, mock := newMock(t)
	d, _ := New(db, orgA)

	expectOrgTx(mock, orgA)
	mock.ExpectQuery(`SELECT name FROM bins`).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("bin-7"))
	mock.ExpectCommit()

	var name string
	if err := d.Get(&name, `SELECT name FROM bins WHERE id = $1`, "b1"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if name != "bin-7" {
		t.Fatalf("got %q", name)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBoundGetPropagatesErrNoRows(t *testing.T) {
	db, mock := newMock(t)
	d, _ := New(db, orgA)

	expectOrgTx(mock, orgA)
	mock.ExpectQuery(`SELECT name FROM bins`).WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	var name string
	err := d.Get(&name, `SELECT name FROM bins WHERE id = $1`, "b1")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("want sql.ErrNoRows, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPassthroughGetSkipsTransaction(t *testing.T) {
	db, mock := newMock(t)
	d := Passthrough(db)

	// No ExpectBegin — a BEGIN here would fail the test.
	mock.ExpectQuery(`SELECT name FROM bins`).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("bin-7"))

	var name string
	if err := d.Get(&name, `SELECT name FROM bins WHERE id = $1`, "b1"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBoundExecMaterialisesRowsAffected(t *testing.T) {
	db, mock := newMock(t)
	d, _ := New(db, orgA)

	expectOrgTx(mock, orgA)
	mock.ExpectExec(`UPDATE bins SET`).WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()

	res, err := d.Exec(`UPDATE bins SET x = 1`)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 3 {
		t.Fatalf("RowsAffected = %d, %v", n, err)
	}
	if _, err := res.LastInsertId(); err == nil {
		t.Fatal("LastInsertId should error, as with lib/pq")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestQueryRowDefersUntilScan(t *testing.T) {
	db, mock := newMock(t)
	d, _ := New(db, orgA)

	row := d.QueryRow(`SELECT count(*) FROM bins WHERE city = $1`, "San Jose")
	// Nothing must have executed yet.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("query ran before Scan: %v", err)
	}

	expectOrgTx(mock, orgA)
	mock.ExpectQuery(`SELECT count\(\*\) FROM bins`).
		WithArgs("San Jose").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(42))
	mock.ExpectCommit()

	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if n != 42 {
		t.Fatalf("got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestQueryxParksAndReleaseReaps(t *testing.T) {
	db, mock := newMock(t)
	d, _ := New(db, orgA)

	expectOrgTx(mock, orgA)
	mock.ExpectQuery(`SELECT id FROM bins`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("b1").AddRow("b2"))

	rows, err := d.Queryx(`SELECT id FROM bins`)
	if err != nil {
		t.Fatalf("Queryx: %v", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 2 {
		t.Fatalf("got %v", ids)
	}

	// The tx is parked, not committed — Release must roll it back.
	mock.ExpectRollback()
	d.Release()
	d.Release() // idempotent
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBeginxReturnsGUCedTx(t *testing.T) {
	db, mock := newMock(t)
	d, _ := New(db, orgA)

	expectOrgTx(mock, orgA)
	mock.ExpectExec(`INSERT INTO moves`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := d.Beginx()
	if err != nil {
		t.Fatalf("Beginx: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO moves (id) VALUES ($1)`, "m1"); err != nil {
		t.Fatalf("tx.Exec: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSystemModes(t *testing.T) {
	db, _ := newMock(t)

	restore := setStateForTest(&tenancyState{migrated: false, root: db, neutral: Passthrough(db)})
	defer restore()
	d, err := System(db, "ignored-in-dark-mode")
	if err != nil {
		t.Fatalf("System dark: %v", err)
	}
	if !d.passthrough {
		t.Fatal("dark-mode System must return a passthrough handle")
	}

	setStateForTest(&tenancyState{migrated: true, root: db})
	d, err = System(db, orgA)
	if err != nil {
		t.Fatalf("System migrated: %v", err)
	}
	if d.passthrough || d.OrgID() != orgA {
		t.Fatalf("migrated System must bind the org, got passthrough=%v org=%q", d.passthrough, d.OrgID())
	}
	if _, err := System(db, "junk"); err == nil {
		t.Fatal("migrated System must validate the org id")
	}
}

func TestFromPrecedence(t *testing.T) {
	db, _ := newMock(t)

	// 1. A request-bound handle always wins.
	bound, _ := New(db, orgA)
	req := httptest.NewRequest("GET", "/x", nil)
	req = req.WithContext(NewContext(req.Context(), bound))
	if got := From(req); got != bound {
		t.Fatal("ctx handle must win")
	}

	// 2. Dark mode falls back to the neutral passthrough.
	neutral := Passthrough(db)
	restore := setStateForTest(&tenancyState{migrated: false, root: db, neutral: neutral})
	defer restore()
	if got := From(httptest.NewRequest("GET", "/x", nil)); got != neutral {
		t.Fatal("dark mode must fall back to the neutral handle")
	}

	// 3. Migrated mode with no handle is a wiring bug → panic.
	setStateForTest(&tenancyState{migrated: true, root: db})
	assertPanics(t, func() { From(httptest.NewRequest("GET", "/x", nil)) })

	// 4. Uninitialised state (unit tests without setup) → panic with guidance.
	var nilState *tenancyState
	state.Store(nilState)
	assertPanics(t, func() { From(httptest.NewRequest("GET", "/x", nil)) })
}

func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}

func TestForEachActiveOrgDarkRunsOnce(t *testing.T) {
	db, _ := newMock(t)
	restore := setStateForTest(&tenancyState{migrated: false, root: db, neutral: Passthrough(db)})
	defer restore()

	var calls int
	ForEachActiveOrg(db, "test", func(d *DB) error {
		calls++
		if !d.passthrough {
			t.Fatal("dark mode must hand out a passthrough")
		}
		return nil
	})
	if calls != 1 {
		t.Fatalf("want exactly 1 call, got %d", calls)
	}
}

func TestForEachActiveOrgLoopsAndSurvivesFailures(t *testing.T) {
	db, mock := newMock(t)
	restore := setStateForTest(&tenancyState{migrated: true, root: db})
	defer restore()

	mock.ExpectQuery(`SELECT id FROM organizations WHERE status = 'active'`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(orgA).AddRow(orgB).AddRow(orgC))

	var seen []string
	ForEachActiveOrg(db, "test", func(d *DB) error {
		seen = append(seen, d.OrgID())
		switch d.OrgID() {
		case orgA:
			return errors.New("org A failed")
		case orgB:
			panic("org B panicked")
		}
		return nil
	})
	if len(seen) != 3 || seen[2] != orgC {
		t.Fatalf("failures must not stop the loop; saw %v", seen)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigConflictTarget(t *testing.T) {
	db, _ := newMock(t)
	restore := setStateForTest(&tenancyState{migrated: false, root: db, neutral: Passthrough(db)})
	defer restore()
	if got := ConfigConflictTarget(); got != "(key)" {
		t.Fatalf("dark mode: got %q", got)
	}
	setStateForTest(&tenancyState{migrated: true, root: db})
	if got := ConfigConflictTarget(); got != "(organization_id, key)" {
		t.Fatalf("migrated: got %q", got)
	}
}

func TestForContextResolvesFromPlainContext(t *testing.T) {
	db, _ := newMock(t)
	bound, _ := New(db, orgA)
	ctx := NewContext(context.Background(), bound)
	if ForContext(ctx) != bound {
		t.Fatal("ForContext must return the stashed handle")
	}
}
