-- Migration: Shared-database multi-tenancy via organization_id + Row Level Security
-- Purpose: Convert the single-tenant schema to a shared-database / shared-schema
--          multi-tenant model. Every tenant-scoped table gains an `organization_id`
--          column, all existing data is backfilled to one default organization, and
--          Postgres RLS enforces isolation using current_setting('app.org_id').
-- Date: 2026-07-29
-- Target: PostgreSQL 15+ (uses `ON DELETE SET NULL (column_list)`, `security_invoker`
--         views). Verified against the production database: PostgreSQL 17.7.
--
-- ============================================================================
--  HOW TO RUN
-- ============================================================================
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f migrations/add_multi_tenancy_rls.sql
--
--   The whole file is idempotent and is designed to run inside ONE transaction so
--   a partial failure rolls back cleanly. Do NOT add CREATE INDEX CONCURRENTLY —
--   it cannot run in a transaction block. The largest table is
--   driver_location_history (~95k rows / 23 MB), so every rewrite/scan here is a
--   matter of seconds, not minutes.
--
-- ============================================================================
--  !!! READ BEFORE DEPLOYING — APPLICATION-BREAKING CHANGES !!!
-- ============================================================================
--
--  (1) `SELECT *` + sqlx will 500 the moment this runs.
--      The app runs `SELECT *` against users, bins, shifts, route_tasks,
--      no_go_zones, bin_move_requests, potential_locations, fcm_tokens and
--      ai_recommendations and scans the result into a struct. sqlx fails with
--      "missing destination name organization_id" -> HTTP 500. `handlers/auth.go`
--      does `SELECT * FROM users`, so **login breaks for everyone** until the Go
--      models carry the column.
--      => Ship `OrganizationID string \`db:"organization_id" json:"organization_id"\``
--         on every affected struct in internal/models/ FIRST, deploy, and only
--         then run this migration.
--
--  (2) `ON CONFLICT (key)` on `config` breaks (Part 6 makes the key composite).
--      Fix these 5 call sites to `ON CONFLICT (organization_id, key)`:
--        internal/database/database.go:810
--        internal/handlers/config.go:92
--        internal/handlers/airtag_locations_internal.go:112
--        internal/handlers/notification_settings.go:203
--        internal/services/digest_scheduler.go:789
--      (`ON CONFLICT (driver_id)`, `(user_id)`, `(token)`, `(zip)`, `(id)` are all
--       unaffected — those keys stay as they are.)
--
--  (3) RLS IS INERT UNTIL THE APP STOPS CONNECTING AS `postgres`. See Part 9.
--
--  (4) The app must set `app.org_id` on every connection/transaction, or every
--      query returns 0 rows and every INSERT fails NOT NULL. See Part 9.
--
-- ============================================================================
--  TABLE CLASSIFICATION (40 tables in public, all accounted for)
-- ============================================================================
--  TENANT-SCOPED (35) — get organization_id + RLS. Row counts at 2026-07-29:
--    ai_recommendations 168 | airtag_accounts 3 | airtag_keys 76
--    airtag_locations 72 | app_error_logs 0 | bin_change_log 283
--    bin_check_recommendations 0 | bin_features 79 | bin_move_requests 157
--    bin_watchlist 13 | bins 134 | checks 2333 | config 8
--    driver_current_location 8 | driver_location_history 94647
--    driver_location_snapshots 2 | driver_locations 6244 | fcm_tokens 54
--    move_request_history 508 | moves 32 | no_go_zones 114
--    notification_log 2024 | placement_decisions 0 | potential_locations 266
--    route_bins 82 | route_tasks 7845 | routes 3 | shift_bins 0
--    shift_history 554 | shifts 568 | user_notification_preferences 3
--    user_notifications 7061 | users 14 | zone_incidents 112
--    zone_risk_overrides 0
--    (total backfill: ~123,300 rows)
--
--  GLOBAL REFERENCE (1) — NO organization_id, shared by every tenant:
--    census_income_cache 59  (public US Census ACS ZIP data, seeded by
--    database.go:937-973, no tenant FK anywhere)
--
--  LEGACY BACKUPS (4) — contain tenant data, no app code path. Locked down
--  (RLS on, zero policies = deny-all to the app role) rather than scoped:
--    zz_backup_overnight_20260710_bins 14, ..._route_tasks 7,
--    zz_backup_overnight2_20260710_bins 10, ..._route_tasks 5
--    => These should be DROPPED. See the commented block in Part 11.
--
--  DERIVED / JOIN-ONLY tables (route_tasks, move_request_history, airtag_keys,
--  route_bins, shift_bins, bin_features, bin_watchlist, zone_risk_overrides,
--  user_notifications, fcm_tokens, user_notification_preferences,
--  driver_location*) still get their OWN organization_id. Justification:
--    a) An RLS policy that reaches the parent (`EXISTS (SELECT 1 FROM shifts ...)`)
--       is re-evaluated per row on every scan, and route_tasks is read on nearly
--       every driver request. A local column is an index lookup, a subquery is not.
--    b) A subquery policy on the parent is itself subject to the parent's RLS,
--       which makes correctness depend on policy evaluation order and makes
--       "parent row not visible" indistinguishable from "child does not exist".
--    c) Several of these tables are queried directly by primary key with no join
--       to the parent (route_tasks by task id, move_request_history by move id,
--       fcm_tokens by token) — there is no parent in scope to filter on.
--    d) The denormalised column is what makes the composite foreign keys in
--       Part 7 possible, which is the only mechanism that structurally prevents
--       cross-tenant parent references.
--
-- ============================================================================


