-- Scope airtag_accounts.email uniqueness to the organization.
--
-- WHY: `airtag_accounts_email_key UNIQUE (email)` is GLOBAL. Under RLS a tenant
-- cannot SEE another tenant's rows, but a unique index is enforced across all of
-- them — so adding an Apple ID already registered by a different organization
-- returns 23505 (unique violation) on a row RLS says does not exist. That is a
-- cross-tenant existence oracle: it confirms another tenant holds a given Apple
-- ID. It also blocks a legitimate case, since two haulers can genuinely use the
-- same Apple ID for their own FindMy accounts.
--
-- Safe to run: 3 rows, 3 distinct emails (verified 2026-07-30), so no collision
-- can surface when the constraint is relaxed.
--
-- Idempotent. Run as postgres (index DDL on a binly_app-owned table is fine
-- either way; ownership is preserved).
BEGIN;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'airtag_accounts_email_key') THEN
        ALTER TABLE airtag_accounts DROP CONSTRAINT IF EXISTS airtag_accounts_email_key;
        -- older schemas may have it as a bare index rather than a constraint
        DROP INDEX IF EXISTS airtag_accounts_email_key;
        RAISE NOTICE 'dropped global airtag_accounts_email_key';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'uq_airtag_accounts_org_email') THEN
        ALTER TABLE airtag_accounts
            ADD CONSTRAINT uq_airtag_accounts_org_email UNIQUE (organization_id, email);
        RAISE NOTICE 'added uq_airtag_accounts_org_email (organization_id, email)';
    END IF;
END $$;

COMMIT;
