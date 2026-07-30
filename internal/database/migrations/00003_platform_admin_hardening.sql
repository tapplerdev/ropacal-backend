-- Hardening from the Phase 1 security review. Three schema-level gaps.
--
-- +goose Up

-- 1. TOTP REPLAY. totp.Validate accepts a code across three counter windows
--    (Skew=1, Period=30), and nothing recorded which counters had already been
--    used — so an observed code was replayable for up to 90 seconds. Verified in
--    review: the same 6-digit code authenticated three times in a row.
--    Login now refuses any counter <= the last one accepted.
ALTER TABLE platform_admins
    ADD COLUMN IF NOT EXISTS last_totp_counter BIGINT NOT NULL DEFAULT 0;

-- 2. CASE-SENSITIVE UNIQUENESS vs CASE-INSENSITIVE LOOKUP. The table had
--    UNIQUE (email) while login matches on LOWER(email), so 'op@binly.com' and
--    'OP@binly.com' could both exist and the lookup would match both — with
--    sqlx.Get taking whichever row Postgres happened to return first. Which
--    operator authenticates then depends on physical row order.
ALTER TABLE platform_admins DROP CONSTRAINT IF EXISTS platform_admins_email_key;
CREATE UNIQUE INDEX IF NOT EXISTS uq_platform_admins_email_lower
    ON platform_admins (LOWER(email));

-- 3. THE CREDENTIAL STORE WAS READABLE FROM A TENANT SESSION. platform_admins
--    was the only table in the database with no RLS, and it holds bcrypt hashes
--    and TOTP secrets for identities that reach EVERY tenant. Review confirmed a
--    tenant-scoped session could SELECT every row.
--
--    It still cannot use the normal org policy — these rows belong to no
--    organization. Instead the policy inverts the test: readable ONLY when
--    app.org_id is unset, which is true for the platform path (raw pool, no
--    GUC) and false for every tenant request (orgdb sets the GUC on every
--    statement). FORCE is required or the owning role would bypass it.
--
--    Defense in depth: it does not make an authenticated platform admin any less
--    powerful. It means a SQL injection in one of the 45 tenant handlers can no
--    longer read the platform credential store.
ALTER TABLE platform_admins ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform_admins FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS platform_admins_no_tenant_access ON platform_admins;
CREATE POLICY platform_admins_no_tenant_access ON platform_admins
    FOR ALL TO PUBLIC
    USING (COALESCE(NULLIF(current_setting('app.org_id', true), ''), '') = '')
    WITH CHECK (COALESCE(NULLIF(current_setting('app.org_id', true), ''), '') = '');

-- Same treatment for the audit trail: a tenant request has no business reading
-- which tenants an operator has been looking at.
ALTER TABLE platform_audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform_audit_log FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS platform_audit_no_tenant_access ON platform_audit_log;
CREATE POLICY platform_audit_no_tenant_access ON platform_audit_log
    FOR ALL TO PUBLIC
    USING (COALESCE(NULLIF(current_setting('app.org_id', true), ''), '') = '')
    WITH CHECK (COALESCE(NULLIF(current_setting('app.org_id', true), ''), '') = '');

-- +goose Down
-- Deliberately does NOT drop platform_audit_log. 00002's Down did, which meant a
-- routine deploy rollback erased the entire record of cross-tenant access — the
-- inconsistent half of a design that otherwise goes out of its way to keep the
-- trail alive (target_org_id is not a foreign key precisely so it survives
-- organization deletion). Rolling back should relax the hardening, not destroy
-- evidence.
DROP POLICY IF EXISTS platform_audit_no_tenant_access ON platform_audit_log;
DROP POLICY IF EXISTS platform_admins_no_tenant_access ON platform_admins;
ALTER TABLE platform_audit_log NO FORCE ROW LEVEL SECURITY;
ALTER TABLE platform_audit_log DISABLE ROW LEVEL SECURITY;
ALTER TABLE platform_admins NO FORCE ROW LEVEL SECURITY;
ALTER TABLE platform_admins DISABLE ROW LEVEL SECURITY;
