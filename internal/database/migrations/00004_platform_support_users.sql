-- Per-organization support identity, so a platform operator's WRITES can be
-- attributed to a real user row.
--
-- WHY THIS IS NEEDED. `users` is referenced by 28 foreign keys and read by 115
-- `userClaims.UserID` sites. A platform operator is not a tenant user, so those
-- writes had nothing valid to record — the previous phase blocked writes entirely
-- rather than fabricate an id (which would violate the FKs) or borrow a real
-- tenant admin (which would corrupt that tenant's own history by attributing
-- Binly's actions to their staff).
--
-- A dedicated row per organization solves both: the FKs are satisfied, and the
-- tenant's audit trail honestly reads "Binly Support" — distinguishable from
-- their own people. Which HUMAN operator it was lives in platform_audit_log.
--
-- +goose Up

CREATE TABLE IF NOT EXISTS platform_support_users (
    organization_id TEXT PRIMARY KEY,
    -- The users.id inside that organization. Deliberately NOT a foreign key:
    -- `users` is under FORCE RLS and this table is read on the raw pool with no
    -- app.org_id, so an FK check here would be evaluated in a context that
    -- cannot see the row. The mapping is re-derived from `users` if it ever
    -- disagrees (see ensureSupportUser).
    user_id         TEXT NOT NULL,
    created_at      BIGINT NOT NULL
);

-- Same inverted policy as the other platform tables: readable ONLY when
-- app.org_id is unset, which is true for the platform path and false for every
-- tenant request. A tenant has no business enumerating which of them Binly can
-- act on.
ALTER TABLE platform_support_users ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform_support_users FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS platform_support_no_tenant_access ON platform_support_users;
CREATE POLICY platform_support_no_tenant_access ON platform_support_users
    FOR ALL TO PUBLIC
    USING (COALESCE(NULLIF(current_setting('app.org_id', true), ''), '') = '')
    WITH CHECK (COALESCE(NULLIF(current_setting('app.org_id', true), ''), '') = '');

-- TOTP becomes OPTIONAL rather than mandatory: enabled per account by setting a
-- secret, required whenever one is present. The owner chose password-only for
-- now; this keeps re-enabling it a single UPDATE rather than a migration, and
-- leaves the working replay-protection code in place.
ALTER TABLE platform_admins ALTER COLUMN totp_secret DROP NOT NULL;

-- Per-account write permission. Cheap to add now, awkward later: it lets some
-- staff hold read-only cross-tenant access without a schema change.
ALTER TABLE platform_admins
    ADD COLUMN IF NOT EXISTS can_write BOOLEAN NOT NULL DEFAULT TRUE;

-- +goose Down
DROP TABLE IF EXISTS platform_support_users;
ALTER TABLE platform_admins DROP COLUMN IF EXISTS can_write;
-- totp_secret is deliberately left nullable: re-imposing NOT NULL would fail on
-- any password-only admin created since.
