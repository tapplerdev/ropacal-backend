package database

import (
	"fmt"
	"log"

	"ropacal-backend/internal/orgdb"

	"github.com/jmoiron/sqlx"
)

// InitTenancy wires the multi-tenancy plumbing at boot. MUST run after
// Migrate. It (1) detects — once — whether migrations/add_multi_tenancy_rls.sql
// has been applied (organizations table present) and puts orgdb into the
// matching mode, and (2) when tenancy is live, asserts that RLS is actually
// filtering rows for this connection role, refusing to serve if it provably
// is not.
//
// Pre-migration this is a no-op beyond the detection query: the server keeps
// today's single-tenant behavior byte-for-byte.
func InitTenancy(db *sqlx.DB) error {
	if err := orgdb.Init(db); err != nil {
		return err
	}
	return AssertRLSEnforced(db)
}

// AssertRLSEnforced is the boot-time isolation tripwire.
//
// When the tenancy migration is live (organizations table exists) and the
// connection role is subject to RLS, a session with NO app.org_id set must not
// be able to see any rows in a tenant table. The migration's policies use
// current_setting('app.org_id', true) (missing_ok), so an unscoped SELECT
// fails CLOSED with zero rows rather than raising; a strict-form policy would
// error instead. Either outcome passes. Visible rows mean the policies are
// not being evaluated — serving traffic in that state would leak every
// tenant's data to every tenant, so the caller must treat an error here as
// fatal.
//
// The check skips cleanly (returns nil) whenever it cannot prove a problem:
//   - tenancy not migrated yet (dark ship — pre-migration deploys),
//   - RLS not (yet) enabled on bins (partial/manual migration state),
//   - the role is a superuser or has BYPASSRLS (policies are unconditionally
//     bypassed for it, so the probe proves nothing — it logs a screaming
//     warning instead, per migration Part 9).
func AssertRLSEnforced(db *sqlx.DB) error {
	if !orgdb.Migrated() {
		log.Println("🛡️  [RLS] Tenancy not migrated — RLS assertion skipped (single-tenant mode)")
		return nil
	}

	var role struct {
		Super  bool `db:"rolsuper"`
		Bypass bool `db:"rolbypassrls"`
	}
	if err := db.Get(&role, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`); err != nil {
		return fmt.Errorf("rls assertion: reading current role privileges: %w", err)
	}
	if role.Super || role.Bypass {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("🚨 [RLS] WARNING: TENANCY IS LIVE BUT THIS CONNECTION ROLE BYPASSES RLS")
		log.Printf("   current_user is superuser=%v bypassrls=%v — every org-isolation policy", role.Super, role.Bypass)
		log.Println("   is INERT on this connection. Tenant isolation is NOT enforced.")
		log.Println("   Point DATABASE_URL at a non-superuser app role without BYPASSRLS")
		log.Println("   (see migrations/add_multi_tenancy_rls.sql Part 9).")
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		return nil
	}

	var rls struct {
		Enabled bool `db:"relrowsecurity"`
		Forced  bool `db:"relforcerowsecurity"`
	}
	if err := db.Get(&rls, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'public.bins'::regclass`); err != nil {
		return fmt.Errorf("rls assertion: reading bins RLS flags: %w", err)
	}
	if !rls.Enabled {
		log.Println("⚠️  [RLS] organizations table exists but RLS is not enabled on bins — partial migration state; assertion skipped")
		return nil
	}
	if !rls.Forced {
		log.Println("⚠️  [RLS] bins has RLS enabled but not FORCED — a table-owner connection would bypass it (migration Part 10 sets FORCE)")
	}

	// Probe: in a throwaway transaction, explicitly blank app.org_id (so a
	// dirty pooled connection cannot fake a pass) and count bins unscoped.
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("rls assertion: begin probe tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`SELECT set_config('app.org_id', '', true)`); err != nil {
		return fmt.Errorf("rls assertion: clearing app.org_id for probe: %w", err)
	}
	var visible int
	if err := tx.Get(&visible, `SELECT count(*) FROM bins`); err != nil {
		// A strict current_setting('app.org_id') policy raises on an unset GUC.
		// That is RLS enforcing, loudly — exactly what we want.
		log.Printf("🛡️  [RLS] Enforcement verified: unscoped probe errored as a strict policy should (%v)", err)
		return nil
	}
	if visible > 0 {
		return fmt.Errorf(
			"RLS IS NOT FILTERING: an unscoped 'SELECT count(*) FROM bins' (no app.org_id) returned %d row(s); "+
				"org-isolation policies are not being evaluated for role in use — REFUSING TO SERVE. "+
				"Check ENABLE/FORCE ROW LEVEL SECURITY and the role's grants (migration Parts 9-10)", visible)
	}
	log.Println("🛡️  [RLS] Enforcement verified: unscoped probe sees 0 rows (fail-closed)")
	return nil
}
