-- AirTag tracking becomes a per-organization capability, ON for ropacal only.
--
-- The FindMy bridge serves ONE company's Apple accounts and knows nothing about
-- organizations. Every org nevertheless got the AirTag surfaces — the tracker
-- page, drift alerts, the daily battery report — and the battery report was
-- worse than clutter: an org with no AirTags read an empty table, fell back to
-- the bridge, and pushed ropacal's tags (bin numbers, street addresses) to its
-- own admins every morning.
--
-- With this flag the backend skips drift checks and battery reports, 404s the
-- AirTag endpoints, and never resolves a bridge location into an org that has
-- it off; the dashboard and app hide the surfaces.
--
-- DO NOT turn it on for a second organization until the bridge is org-aware
-- (see TENANCY_BACKLOG.md, binly-findmy-bridge findings): today it would mix
-- both companies' Apple sessions and tag lists.

-- +goose Up
-- +goose StatementBegin

-- ADD COLUMN takes ACCESS EXCLUSIVE on organizations, and logins, membership
-- checks and every tenant-row write (each has a foreign key to organizations)
-- touch it. Waiting behind a long-lived transaction would queue all of them;
-- fail fast instead — the boot fails, the deploy restarts, the old instance
-- keeps serving.
SET LOCAL lock_timeout = '5s';
-- A fresh database's baseline (00001) leaves row_security off on the session
-- goose reuses, under which the UPDATE below errors instead of filtering.
-- Production never ran 00001; this only hardens fresh environments.
SET LOCAL row_security = on;

ALTER TABLE organizations
    ADD COLUMN IF NOT EXISTS airtag_tracking BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN organizations.airtag_tracking IS
    'AirTag tracking (FindMy bridge) is enabled for this org. Single-tenant bridge: ropacal only until it is org-aware.';

-- organizations is under FORCE row-level security, and its write policy only
-- matches the row whose id equals app.org_id. A migration runs with no tenant
-- bound, so a bare UPDATE here would silently match NOTHING — leaving ropacal
-- with AirTags switched off. Bind this transaction to ropacal first (the id is
-- fixed: it is the org every pre-tenancy row was assigned to). On a database
-- without ropacal — a fresh environment — this updates zero rows, correctly.
SELECT set_config('app.org_id', '00000000-0000-0000-0000-000000000001', true);
UPDATE organizations SET airtag_tracking = true
 WHERE id = '00000000-0000-0000-0000-000000000001';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Revert the code FIRST: login reads this column, so dropping it under the
-- current binary makes every login 500.
ALTER TABLE organizations DROP COLUMN IF EXISTS airtag_tracking;
-- +goose StatementEnd
