package services

import "database/sql"

// Querier is the query/exec subset shared by *sqlx.DB and *orgdb.DB.
//
// Service helpers that run on both request paths (org-scoped handles under
// RLS) and worker/boot paths take this seam instead of a concrete pool, so
// WHO is scoped is decided entirely by the caller's handle: hand an
// *orgdb.DB and every query here carries app.org_id; hand the root pool and
// it is deliberately unscoped. (QueryRow is intentionally absent — its return
// type differs between the two implementations; use Get.)
type Querier interface {
	Get(dest interface{}, query string, args ...interface{}) error
	Select(dest interface{}, query string, args ...interface{}) error
	Exec(query string, args ...interface{}) (sql.Result, error)
}
