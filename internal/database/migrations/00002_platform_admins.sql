-- Platform (cross-tenant) operator identities and their audit trail.
--
-- These tables are deliberately OUTSIDE the tenancy model:
--
--   * no organization_id — a platform admin belongs to no tenant
--   * no RLS            — they are not tenant data, and enabling RLS would
--                         require an app.org_id that a platform request has no
--                         value for
--
-- They could not live in `users`. That table carries FORCE ROW LEVEL SECURITY
-- with a policy keyed on organization_id, so a row with a NULL org would be
-- invisible to every query including its own login lookup.
--
-- Nothing here touches an existing table or policy.

-- +goose Up

CREATE TABLE IF NOT EXISTS platform_admins (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password      TEXT NOT NULL,              -- bcrypt
    name          TEXT NOT NULL,
    -- TOTP is mandatory, not optional. This credential reaches EVERY tenant's
    -- data, so there is deliberately no password-only path: the column is NOT
    -- NULL and login always demands a code.
    totp_secret   TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'disabled')),
    created_at    BIGINT NOT NULL,
    updated_at    BIGINT NOT NULL,
    last_login_at BIGINT
);

-- Every platform request writes one row here, from middleware rather than from
-- handlers — 45 handlers will not remember to log, and an audit trail with gaps
-- is worse than none because it invites false confidence.
CREATE TABLE IF NOT EXISTS platform_audit_log (
    id            BIGSERIAL PRIMARY KEY,
    admin_id      TEXT NOT NULL,
    admin_email   TEXT NOT NULL,
    -- The tenant this request acted on, when the request named one via
    -- X-Act-As-Org. NULL for platform-wide calls (login, whoami, aggregates).
    -- Deliberately NOT a foreign key: the audit trail must survive the deletion
    -- of the organization it refers to.
    target_org_id TEXT,
    method        TEXT NOT NULL,
    path          TEXT NOT NULL,
    status_code   INTEGER,
    at            BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_platform_audit_at ON platform_audit_log (at DESC);
CREATE INDEX IF NOT EXISTS idx_platform_audit_admin ON platform_audit_log (admin_id, at DESC);
CREATE INDEX IF NOT EXISTS idx_platform_audit_org ON platform_audit_log (target_org_id, at DESC);

-- +goose Down
-- platform_audit_log is deliberately NOT dropped. A routine deploy rollback must
-- not erase the record of who reached which tenant's data — that is the one
-- artifact you cannot reconstruct afterwards. target_org_id is already not a
-- foreign key so the trail survives organization deletion; dropping the whole
-- table on rollback would have been the inconsistent half of that decision.
-- Drop it by hand if you genuinely mean to.
DROP TABLE IF EXISTS platform_admins;