-- ============================================================================
-- PART 0 — Guards
-- ============================================================================

DO $$
BEGIN
    IF current_setting('server_version_num')::int < 150000 THEN
        RAISE EXCEPTION 'This migration requires PostgreSQL 15+ (found %). '
                        'Parts 7 and 8 use ON DELETE SET NULL (col_list) and '
                        'security_invoker views.', current_setting('server_version');
    END IF;
END $$;


-- ============================================================================
-- PART 1 — organizations table + the default (existing) tenant
-- ============================================================================

CREATE TABLE IF NOT EXISTS organizations (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    slug       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'active'
               CHECK (status IN ('active', 'suspended', 'cancelled')),
    created_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,
    updated_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT
);

CREATE UNIQUE INDEX IF NOT EXISTS uidx_organizations_slug ON organizations (slug);

COMMENT ON TABLE organizations IS
    'Tenants. Every tenant-scoped table carries organization_id -> organizations.id, '
    'enforced by RLS against current_setting(''app.org_id'').';

-- The single tenant that owns all pre-migration data. Fixed UUID so the value is
-- deterministic across environments and this migration stays idempotent.
INSERT INTO organizations (id, name, slug)
VALUES ('00000000-0000-0000-0000-000000000001', 'Ropacal', 'ropacal')
ON CONFLICT (id) DO NOTHING;


-- ============================================================================
-- PART 2 — Add organization_id (nullable) to every tenant-scoped table
-- ============================================================================
-- Deliberately added WITHOUT a default first: a non-constant default
-- (current_setting) forces a full table rewrite under ACCESS EXCLUSIVE.
-- Add (instant, metadata-only) -> backfill -> set default -> set not null.
--
-- The canonical tenant-table list lives here and is reused by Parts 3, 4, 5,
-- 7, 10 and the Part 12 completeness assertion. Adding a table to this one list
-- is all it takes to bring it under tenancy.

CREATE TEMP TABLE _tenant_tables (tbl TEXT PRIMARY KEY) ON COMMIT DROP;
INSERT INTO _tenant_tables (tbl) VALUES
    ('ai_recommendations'),
    ('airtag_accounts'),
    ('airtag_keys'),
    ('airtag_locations'),
    ('app_error_logs'),
    ('bin_change_log'),
    ('bin_check_recommendations'),
    ('bin_features'),
    ('bin_move_requests'),
    ('bin_watchlist'),
    ('bins'),
    ('checks'),
    ('config'),
    ('driver_current_location'),
    ('driver_location_history'),
    ('driver_location_snapshots'),
    ('driver_locations'),
    ('fcm_tokens'),
    ('move_request_history'),
    ('moves'),
    ('no_go_zones'),
    ('notification_log'),
    ('placement_decisions'),
    ('potential_locations'),
    ('route_bins'),
    ('route_tasks'),
    ('routes'),
    ('shift_bins'),
    ('shift_history'),
    ('shifts'),
    ('user_notification_preferences'),
    ('user_notifications'),
    ('users'),
    ('zone_incidents'),
    ('zone_risk_overrides');

DO $$
DECLARE
    t TEXT;
BEGIN
    FOR t IN SELECT tbl FROM _tenant_tables ORDER BY tbl LOOP
        IF to_regclass('public.' || quote_ident(t)) IS NULL THEN
            RAISE EXCEPTION 'Expected tenant table public.% does not exist', t;
        END IF;
        EXECUTE format('ALTER TABLE public.%I ADD COLUMN IF NOT EXISTS organization_id TEXT', t);
    END LOOP;
END $$;


-- ============================================================================
-- PART 3 — Backfill every existing row to the default organization
-- ============================================================================

DO $$
DECLARE
    t       TEXT;
    n       BIGINT;
    total   BIGINT := 0;
BEGIN
    FOR t IN SELECT tbl FROM _tenant_tables ORDER BY tbl LOOP
        EXECUTE format(
            'UPDATE public.%I SET organization_id = %L WHERE organization_id IS NULL',
            t, '00000000-0000-0000-0000-000000000001');
        GET DIAGNOSTICS n = ROW_COUNT;
        total := total + n;
        IF n > 0 THEN
            RAISE NOTICE 'backfilled % row(s) in %', n, t;
        END IF;
    END LOOP;
    RAISE NOTICE 'total rows backfilled: %', total;
END $$;


-- ============================================================================
-- PART 4 — DEFAULT from the session GUC, NOT NULL, and FK to organizations
-- ============================================================================
-- The DEFAULT is the compatibility trick that lets the existing INSERT
-- statements keep working untouched: they never name organization_id, so the
-- default fills it from the session's app.org_id. If app.org_id is unset the
-- default evaluates to NULL and the write is rejected — by the RLS WITH CHECK
-- policy for the app role, or by this NOT NULL for a superuser. Either way the
-- system fails CLOSED; it never silently writes an unowned row. (Verified.)

DO $$
DECLARE
    t TEXT;
