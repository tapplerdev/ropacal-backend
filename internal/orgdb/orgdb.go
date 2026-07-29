// Package orgdb provides an organization-bound database handle for the
// multi-tenancy migration (migrations/add_multi_tenancy_rls.sql).
//
// A bound *DB runs every statement inside a transaction that first executes
//
//	SELECT set_config('app.org_id', $orgID, true)
//
// so the Row-Level Security policies see the caller's tenant, and the setting
// dies with the transaction — it can never leak onto a pooled connection.
// (set_config(..., is_local=true) is exactly SET LOCAL; SET LOCAL itself cannot
// take a bind parameter, which is why the function form is used.)
//
// DARK-SHIP MODE: until the tenancy migration has run (no `organizations`
// table), every handle constructed through Init/From/System/ForEachActiveOrg is
// a PASSTHROUGH that delegates straight to the root *sqlx.DB — no transactions,
// no set_config, byte-identical to pre-tenancy behavior. Mode is detected ONCE
// at boot by Init; flipping modes requires a redeploy (deliberate: the Go
// models and the migration must ship together anyway).
package orgdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type ctxKey struct{}

// DB is a database handle bound to exactly one organization (or, in
// passthrough mode, to the whole single-tenant database). The zero value is
// unusable; construct with New, Passthrough, or System.
//
// Safe for concurrent use: handlers may hand it to a post-response goroutine
// (see handlers/shifts.go StartShift's async broadcast) — each statement opens
// its own transaction, nothing is shared but the pool.
type DB struct {
	root        *sqlx.DB
	orgID       string
	passthrough bool

	mu     sync.Mutex
	parked []*sqlx.Tx // read-only txs still feeding open *Rows; reaped by Release
}

// New returns a handle bound to orgID. Every statement it issues carries
// app.org_id = orgID for its transaction's lifetime.
func New(root *sqlx.DB, orgID string) (*DB, error) {
	if root == nil {
		return nil, errors.New("orgdb: nil root pool")
	}
	if _, err := uuid.Parse(orgID); err != nil {
		return nil, fmt.Errorf("orgdb: invalid organization id %q: %w", orgID, err)
	}
	return &DB{root: root, orgID: orgID}, nil
}

// Passthrough returns a handle that delegates every call directly to root with
// no transaction wrapping and no app.org_id — pre-tenancy behavior, byte for
// byte. Used in dark-ship (single-tenant) mode and by unit tests.
func Passthrough(root *sqlx.DB) *DB {
	return &DB{root: root, passthrough: true}
}

// System is the escape hatch for non-request contexts (background workers,
// boot jobs) that need an org-bound handle without an *http.Request. In
// single-tenant (pre-migration) mode it returns a passthrough regardless of
// orgID, preserving today's behavior exactly.
func System(root *sqlx.DB, orgID string) (*DB, error) {
	if !Migrated() {
		return Passthrough(root), nil
	}
	return New(root, orgID)
}

// OrgID reports the bound organization id ("" for passthrough handles).
func (d *DB) OrgID() string { return d.orgID }

// NewContext stores the handle for the request's lifetime.
func NewContext(ctx context.Context, d *DB) context.Context {
	return context.WithValue(ctx, ctxKey{}, d)
}

// FromContext returns the request-bound handle, or nil if none was stashed.
func FromContext(ctx context.Context) *DB {
	d, _ := ctx.Value(ctxKey{}).(*DB)
	return d
}

// From is the one-line handler pickup: `db := orgdb.From(r)`.
//
// It returns the handle the org middleware bound to the request. When tenancy
// is live and no handle is present (a route wired without middleware.Auth/Org),
// it panics — deliberately: an unscoped query under RLS must surface on the
// first request as a loud 500 (chi's Recoverer), never degrade into a silent
// cross-tenant read. In single-tenant mode it falls back to the shared
// passthrough handle so public (unauthenticated) routes behave exactly as
// today.
func From(r *http.Request) *DB {
	return ForContext(r.Context())
}

// ForContext is From for callers that hold a context rather than a request
// (stores resolving per-call, chat tools, the digest trigger).
func ForContext(ctx context.Context) *DB {
	if d := FromContext(ctx); d != nil {
		return d
	}
	s := state.Load()
	if s == nil {
		panic("orgdb: no organization context and orgdb.Init was never called " +
			"(tests must either stash a handle with orgdb.NewContext or construct " +
			"handlers around orgdb.Passthrough)")
	}
	if s.migrated {
		panic("orgdb: no organization context on request — route is missing middleware.Auth/Org")
	}
	return s.neutral
}

