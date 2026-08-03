-- +goose Up
-- +goose StatementBegin

-- Narrow org_catalog_read so it cannot leak the tenant catalogue.
--
-- 00001 is a pg_dump baseline and still contains the original:
--
--     CREATE POLICY org_catalog_read ON public.organizations
--         FOR SELECT USING (true);
--
-- USING (true) means ANY authenticated tenant can SELECT EVERY row of
-- `organizations` — every other tenant's id, name and slug. It exists because
-- login has to resolve an org slug BEFORE a tenant context exists, so the
-- policy has to permit a read while `app.org_id` is unset. It just permits far
-- more than that.
--
-- PRODUCTION IS ALREADY FIXED. The narrowed policy was applied there by hand on
-- 2026-08-02, and re-verified live on 2026-08-03:
--
--   org_catalog_read | ((NULLIF(current_setting('app.org_id', true), '') IS NULL)
--                       OR (id = NULLIF(current_setting('app.org_id', true), '')))
--
-- The fix lived only in binly-backend's alembic/versions/0002_close_org_catalog_hole.py,
-- which is `stamp`ed against production rather than run — so it never reached
-- this migration chain. That is the gap this file closes.
--
-- WHY IT STILL MATTERS WITH PROD ALREADY FIXED: goose stamps 00001 on an
-- existing database instead of executing it, so prod never re-runs the wide
-- version. But any database built FROM these migrations — a new environment,
-- staging, the RDS restore, or a disaster-recovery rebuild — executes 00001 for
-- real and comes up with the hole open. Silently: the policy permits, so
-- nothing errors and nothing logs.
--
-- Rewriting 00001 in place would be worse — a historical migration whose
-- checksum no longer matches what already ran everywhere.
--
-- Idempotent by construction (DROP ... IF EXISTS then CREATE), so this is a
-- no-op against prod and a real fix everywhere else.

DROP POLICY IF EXISTS org_catalog_read ON public.organizations;

CREATE POLICY org_catalog_read ON public.organizations
    FOR SELECT USING (
        -- Unscoped read: permitted ONLY while no tenant is bound, which is the
        -- login path resolving a slug to an org. Every other caller has
        -- app.org_id set by middleware.Org before the first statement.
        NULLIF(current_setting('app.org_id', true), '') IS NULL
        -- Scoped read: a bound tenant sees its own row and nothing else.
        OR id = NULLIF(current_setting('app.org_id', true), '')
    );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Deliberately restores the WIDE policy, because that is what Down means here.
-- Rolling this back re-opens cross-tenant catalogue reads; do not run it on a
-- multi-tenant database.
DROP POLICY IF EXISTS org_catalog_read ON public.organizations;
CREATE POLICY org_catalog_read ON public.organizations FOR SELECT USING (true);

-- +goose StatementEnd
