//go:build pgintegration

package handlers

import (
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// shadowPool returns a pool whose connections look tables up in a scratch
// schema before public (search_path), so a test can stand a broken table or
// view in for one real table while every other table stays real. Other pools,
// and so other tests, never see the scratch schema. In ddl, {s} is the scratch
// schema and {role} the app role.
func shadowPool(t *testing.T, admin *sqlx.DB, ddl ...string) *sqlx.DB {
	t.Helper()
	u, err := url.Parse(os.Getenv("BINLY_TEST_DATABASE_URL")) // checked local by pgtest.Connect
	if err != nil {
		t.Fatal(err)
	}
	schema := "shadow_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	role := pq.QuoteIdentifier(u.User.Username())
	admin.MustExec(`CREATE SCHEMA ` + schema)
	t.Cleanup(func() {
		// lock_timeout: a transaction left open on the stand-in (a handle never
		// released) would otherwise hang this DROP until the package times out.
		tx := admin.MustBegin()
		defer tx.Rollback() //nolint:errcheck // no-op after Commit
		tx.MustExec(`SET LOCAL lock_timeout = '10s'`)
		if _, err := tx.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Errorf("dropping %s: %v", schema, err)
			return
		}
		if err := tx.Commit(); err != nil {
			t.Errorf("dropping %s: %v", schema, err)
		}
	})
	admin.MustExec(`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role)
	fill := strings.NewReplacer("{s}", schema, "{role}", role)
	for _, stmt := range ddl {
		admin.MustExec(fill.Replace(stmt))
	}

	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	pool := sqlx.MustConnect("postgres", u.String())
	t.Cleanup(func() { pool.Close() })
	return pool
}
