#!/usr/bin/env bash
# Provision a new tenant: organization + first admin user + warehouse location.
#
# ALL THREE INSERTS RUN IN ONE TRANSACTION. That is the whole point: every FK
# pointing at `organizations` is ON DELETE RESTRICT (35 of them), so a
# half-provisioned org — org row created, admin not — cannot simply be deleted.
# It would sit there permanently AND change live behaviour (login starts
# demanding an org slug; the AirTag monitor starts sweeping across tenants).
# One transaction means any failure leaves nothing behind.
#
# Usage:
#   DATABASE_URL='postgres://...' ./scripts/provision-org.sh "Acme Hauling" acme admin@acme.com
#
# Notes:
#   * Runs as whatever DATABASE_URL says. binly_app is sufficient — it already
#     holds INSERT on organizations, and organization_id column DEFAULTs pick up
#     app.org_id, which this script sets explicitly anyway.
#   * The generated password is printed ONCE. There is no recovery path; the
#     column stores a bcrypt hash.
#   * BEFORE running this: ship D5 (AirTag tenancy) and confirm Centrifugo's
#     proxy header is configured. A second org changes system behaviour the
#     moment it exists — see TIER1_PLAN.md §0.10-0.12.
set -euo pipefail

NAME="${1:?usage: provision-org.sh <name> <slug> <admin-email>}"
SLUG="${2:?missing slug}"
EMAIL="${3:?missing admin email}"
: "${DATABASE_URL:?DATABASE_URL must be set}"

PSQL="${PSQL:-psql}"
command -v "$PSQL" >/dev/null || { echo "psql not found (set PSQL=/path/to/psql)"; exit 1; }

# Slug is used in login requests and as a Centrifugo/URL-safe identifier.
[[ "$SLUG" =~ ^[a-z0-9][a-z0-9-]{1,30}$ ]] || {
  echo "slug must be lowercase alphanumeric/hyphen, 2-31 chars: got '$SLUG'"; exit 1; }

ORG_ID="$(uuidgen | tr 'A-Z' 'a-z')"
USER_ID="$(uuidgen | tr 'A-Z' 'a-z')"
PASSWORD="$(openssl rand -base64 18 | tr -d '/+=' | cut -c1-20)"

# bcrypt the password. python3 + bcrypt is the only external dependency.
HASH="$(python3 - "$PASSWORD" <<'PY'
import sys, bcrypt
print(bcrypt.hashpw(sys.argv[1].encode(), bcrypt.gensalt()).decode())
PY
)"
[ -n "$HASH" ] || { echo "failed to generate password hash (pip install bcrypt)"; exit 1; }

echo "Provisioning '$NAME' (slug: $SLUG)  org_id=$ORG_ID"

# ONE transaction. ON_ERROR_STOP + --single-transaction => all or nothing.
"$PSQL" "$DATABASE_URL" --single-transaction -v ON_ERROR_STOP=1 \
  -v org="$ORG_ID" -v uid="$USER_ID" -v nm="$NAME" -v slug="$SLUG" \
  -v email="$EMAIL" -v hash="$HASH" <<'SQL'
-- Scope the session so organization_id column DEFAULTs resolve, and so the
-- organizations RLS WITH CHECK (id = current_setting('app.org_id')) is satisfied
-- when running as the non-superuser app role.
SELECT set_config('app.org_id', :'org', true);

INSERT INTO organizations (id, name, slug, status)
VALUES (:'org', :'nm', lower(:'slug'), 'active');

INSERT INTO users (id, email, password, name, role, organization_id, created_at, updated_at)
VALUES (:'uid', :'email', :'hash', 'Admin', 'admin', :'org',
        EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT);

-- config.id is a SERIAL INTEGER, not a uuid. Omit it. (Passing a uuid here is a
-- real mistake that has been made — it fails with "invalid input syntax for
-- type integer".)
INSERT INTO config (key, value, organization_id)
VALUES ('warehouse_location',
        '{"address":"CHANGE ME","latitude":0,"longitude":0}'::jsonb,
        :'org');

-- Fail loudly rather than leaving a tenant that cannot log in or route.
DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM users WHERE organization_id = current_setting('app.org_id');
  IF n <> 1 THEN RAISE EXCEPTION 'expected exactly 1 admin, found %', n; END IF;
  SELECT count(*) INTO n FROM config
   WHERE organization_id = current_setting('app.org_id') AND key = 'warehouse_location';
  IF n <> 1 THEN RAISE EXCEPTION 'warehouse_location row missing'; END IF;
END $$;
SQL

cat <<EOF

  Provisioned.
    organization   $NAME  (slug: $SLUG)
    org_id         $ORG_ID
    admin email    $EMAIL
    admin password $PASSWORD      <-- shown ONCE, store it now

  NEXT, before this tenant is usable:
    1. Set a real warehouse location (currently 0,0 — route optimization returns
       412 until it is set):
         PATCH /api/config/warehouse   as this org's admin
    2. Clients must send "organization":"$SLUG" on login. With more than one org
       the single-org grace is gone and omitting it returns 400.
    3. Verify isolation BOTH ways before trusting it:
         log in as $EMAIL   -> must see 0 bins, 0 shifts
         log in as your own -> must see your fleet, unchanged

  To remove this tenant:  ./scripts/teardown-org.sh $SLUG
EOF