BEGIN
    FOR t IN SELECT tbl FROM _tenant_tables ORDER BY tbl LOOP
        EXECUTE format(
            'ALTER TABLE public.%I ALTER COLUMN organization_id '
            'SET DEFAULT NULLIF(current_setting(''app.org_id'', true), '''')', t);

        EXECUTE format('ALTER TABLE public.%I ALTER COLUMN organization_id SET NOT NULL', t);

        -- FK to organizations. RESTRICT on delete: deleting a tenant must be an
        -- explicit, ordered purge, never an accidental cascade across 35 tables.
        IF NOT EXISTS (
            SELECT 1 FROM pg_constraint
            WHERE conname = 'fk_' || t || '_organization'
              AND conrelid = ('public.' || quote_ident(t))::regclass
        ) THEN
            EXECUTE format(
                'ALTER TABLE public.%I ADD CONSTRAINT %I '
                'FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE RESTRICT',
                t, 'fk_' || t || '_organization');
        END IF;

        EXECUTE format(
            'COMMENT ON COLUMN public.%I.organization_id IS %L', t,
            'Owning tenant. Enforced by RLS policy org_isolation_' || t || '.');
    END LOOP;
END $$;


-- ============================================================================
-- PART 5 — Indexes
-- ============================================================================

-- 5a. Baseline: a plain btree on organization_id for every tenant table. Low
--     selectivity while there is one tenant; essential the moment there are two.
DO $$
DECLARE
    t TEXT;
BEGIN
    FOR t IN SELECT tbl FROM _tenant_tables ORDER BY tbl LOOP
        EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON public.%I (organization_id)',
                       'idx_' || t || '_org', t);
    END LOOP;
END $$;

-- 5b. Composite indexes that mirror the existing hot query patterns. RLS silently
--     prepends `organization_id = ...` to every predicate, so the pre-existing
--     single-column indexes (idx_bins_status, idx_shifts_status, ...) stop being
--     leading-column matches. These restore them.
CREATE INDEX IF NOT EXISTS idx_bins_org_status              ON bins (organization_id, status);
CREATE INDEX IF NOT EXISTS idx_bins_org_bin_number          ON bins (organization_id, bin_number);
CREATE INDEX IF NOT EXISTS idx_shifts_org_status            ON shifts (organization_id, status);
CREATE INDEX IF NOT EXISTS idx_shifts_org_driver            ON shifts (organization_id, driver_id);
CREATE INDEX IF NOT EXISTS idx_shifts_org_scheduled_date    ON shifts (organization_id, scheduled_date);
CREATE INDEX IF NOT EXISTS idx_route_tasks_org_shift_seq    ON route_tasks (organization_id, shift_id, is_deleted, sequence_order);
CREATE INDEX IF NOT EXISTS idx_route_tasks_org_move_request ON route_tasks (organization_id, move_request_id) WHERE move_request_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_checks_org_bin               ON checks (organization_id, bin_id);
CREATE INDEX IF NOT EXISTS idx_checks_org_checked_on        ON checks (organization_id, checked_on DESC);
CREATE INDEX IF NOT EXISTS idx_bmr_org_status               ON bin_move_requests (organization_id, status);
CREATE INDEX IF NOT EXISTS idx_bmr_org_bin                  ON bin_move_requests (organization_id, bin_id);
CREATE INDEX IF NOT EXISTS idx_bmr_org_scheduled_date       ON bin_move_requests (organization_id, scheduled_date);
CREATE INDEX IF NOT EXISTS idx_mrh_org_move_seq             ON move_request_history (organization_id, move_request_id, seq);
CREATE INDEX IF NOT EXISTS idx_users_org_role               ON users (organization_id, role);
CREATE INDEX IF NOT EXISTS idx_user_notifs_org_user_created ON user_notifications (organization_id, user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_notification_log_org_created ON notification_log (organization_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_potential_loc_org_status     ON potential_locations (organization_id, conversion_status);
CREATE INDEX IF NOT EXISTS idx_no_go_zones_org_status       ON no_go_zones (organization_id, status);
CREATE INDEX IF NOT EXISTS idx_zone_incidents_org_zone      ON zone_incidents (organization_id, zone_id);
CREATE INDEX IF NOT EXISTS idx_shift_history_org_ended      ON shift_history (organization_id, ended_at DESC);
CREATE INDEX IF NOT EXISTS idx_moves_org_bin                ON moves (organization_id, bin_id);
CREATE INDEX IF NOT EXISTS idx_dcl_org_connected            ON driver_current_location (organization_id, is_connected);
CREATE INDEX IF NOT EXISTS idx_bin_change_log_org_bin       ON bin_change_log (organization_id, bin_id);
CREATE INDEX IF NOT EXISTS idx_airtag_locations_org_binnum  ON airtag_locations (organization_id, bin_number);
CREATE INDEX IF NOT EXISTS idx_fcm_tokens_org_user          ON fcm_tokens (organization_id, user_id);


-- ============================================================================
-- PART 6 — Unique constraints
-- ============================================================================
-- Every primary key in this schema is a UUID/TEXT surrogate or a global sequence,
-- so no PK needs to change. Only the four secondary UNIQUE constraints and two
-- unique indexes are affected.

-- ----------------------------------------------------------------------------
-- 6a. config.key  ->  (organization_id, key)   *** MUST CHANGE ***
--     warehouse_location, notification_settings and the digest de-dupe keys
--     (last_morning_digest, last_daily_move_report, airtag_last_sync, ...) are
--     all per-tenant. A global unique key would give every tenant the same
--     warehouse. This is the one unambiguous composite.
-- ----------------------------------------------------------------------------
ALTER TABLE config DROP CONSTRAINT IF EXISTS config_key_key;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'uq_config_org_key') THEN
        ALTER TABLE config ADD CONSTRAINT uq_config_org_key UNIQUE (organization_id, key);
    END IF;
END $$;
-- idx_config_key is now fully covered by the composite constraint's index.
DROP INDEX IF EXISTS idx_config_key;

-- ----------------------------------------------------------------------------
-- 6b. users.email  ->  UNIQUE PER ORGANIZATION.  (decided 2026-07-29)
--     Owner's call: emails are scoped per-tenant and the login screen gains an
--     organization selector (the org's `slug`, already UNIQUE above). The login
--     handler therefore resolves (organization_id, email) — handlers/auth.go
--     must ship that change BEFORE this migration runs, or login with a
--     same-email collision becomes ambiguous. LoginRequest gains an
--     `organization` slug field; slug -> organizations.id -> composite lookup.
-- ----------------------------------------------------------------------------
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_email_key;
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'uq_users_org_email') THEN
        ALTER TABLE users ADD CONSTRAINT uq_users_org_email UNIQUE (organization_id, email);
    END IF;
END $$;

-- idx_users_email duplicates users_email_key's index exactly. Redundant either way.
DROP INDEX IF EXISTS idx_users_email;

-- ----------------------------------------------------------------------------
-- 6c. fcm_tokens.token  ->  STAYS GLOBALLY UNIQUE.  (recommended, not a guess)
--     An FCM registration token identifies a physical device install, not a
--     tenant. If the same token could exist in two orgs, a driver who changes
--     employer would keep receiving the previous tenant's push notifications on
--     the same handset — a cross-tenant data leak over FCM, outside RLS's reach.
--     Global uniqueness makes handlers/driver_location.go:427's
--     `ON CONFLICT(token) DO UPDATE` re-home the device to its new owner, which
--     is exactly the desired behaviour. That upsert must also start writing
--     organization_id (the DEFAULT covers the INSERT arm; add it to the SET arm).
-- ----------------------------------------------------------------------------
-- idx_fcm_tokens_token is an exact duplicate of fcm_tokens_token_key.
DROP INDEX IF EXISTS idx_fcm_tokens_token;

-- ----------------------------------------------------------------------------
-- 6d. airtag_accounts.email  ->  STAYS GLOBALLY UNIQUE.  (recommended)
--     This is an Apple iCloud login (with a plaintext password in the row) used
--     by the FindMy bridge. Two tenants registering the same Apple ID would mean
--     one tenant holding another's credentials and both bridges polling the same
--     account. Global uniqueness makes that collision an error instead of a leak.
-- ----------------------------------------------------------------------------
-- (no change)

-- ----------------------------------------------------------------------------
-- 6e. Partial/composite unique indexes -> prefix with organization_id.
--     bin_id and route_id are tenant-scoped UUIDs, so cross-tenant collision is
--     already impossible; prefixing is for index locality and defence in depth
--     (a unique violation must never be observable across a tenant boundary).
-- ----------------------------------------------------------------------------
DROP INDEX IF EXISTS uidx_bin_move_requests_one_open;
CREATE UNIQUE INDEX IF NOT EXISTS uidx_bin_move_requests_one_open
    ON bin_move_requests (organization_id, bin_id)
    WHERE status IN ('pending', 'assigned', 'in_progress');

DROP INDEX IF EXISTS idx_route_bins_unique;
CREATE UNIQUE INDEX IF NOT EXISTS idx_route_bins_unique
    ON route_bins (organization_id, route_id, bin_id);


-- ============================================================================
-- PART 7 — Cross-tenant foreign keys
-- ============================================================================
--  THE RISK, precisely:
--    Postgres runs referential-integrity checks with the privileges of the
--    referenced table's owner and they ALWAYS BYPASS ROW LEVEL SECURITY
--    (documented behaviour). RLS's INSERT/UPDATE WITH CHECK only validates the
--    new row's own organization_id column — it says nothing about whether the
--    parent it points at belongs to the same tenant.
--    So with single-column FKs, this succeeds:
--        INSERT INTO checks (bin_id, organization_id) VALUES (<org B's bin>, 'org-A');
--    Org A now owns a check row hanging off org B's bin. RLS will happily show
--    that row to org A, and any join that reaches through bin_id leaks org B's
--    bin. All 72 FKs in this schema have this shape.
--
--  THE FIX: composite foreign keys. Add UNIQUE (organization_id, id) to each
--  parent, then re-point every child FK at (organization_id, <col>). A
--  cross-tenant reference then fails at the constraint level, independently of
--  RLS, independently of application correctness.

-- 7a. Drop the four exact-duplicate FKs found in production (same columns, same
--     target, same action — they double every referential check).
ALTER TABLE checks              DROP CONSTRAINT IF EXISTS fk_checks_user;                       -- dup of checks_checked_by_fkey
ALTER TABLE potential_locations DROP CONSTRAINT IF EXISTS fk_potential_locations_shift;         -- dup of fk_potential_locations_assigned_shift
ALTER TABLE potential_locations DROP CONSTRAINT IF EXISTS fk_potential_locations_converted_via_shift; -- dup of potential_locations_converted_via_shift_id_fkey

-- 7b. UNIQUE (organization_id, id) on every table that is a FK target.
DO $$
DECLARE
    p TEXT;
BEGIN
    FOREACH p IN ARRAY ARRAY[
        'users', 'bins', 'shifts', 'bin_move_requests', 'potential_locations',
        'no_go_zones', 'routes', 'airtag_accounts', 'notification_log',
        'checks', 'moves'
    ] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'uq_' || p || '_org_id') THEN
            EXECUTE format('ALTER TABLE public.%I ADD CONSTRAINT %I UNIQUE (organization_id, id)',
                           p, 'uq_' || p || '_org_id');
        END IF;
    END LOOP;
END $$;

-- 7c. Rewrite every single-column FK between two tenant tables as a composite.
--     Driven off pg_constraint so it is complete by construction — a hand-written
--     list of 68 ALTERs is exactly where a table gets missed.
--
--     PREVIEW what this will do before running the migration:
--       SELECT c.relname AS child, con.conname, a.attname AS col,
--              pc.relname AS parent, con.confdeltype
--       FROM pg_constraint con
--       JOIN pg_class c  ON c.oid  = con.conrelid
--       JOIN pg_class pc ON pc.oid = con.confrelid
--       JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = con.conkey[1]
--       WHERE con.contype = 'f' AND array_length(con.conkey, 1) = 1
--         AND pc.relname <> 'organizations';
--
--     Delete actions are preserved. ON DELETE SET NULL uses the PG15+ column-list
--     form `SET NULL (child_col)` so that organization_id — which is NOT NULL —
--     is not also nulled out.
DO $$
DECLARE
    r          RECORD;
    del_clause TEXT;
    converted  INT := 0;
BEGIN
    FOR r IN
        SELECT con.oid,
               con.conname                    AS name,
               c.relname                      AS child,
               pc.relname                     AS parent,
               a.attname                      AS child_col,
               pa.attname                     AS parent_col,
               con.confdeltype                AS del,
               NOT a.attnotnull               AS child_col_nullable
        FROM pg_constraint con
        JOIN pg_class c        ON c.oid  = con.conrelid
        JOIN pg_class pc       ON pc.oid = con.confrelid
        JOIN pg_namespace n    ON n.oid  = c.relnamespace
        JOIN pg_attribute a    ON a.attrelid  = c.oid  AND a.attnum  = con.conkey[1]
        JOIN pg_attribute pa   ON pa.attrelid = pc.oid AND pa.attnum = con.confkey[1]
        WHERE n.nspname = 'public'
          AND con.contype = 'f'
          AND array_length(con.conkey, 1) = 1          -- skip already-composite
          AND a.attname <> 'organization_id'           -- skip the Part 4 org FKs
          AND c.relname  IN (SELECT tbl FROM _tenant_tables)
          AND pc.relname IN (SELECT tbl FROM _tenant_tables)
        ORDER BY c.relname, con.conname
    LOOP
        CASE r.del
            WHEN 'c' THEN del_clause := ' ON DELETE CASCADE';
            WHEN 'n' THEN
                -- A SET NULL FK on a NOT NULL child column is already broken today
                -- (the delete raises a not-null violation). Downgrade to NO ACTION
                -- rather than carry the latent failure forward. See the notes at
                -- the bottom of this file: bin_move_requests.requested_by and
                -- move_request_history.actor_id are both in this state.
                IF r.child_col_nullable THEN
                    del_clause := format(' ON DELETE SET NULL (%I)', r.child_col);
                ELSE
                    del_clause := '';
                    RAISE NOTICE 'FK %.% was ON DELETE SET NULL on NOT NULL column %; '
                                 'recreated as NO ACTION', r.child, r.name, r.child_col;
                END IF;
            ELSE del_clause := '';
        END CASE;

        EXECUTE format('ALTER TABLE public.%I DROP CONSTRAINT %I', r.child, r.name);
        EXECUTE format(
            'ALTER TABLE public.%I ADD CONSTRAINT %I '
            'FOREIGN KEY (organization_id, %I) REFERENCES public.%I (organization_id, %I)%s',
            r.child, r.name, r.child_col, r.parent, r.parent_col, del_clause);

        converted := converted + 1;
    END LOOP;

    RAISE NOTICE 'converted % foreign key(s) to composite (organization_id, col)', converted;
END $$;

-- 7d. Residual FK gaps that composite FKs cannot close — documented, not fixed here.
--   * driver_location_history.driver_id/shift_id are UUID while users.id/shifts.id
--     are TEXT, so the table has NO foreign keys at all and cannot get one without
--     a type change. It is org-scoped by column + RLS only. 94,647 rows, last
--     written 2026-06-01, zero references in the Go code — recommend archiving.
--   * bin_features.bin_id and placement_decisions.bin_id have no FK to bins.
--     Both are written by something outside this repository (bin_features was
--     last written 2026-07-26). Do not add a FK until that writer is identified.
--   * airtag_locations matches bins by bin_number, not bin_id, and bin_number is
--     NOT unique — production already has three duplicate numbers (56, 86, 116).
--     Any matching query MUST filter on organization_id or it will match another
--     tenant's bin. Index idx_airtag_locations_org_binnum (Part 5b) supports that.


-- ============================================================================
-- PART 8 — The view must not launder RLS
-- ============================================================================
-- shift_tasks_detailed joins route_tasks + bins + potential_locations +
-- bin_move_requests and is owned by postgres. A plain view executes against its
-- base tables with the VIEW OWNER's privileges and RLS context, so a tenant
-- querying this view would see EVERY tenant's tasks — RLS on the base tables is
-- bypassed entirely. security_invoker makes base-table access run as the caller.
-- This is the single easiest hole to miss in this whole migration.

DO $$
BEGIN
    IF to_regclass('public.shift_tasks_detailed') IS NOT NULL THEN
        EXECUTE 'ALTER VIEW public.shift_tasks_detailed SET (security_invoker = on)';
    END IF;
END $$;

-- NOTE: the view does not expose route_tasks.organization_id. Add it if callers
-- need to read the column back:
--   CREATE OR REPLACE VIEW shift_tasks_detailed AS SELECT rt.organization_id, rt.* ...
-- Any future view over tenant tables MUST be created WITH (security_invoker = on).


-- ============================================================================
-- PART 9 — *** THE APPLICATION ROLE. READ THIS. ***
-- ============================================================================
--
--  FINDING: the app connects as `postgres`, which on this Railway instance is
--           rolsuper = true, rolbypassrls = true, and is the OWNER of all 40
--           tables. Verified live.
--
--  CONSEQUENCE: with that connection, EVERY policy created in Part 10 is inert.
--
--    * A SUPERUSER always bypasses row level security. Unconditionally.
--    * A role with BYPASSRLS always bypasses row level security.
--    * FORCE ROW LEVEL SECURITY does NOT change either of those. All FORCE does
--      is remove the TABLE OWNER's implicit exemption. It has no effect on
--      superusers.
--
--  So: enabling RLS while connecting as postgres buys you exactly nothing. The
--  policies below will be created and then never evaluated. Every tenant would
--  see every other tenant's data. This is not a tuning detail — it is the
--  difference between this migration working and this migration being theatre.
--
--  RECOMMENDATION — create a dedicated non-superuser application role. Two
--  viable shapes:
--
--   OPTION A (pragmatic first step) — app role OWNS the tables, FORCE does the work.
--     Works with the current architecture, where database.Migrate(db) runs
--     CREATE TABLE / ALTER TABLE inline on every boot; the role needs DDL rights
--     for that to keep working. FORCE ROW LEVEL SECURITY (Part 10) is what makes
--     the owner obey its own policies.
--     Caveat: an owner can DROP POLICY or DISABLE ROW LEVEL SECURITY, so this is
--     a strong guard against application bugs but not against SQL injection.
--
--   OPTION B (target state, recommended) — app role owns nothing.
--     postgres keeps ownership and runs migrations as a separate deploy step;
--     the app role gets only SELECT/INSERT/UPDATE/DELETE + sequence USAGE and
--     cannot touch policies at all. Requires moving the inline migrations in
--     internal/database/database.go out of the server boot path — the right
--     move regardless, but it is a real refactor, so it is not assumed here.
--
--  Either way the app must stop using the postgres credentials.
--
--  >>> Run this block manually with a real password from your secret manager.
--  >>> It is left commented so this file never creates a credential on its own,
--  >>> and so the migration stays runnable before the role exists.
--
--   CREATE ROLE ropacal_app LOGIN PASSWORD '<from-secret-manager>' NOBYPASSRLS NOSUPERUSER;
--   GRANT CONNECT ON DATABASE railway TO ropacal_app;
--   GRANT USAGE ON SCHEMA public TO ropacal_app;
--   GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ropacal_app;
--   GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ropacal_app;
--   ALTER DEFAULT PRIVILEGES IN SCHEMA public
--       GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ropacal_app;
--   ALTER DEFAULT PRIVILEGES IN SCHEMA public
--       GRANT USAGE, SELECT ON SEQUENCES TO ropacal_app;
--   -- Option A only (app role owns the tables so inline migrations keep working):
--   --   GRANT CREATE ON SCHEMA public TO ropacal_app;
--   --   DO $reassign$ DECLARE t TEXT; BEGIN
--   --     FOR t IN SELECT tablename FROM pg_tables WHERE schemaname='public' LOOP
--   --       EXECUTE format('ALTER TABLE public.%I OWNER TO ropacal_app', t);
--   --     END LOOP;
--   --     EXECUTE 'ALTER VIEW public.shift_tasks_detailed OWNER TO ropacal_app';
--   --   END $reassign$;
--   -- Then point DATABASE_URL at ropacal_app and keep the postgres URL as a
--   -- break-glass/support credential only.
--
--  Backup grants for the zz_ tables are intentionally NOT given (Part 11).
--
-- ---------------------------------------------------------------------------
--  SETTING app.org_id — the other half of the contract
-- ---------------------------------------------------------------------------
--  internal/database/database.go opens ONE *sqlx.DB with database/sql defaults
--  (no SetMaxOpenConns anywhere) and shares it across all handlers. Connections
--  are pooled and reused between unrelated requests, so a plain
--  `SET app.org_id = ...` is a cross-tenant bug waiting to happen: the value
--  survives on the connection and the next request that forgets to set it
--  inherits the previous tenant's id.
--
--  Two safe patterns:
--    1. Per-transaction (preferred, and the only one that survives a future
--       transaction-pooling PgBouncer):
--           tx, _ := db.BeginTxx(ctx, nil)
--           tx.ExecContext(ctx, "SELECT set_config('app.org_id', $1, true)", orgID)
--           ... all work on tx ...
--       `true` = local to the transaction, reset on COMMIT/ROLLBACK.
--    2. Per-checked-out-connection:
--           conn, _ := db.Connx(ctx); defer conn.Close()
--           conn.ExecContext(ctx, "SELECT set_config('app.org_id', $1, false)", orgID)
--       Requires a guaranteed RESET before the connection returns to the pool.
--
--  Use set_config($1) rather than `SET app.org_id = ...` — SET does not accept
--  bind parameters, so the string form invites SQL injection on the org id.
--
--  Source the org id from the JWT, not the request body. Add `org_id` to the
--  claims in handlers/auth.go and read it in middleware/auth.go. Background
--  workers (digest scheduler, airtag monitor, move-request monitor, location
--  batch writer) have no request context — they must loop over organizations
--  and set app.org_id per tenant per iteration.


-- ============================================================================
-- PART 10 — ENABLE + FORCE ROW LEVEL SECURITY, one policy per tenant table
-- ============================================================================
-- Policy shape:
--   organization_id = NULLIF(current_setting('app.org_id', true), '')
--
--   * current_setting(..., true) returns NULL when the GUC is unset instead of
--     raising, so a request that forgot to set the tenant sees zero rows and
--     cannot insert — fail closed, not fail open.
--   * NULLIF collapses '' to NULL so a blank value cannot match anything.
--   * FOR ALL gives USING (visibility for SELECT/UPDATE/DELETE) and WITH CHECK
--     (validation for INSERT/UPDATE). The WITH CHECK is what stops a tenant from
--     re-parenting one of its own rows into someone else's organization.
--   * PERMISSIVE + a single policy per table = plain AND-ing onto every query.

DO $$
DECLARE
    t           TEXT;
    policy_name TEXT;
BEGIN
    FOR t IN SELECT tbl FROM _tenant_tables ORDER BY tbl LOOP
        policy_name := 'org_isolation_' || t;

        EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', t);

        EXECUTE format('DROP POLICY IF EXISTS %I ON public.%I', policy_name, t);
        EXECUTE format(
            'CREATE POLICY %I ON public.%I AS PERMISSIVE FOR ALL TO PUBLIC '
            'USING (organization_id = NULLIF(current_setting(''app.org_id'', true), '''')) '
            'WITH CHECK (organization_id = NULLIF(current_setting(''app.org_id'', true), ''''))',
            policy_name, t);
    END LOOP;
END $$;

-- The organizations table itself: a tenant may see only its own row.
ALTER TABLE organizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE organizations FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS org_isolation_organizations ON organizations;
CREATE POLICY org_isolation_organizations ON organizations
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING      (id = NULLIF(current_setting('app.org_id', true), ''))
    WITH CHECK (id = NULLIF(current_setting('app.org_id', true), ''));
-- NOTE: this makes tenant provisioning and the per-tenant background-worker loop
-- ("SELECT id FROM organizations") invisible to the app role. Run those as
-- postgres, or add a second permissive read policy for a dedicated admin role.


-- ============================================================================
-- PART 11 — Non-tenant tables
-- ============================================================================

-- 11a. GLOBAL REFERENCE: census_income_cache.
--      Public US Census ACS data keyed by ZIP, seeded by database.go, read by
--      chat_tools.go, chat_locations.go and placement_opportunities.go. No
--      organization_id, no RLS — every tenant reads the same rows, and there is
--      nothing tenant-identifying in them.
COMMENT ON TABLE census_income_cache IS
    'GLOBAL REFERENCE — public US Census ACS median income/population by ZIP. '
    'Intentionally NOT tenant-scoped and NOT under RLS: shared by all organizations.';

-- 11b. LEGACY BACKUPS: RLS on with zero policies == deny all, for everyone except
--      superusers. They hold real tenant data and no code reads them, so denying
--      the app role is strictly correct. postgres can still read them for recovery.
DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'zz_backup_overnight_20260710_bins',
        'zz_backup_overnight_20260710_route_tasks',
        'zz_backup_overnight2_20260710_bins',
        'zz_backup_overnight2_20260710_route_tasks'
    ] LOOP
        IF to_regclass('public.' || quote_ident(t)) IS NOT NULL THEN
            EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', t);
            EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', t);
            EXECUTE format('REVOKE ALL ON public.%I FROM PUBLIC', t);
        END IF;
    END LOOP;
END $$;
-- These snapshots served the 2026-07-10 overnight-shift correction and are done.
-- Drop them once you have confirmed they are no longer needed:
--   DROP TABLE IF EXISTS zz_backup_overnight_20260710_bins;
--   DROP TABLE IF EXISTS zz_backup_overnight_20260710_route_tasks;
--   DROP TABLE IF EXISTS zz_backup_overnight2_20260710_bins;
--   DROP TABLE IF EXISTS zz_backup_overnight2_20260710_route_tasks;


-- ============================================================================
-- PART 12 — Completeness assertion
-- ============================================================================
-- Hard stop if any table in public is neither tenant-scoped nor explicitly
-- allow-listed as global/backup. A table added later and forgotten here is a
-- cross-tenant data leak, so this fails the migration rather than warning.

DO $$
DECLARE
    missing TEXT[];
BEGIN
    SELECT array_agg(tablename ORDER BY tablename) INTO missing
    FROM pg_tables
    WHERE schemaname = 'public'
      AND tablename NOT IN (SELECT tbl FROM _tenant_tables)
      AND tablename NOT IN (
            'organizations',
            'census_income_cache',                        -- global reference
            'zz_backup_overnight_20260710_bins',          -- legacy backup
            'zz_backup_overnight_20260710_route_tasks',
            'zz_backup_overnight2_20260710_bins',
            'zz_backup_overnight2_20260710_route_tasks'
      );

    IF missing IS NOT NULL THEN
        RAISE EXCEPTION
            'Unclassified table(s) in public: %. Every table must be listed as '
            'tenant-scoped in Part 2 or allow-listed here as global reference. '
            'Refusing to leave a table outside the tenancy boundary.', missing;
    END IF;
END $$;

-- Assert every tenant table actually ended up protected.
DO $$
DECLARE
    bad TEXT[];
BEGIN
    SELECT array_agg(t.tbl ORDER BY t.tbl) INTO bad
    FROM _tenant_tables t
    JOIN pg_class c ON c.oid = ('public.' || quote_ident(t.tbl))::regclass
    WHERE NOT c.relrowsecurity
       OR NOT c.relforcerowsecurity
       OR NOT EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid)
       OR NOT EXISTS (
              SELECT 1 FROM pg_attribute a
              WHERE a.attrelid = c.oid AND a.attname = 'organization_id'
                AND a.attnotnull AND NOT a.attisdropped);

    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'Tenant table(s) not fully protected: %', bad;
    END IF;
END $$;


-- ============================================================================
-- POST-MIGRATION VERIFICATION (run manually, as postgres)
-- ============================================================================
--
-- 1. Coverage: expect 35 rows, all t/t/1.
--      SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
--             (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid) AS policies
--      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
--      WHERE n.nspname='public' AND c.relkind='r' AND c.relrowsecurity
--      ORDER BY 1;
--
-- 2. No orphan rows: expect 0.
--      SELECT count(*) FROM bins WHERE organization_id IS NULL;   -- and friends
--
-- 3. All FKs composite: expect ZERO rows other than the organizations FKs.
--      SELECT c.relname, con.conname, array_length(con.conkey,1)
--      FROM pg_constraint con
--      JOIN pg_class c ON c.oid = con.conrelid
--      JOIN pg_class pc ON pc.oid = con.confrelid
--      WHERE con.contype='f' AND array_length(con.conkey,1)=1
--        AND pc.relname <> 'organizations';
--
-- 4. Isolation smoke test — MUST be run as the non-superuser app role, because
--    as postgres it proves nothing:
--      SET ROLE ropacal_app;
--      SELECT set_config('app.org_id','00000000-0000-0000-0000-000000000001',false);
--      SELECT count(*) FROM bins;                      -- expect 134
--      SELECT set_config('app.org_id','does-not-exist',false);
--      SELECT count(*) FROM bins;                      -- expect 0
--      RESET app.org_id;
--      SELECT count(*) FROM bins;                      -- expect 0  (fail closed)
--      INSERT INTO bins (id, bin_number, current_street, city, zip, status)
--        VALUES ('t',1,'x','x','x','active');
--        -- expect: new row violates row-level security policy for table "bins"
--      RESET ROLE;
--
-- 5. Cross-tenant FK is now impossible — as the app role, with app.org_id set to
--    org A, inserting a child that points at an org B parent must raise
--    "violates foreign key constraint", not succeed. This one also holds when
--    RLS is bypassed entirely, which is the point of Part 7.
--
-- ---------------------------------------------------------------------------
--  This file was executed and verified end-to-end against a throwaway
--  PostgreSQL 17.9 instance loaded from a pg_dump --schema-only of production
--  (40 tables, 74 FKs, 1 view), with a non-superuser NOBYPASSRLS role and two
--  seeded tenants. Confirmed: 35/35 tenant tables end up NOT NULL + RLS +
--  FORCE + policy; 0 single-column FKs remain outside the organizations FKs;
--  71 composite FKs created; running the file a second time is a no-op;
--  cross-tenant SELECT/INSERT/UPDATE and cross-tenant FK references are all
--  rejected; ON DELETE SET NULL (col) preserves organization_id; ON DELETE
--  CASCADE still cascades; the same key in `config` is accepted for two
--  different orgs; and as `postgres` every one of those protections is bypassed
--  (which is Part 9's entire point).
-- ---------------------------------------------------------------------------
--
-- ============================================================================
--  PRE-EXISTING DEFECTS SURFACED WHILE WRITING THIS (not introduced here)
-- ============================================================================
--   * THREE foreign keys are declared ON DELETE SET NULL on a NOT NULL column,
--     which means deleting the referenced user raises a not-null violation today
--     — the delete simply cannot succeed:
--         bin_change_log.changed_by_user_id  -> users(id)
--         bin_move_requests.requested_by     -> users(id)
--         move_request_history.actor_id      -> users(id)
--     Part 7c recreates all three as NO ACTION and emits a NOTICE for each, which
--     turns a runtime surprise into an explicit "you cannot delete a user who has
--     history". Decide the real intent: either make the columns nullable (and
--     restore SET NULL), or keep NO ACTION and soft-delete users instead.
--   * bins.bin_number has no unique constraint and production has three duplicate
--     numbers (56, 86, 116). Anything keyed on bin_number instead of bin_id is
--     already ambiguous within one tenant and will be worse across tenants.
--   * Dead tables carrying live data: driver_location_history (94,647 rows, no
--     code path), driver_locations (6,244 rows, only in a commented-out block at
--     handlers/driver_location.go:582), shift_bins (0 rows, superseded by
--     route_tasks, and migrations/drop_shift_bins_table.sql exists but is not
--     wired into database.go so the table is recreated on every boot),
--     zone_risk_overrides (0 rows, DDL + model but zero queries).
--     All are scoped + RLS'd here so they cannot leak, but they should be dropped.
--   * bin_features (79 rows, last written 2026-07-26) and placement_decisions
--     (0 rows) have no writer in this repository. Identify that writer before
--     enabling RLS in production — it will need to set app.org_id too, or its
--     inserts will start failing the NOT NULL on organization_id.
