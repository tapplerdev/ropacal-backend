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

NAME="${1:?usage: provision-org.sh <name> <slug> <admin-email> [warehouse-address] [lat] [lon]}"
SLUG="${2:?missing slug}"
EMAIL="${3:?missing admin email}"
# Optional warehouse. Supply it here whenever you know it — the dashboard has NO
# UI for setting a warehouse (lib/hooks/use-warehouse.ts exports
# useUpdateWarehouseLocation but nothing consumes it), so a tenant provisioned
# without one has no self-service way to fix it and every route optimization
# returns 412 until someone PATCHes the API by hand.
WH_ADDRESS="${4:-CHANGE ME}"
WH_LAT="${5:-0}"
WH_LON="${6:-0}"
: "${DATABASE_URL:?DATABASE_URL must be set}"

PSQL="${PSQL:-psql}"
command -v "$PSQL" >/dev/null || { echo "psql not found (set PSQL=/path/to/psql)"; exit 1; }

# Slug is used in login requests and as a Centrifugo/URL-safe identifier.
[[ "$SLUG" =~ ^[a-z0-9][a-z0-9-]{1,30}$ ]] || {
  echo "slug must be lowercase alphanumeric/hyphen, 2-31 chars: got '$SLUG'"; exit 1; }

# Reject a half-supplied or nonsense warehouse rather than silently storing it:
# a bad coordinate routes drivers to the wrong continent, which is worse than the
# 412 that an unset (0,0) warehouse produces.
if [ "$WH_LAT" != "0" ] || [ "$WH_LON" != "0" ]; then
  [[ "$WH_LAT" =~ ^-?[0-9]+(\.[0-9]+)?$ && "$WH_LON" =~ ^-?[0-9]+(\.[0-9]+)?$ ]] || {
    echo "warehouse lat/lon must be numeric: got '$WH_LAT','$WH_LON'"; exit 1; }
  awk -v la="$WH_LAT" -v lo="$WH_LON" 'BEGIN{exit !(la>=-90 && la<=90 && lo>=-180 && lo<=180)}' || {
    echo "warehouse lat/lon out of range: $WH_LAT,$WH_LON"; exit 1; }
  [ "$WH_ADDRESS" != "CHANGE ME" ] || {
    echo "supply a warehouse address alongside coordinates"; exit 1; }
fi

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
  -v email="$EMAIL" -v hash="$HASH" \
  -v wh_address="$WH_ADDRESS" -v wh_lat="$WH_LAT" -v wh_lon="$WH_LON" <<'SQL'
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
        jsonb_build_object('address', :'wh_address',
                           'latitude', (:'wh_lat')::numeric,
                           'longitude', (:'wh_lon')::numeric),
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

    warehouse      $WH_ADDRESS ($WH_LAT, $WH_LON)

  NEXT, before this tenant is usable:
    1. $([ "$WH_LAT" = "0" ] && [ "$WH_LON" = "0" ] \
         && echo "Set a real warehouse location — it is 0,0, so route optimization
       returns 412 until it is set. There is NO dashboard UI for this, so it must
       be done by hand:  PATCH /api/config/warehouse  as this org's admin" \
         || echo "Warehouse is set — nothing to do. (Re-run the PATCH if it moves;
       there is no dashboard UI for it.)")
    2. Login needs no organization ID: with 2+ orgs the server resolves it
       from the email (verified in prod 2026-07-30). Only an email that exists
       in MORE THAN ONE org is asked for the ID.
    3. Verify isolation BOTH ways before trusting it:
         log in as $EMAIL   -> must see 0 bins, 0 shifts
         log in as your own -> must see your fleet, unchanged

  To remove this tenant:  ./scripts/teardown-org.sh $SLUG
EOF
