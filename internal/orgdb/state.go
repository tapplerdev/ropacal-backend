package orgdb

import (
	"fmt"
	"log"
	"sync/atomic"

	"github.com/jmoiron/sqlx"
)

// tenancyState is the boot-time mode snapshot. Immutable after Init.
type tenancyState struct {
	migrated bool     // organizations table exists → tenancy is live
	root     *sqlx.DB // the process-wide pool
	neutral  *DB      // shared passthrough handle used while single-tenant
}

var state atomic.Pointer[tenancyState]

// Init detects — ONCE, at boot, after database.Migrate — whether the tenancy
// migration (migrations/add_multi_tenancy_rls.sql) has been applied, keyed on
// the existence of the organizations table. Until it has, every handle this
// package hands out is a passthrough and behavior is byte-identical to the
// pre-tenancy build. Call exactly once from main before serving.
func Init(root *sqlx.DB) error {
	if root == nil {
		return fmt.Errorf("orgdb: Init called with nil pool")
	}
	var migrated bool
	if err := root.Get(&migrated, `SELECT to_regclass('public.organizations') IS NOT NULL`); err != nil {
		return fmt.Errorf("orgdb: detecting tenancy mode: %w", err)
	}
	state.Store(&tenancyState{
		migrated: migrated,
		root:     root,
		neutral:  Passthrough(root),
	})
	if migrated {
		log.Println("🏢 [Tenancy] organizations table present — org-scoped mode ACTIVE (every statement carries app.org_id)")
	} else {
		log.Println("🏢 [Tenancy] organizations table absent — single-tenant passthrough mode (dark ship; run migrations/add_multi_tenancy_rls.sql then redeploy to activate)")
	}
	return nil
}

// Migrated reports whether the tenancy migration has run (detected at boot).
// False when Init has not been called (unit tests), which keeps every code
// path on pre-tenancy behavior.
func Migrated() bool {
	s := state.Load()
	return s != nil && s.migrated
}

// Root returns the process-wide pool handed to Init, for the few deliberate
// unscoped consumers (org enumeration, login's cross-org lookup). Nil before
// Init.
func Root() *sqlx.DB {
	if s := state.Load(); s != nil {
		return s.root
	}
	return nil
}

// Neutral returns the shared single-tenant passthrough handle, or nil when
// tenancy is live (there is deliberately no neutral handle then) or Init has
// not run.
func Neutral() *DB {
	s := state.Load()
	if s == nil || s.migrated {
		return nil
	}
	return s.neutral
}

// setStateForTest swaps the package state and returns a restore func.
// Test use only.
func setStateForTest(s *tenancyState) func() {
	prev := state.Load()
	state.Store(s)
	return func() {
		if prev == nil {
			var nilState *tenancyState
			state.Store(nilState)
			return
		}
		state.Store(prev)
	}
}

// ConfigConflictTarget returns the ON CONFLICT target for upserts into the
// config table: "(key)" while single-tenant, "(organization_id, key)" once the
// tenancy migration has replaced config's UNIQUE(key) with
// UNIQUE(organization_id, key). Centralised so the five config upsert sites
// flip together and the pre-migration build keeps working byte-identically.
func ConfigConflictTarget() string {
	if Migrated() {
		return "(organization_id, key)"
	}
	return "(key)"
}

// ActiveOrgIDs enumerates the active organizations, via the root pool.
//
// NOTE (migration Part 10): the organizations table's RLS policy hides all
// rows from a session with no app.org_id. Today the app connects as postgres
// (policies bypassed) so this works; once DATABASE_URL moves to a non-
// superuser app role, that role needs the second permissive read policy the
// migration prescribes — ForEachActiveOrg logs loudly if this ever returns
// zero rows so the failure is visible, not silent.
func ActiveOrgIDs(root *sqlx.DB) ([]string, error) {
	var ids []string
	err := root.Select(&ids, `SELECT id FROM organizations WHERE status = 'active' ORDER BY created_at, id`)
	return ids, err
}

// ForEachActiveOrg runs fn once per active organization with an org-bound
// handle — the canonical loop for the six background workers. While tenancy is
// dark (or Init has not run) it invokes fn exactly once with a passthrough
// handle, preserving single-tenant behavior byte-for-byte.
//
// One organization's failure (error or panic) is logged and does NOT stop the
// remaining organizations.
func ForEachActiveOrg(root *sqlx.DB, tag string, fn func(d *DB) error) {
	if !Migrated() {
		runForOrg(Passthrough(root), tag, "", fn)
		return
	}
	ids, err := ActiveOrgIDs(root)
	if err != nil {
		log.Printf("❌ [%s] Failed to enumerate organizations: %v", tag, err)
		return
	}
	if len(ids) == 0 {
		log.Printf("🚨 [%s] ZERO active organizations visible — if tenants exist, the connection role "+
			"is being filtered by the organizations RLS policy (see migration Part 10: run as a role "+
			"that can read organizations, or add the permissive read policy). Skipping this cycle.", tag)
		return
	}
	for _, id := range ids {
		d, err := New(root, id)
		if err != nil {
			log.Printf("❌ [%s] org %s: %v", tag, id, err)
			continue
		}
		runForOrg(d, tag, id, fn)
	}
}

// runForOrg isolates one org's execution: recovers panics and logs errors so
// the caller's loop always continues.
func runForOrg(d *DB, tag, orgID string, fn func(d *DB) error) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("🚨 [%s] PANIC while processing org %q: %v", tag, orgID, rec)
		}
	}()
	if err := fn(d); err != nil {
		log.Printf("❌ [%s] org %q: %v", tag, orgID, err)
	}
}