// begin is the ONLY place a bound transaction is created; every wrapped method
// funnels through it, so there is exactly one line to audit for "did we set
// the GUC". Returned transactions already carry app.org_id.
func (d *DB) begin() (*sqlx.Tx, error) {
	tx, err := d.root.Beginx()
	if err != nil {
		return nil, err
	}
	if d.passthrough {
		return tx, nil
	}
	if _, err := tx.Exec(`SELECT set_config('app.org_id', $1, true)`, d.orgID); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("orgdb: set org context: %w", err)
	}
	return tx, nil
}

// beginCtx is begin with a caller context (used by the *Context variants).
func (d *DB) beginCtx(ctx context.Context) (*sqlx.Tx, error) {
	tx, err := d.root.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if d.passthrough {
		return tx, nil
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.org_id', $1, true)`, d.orgID); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("orgdb: set org context: %w", err)
	}
	return tx, nil
}

// ── statement-scoped: result fully consumed, so we commit before returning ──

// Get mirrors (*sqlx.DB).Get. sql.ErrNoRows propagates unchanged — dozens of
// handler call sites test for it.
func (d *DB) Get(dest interface{}, query string, args ...interface{}) error {
	if d.passthrough {
		return d.root.Get(dest, query, args...)
	}
	tx, err := d.begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := tx.Get(dest, query, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// Select mirrors (*sqlx.DB).Select.
func (d *DB) Select(dest interface{}, query string, args ...interface{}) error {
	if d.passthrough {
		return d.root.Select(dest, query, args...)
	}
	tx, err := d.begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := tx.Select(dest, query, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// GetContext mirrors (*sqlx.DB).GetContext.
func (d *DB) GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	if d.passthrough {
		return d.root.GetContext(ctx, dest, query, args...)
	}
	tx, err := d.beginCtx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := tx.GetContext(ctx, dest, query, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// SelectContext mirrors (*sqlx.DB).SelectContext.
func (d *DB) SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	if d.passthrough {
		return d.root.SelectContext(ctx, dest, query, args...)
	}
	tx, err := d.beginCtx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := tx.SelectContext(ctx, dest, query, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// Exec mirrors (*sqlx.DB).Exec. RowsAffected is materialised before COMMIT so
// the returned sql.Result stays valid after the transaction ends.
func (d *DB) Exec(query string, args ...interface{}) (sql.Result, error) {
	if d.passthrough {
		return d.root.Exec(query, args...)
	}
	tx, err := d.begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.Exec(query, args...)
	if err != nil {
		return nil, err
	}
	n, raErr := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return staticResult{n: n, err: raErr}, nil
}

// ExecContext mirrors (*sqlx.DB).ExecContext.
func (d *DB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if d.passthrough {
		return d.root.ExecContext(ctx, query, args...)
	}
	tx, err := d.beginCtx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	n, raErr := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return staticResult{n: n, err: raErr}, nil
}

// NamedExec mirrors (*sqlx.DB).NamedExec.
func (d *DB) NamedExec(query string, arg interface{}) (sql.Result, error) {
	if d.passthrough {
		return d.root.NamedExec(query, arg)
	}
	tx, err := d.begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.NamedExec(query, arg)
	if err != nil {
		return nil, err
	}
	n, raErr := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return staticResult{n: n, err: raErr}, nil
}

// staticResult is a fully-materialised sql.Result, valid after COMMIT.
type staticResult struct {
	n   int64
	err error
}

func (r staticResult) RowsAffected() (int64, error) { return r.n, r.err }
func (r staticResult) LastInsertId() (int64, error) {
	// Matches lib/pq, which has no LastInsertId support on Postgres.
	return 0, errors.New("orgdb: LastInsertId is not supported by postgres")
}

// ── deferred-execution Row: keeps `db.QueryRow(…).Scan(…)` call sites intact ──

// Row defers execution until Scan so the whole statement (BEGIN, set_config,
// query, COMMIT) runs in one place and no transaction is held open between
// QueryRow and Scan.
type Row struct {
	d    *DB
	q    string
	args []interface{}
}

// QueryRow mirrors (*sqlx.DB).QueryRow textually: `db.QueryRow(q, a).Scan(&x)`
// compiles unchanged. The returned *Row runs the query at Scan time.
func (d *DB) QueryRow(query string, args ...interface{}) *Row {
	return &Row{d: d, q: query, args: args}
}

// Scan executes the deferred query and scans the single result row.
// sql.ErrNoRows propagates exactly as with (*sql.Row).Scan.
func (r *Row) Scan(dest ...interface{}) error {
	if r.d.passthrough {
		return r.d.root.QueryRow(r.q, r.args...).Scan(dest...)
	}
	tx, err := r.d.begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := tx.QueryRow(r.q, r.args...).Scan(dest...); err != nil {
		return err
	}
	return tx.Commit()
}

// ── lazily-consumed rows: park the tx, reap at Release ───────────────────────
//
// All Query/Queryx handler call sites are plain SELECTs iterated within the
// request (verified in the tenancy design doc §4.5), so the transaction is
// parked and rolled back by Release at end of request — for a read-only tx,
// rollback is equivalent to commit. Callers already `defer rows.Close()`.
//
// NOTE: a Queryx/Query issued from a post-response goroutine AFTER the
// middleware's Release has run would park a tx nobody reaps. No such site
// exists today (the one post-response goroutine uses Select); keep it that way.

func (d *DB) park(tx *sqlx.Tx) {
	d.mu.Lock()
	d.parked = append(d.parked, tx)
	d.mu.Unlock()
}

// Query mirrors (*sqlx.DB).Query.
func (d *DB) Query(query string, args ...interface{}) (*sql.Rows, error) {
	if d.passthrough {
		return d.root.Query(query, args...)
	}
	tx, err := d.begin()
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	d.park(tx)
	return rows, nil
}

// Queryx mirrors (*sqlx.DB).Queryx.
func (d *DB) Queryx(query string, args ...interface{}) (*sqlx.Rows, error) {
	if d.passthrough {
		return d.root.Queryx(query, args...)
	}
	tx, err := d.begin()
	if err != nil {
		return nil, err
	}
	rows, err := tx.Queryx(query, args...)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	d.park(tx)
	return rows, nil
}

// QueryRowx mirrors (*sqlx.DB).QueryRowx (required by sqlx.Ext / sqlx.Queryer;
// no handler calls it directly). *sqlx.Row cannot carry a construction error,
// so a failure to open the org-scoped transaction panics rather than silently
// running unscoped — chi's Recoverer turns that into a 500, which is the same
// outcome the caller would see from a dead pool anyway.
func (d *DB) QueryRowx(query string, args ...interface{}) *sqlx.Row {
	if d.passthrough {
		return d.root.QueryRowx(query, args...)
	}
	tx, err := d.begin()
	if err != nil {
		panic(fmt.Sprintf("orgdb: QueryRowx could not open org-scoped transaction: %v", err))
	}
	row := tx.QueryRowx(query, args...)
	d.park(tx)
	return row
}

// Release rolls back every parked read-only transaction. The org middleware
// defers it, so it runs on panic paths too. Safe to call multiple times; the
// handle stays usable afterwards (post-response goroutines open fresh
// statement-scoped transactions).
func (d *DB) Release() {
	d.mu.Lock()
	parked := d.parked
	d.parked = nil
	d.mu.Unlock()
	for _, tx := range parked {
		_ = tx.Rollback()
	}
}

// ── explicit transactions: keeps every `tx, err := db.Beginx()` site intact ──

// Beginx returns a transaction with app.org_id already set. The signature
// matches (*sqlx.DB).Beginx, so the 27 existing handler call sites and every
// downstream tx.* / itinerary.X(tx, …) / moverequest.Y(tx, …) call are
// byte-identical. The caller owns Commit/Rollback exactly as before.
func (d *DB) Beginx() (*sqlx.Tx, error) { return d.begin() }

// ── sqlx.Ext conformance: domain funcs taking sqlx.Ext accept *DB as-is ──────

func (d *DB) DriverName() string         { return d.root.DriverName() }
func (d *DB) Rebind(query string) string { return d.root.Rebind(query) }
func (d *DB) BindNamed(query string, arg interface{}) (string, []interface{}, error) {
	return d.root.BindNamed(query, arg)
}

var (
	_ sqlx.Ext     = (*DB)(nil)
	_ sqlx.Queryer = (*DB)(nil)
	_ sqlx.Execer  = (*DB)(nil)
)
