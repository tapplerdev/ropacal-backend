#!/usr/bin/env bash
# Tear down a tenant: delete every row it owns, then the organization itself.
#
# WHY THIS MUST EXIST BEFORE YOU CREATE YOUR FIRST THROWAWAY ORG:
# all 35 FKs pointing at `organizations` are ON DELETE RESTRICT, so
# `DELETE FROM organizations` fails while ANY child row exists. There is no
# CASCADE to lean on, and the constraints are NOT deferrable (condeferrable =
# false on every one), so `SET CONSTRAINTS ALL DEFERRED` does not help either.
# Children must go first, in dependency order.
#
# THE ORDER BELOW IS GENERATED, NOT HAND-WRITTEN. It is a topological sort of the
# 35 tenant tables over BLOCKING FK edges only (confdeltype 'a'/'r'). The 40
# SET NULL and 27 CASCADE edges resolve themselves and MUST be excluded from the
# sort — including them produces a false cycle among
# bins / potential_locations / shifts / users, which is exactly what a
# hand-written order gets wrong. Regenerate with the query in
# TENANCY_BACKLOG.md if the schema changes.
#
# Usage:
#   DATABASE_URL='postgres://...' ./scripts/teardown-org.sh <slug>
#
# Safety:
#   * refuses to delete the last remaining organization
#   * refuses an org with >50 bins+checks+shifts unless FORCE=1
#   * requires typing the slug to confirm
#   * ONE transaction: the tenant is gone, or nothing changed
set -euo pipefail

SLUG="${1:?usage: teardown-org.sh <slug>}"
: "${DATABASE_URL:?DATABASE_URL must be set}"
PSQL="${PSQL:-psql}"
command -v "$PSQL" >/dev/null || { echo "psql not found (set PSQL=/path/to/psql)"; exit 1; }

# Tenant tables, in safe deletion order (children before parents).
TABLES=(
  ai_recommendations
  airtag_accounts
  airtag_keys
  airtag_locations
  app_error_logs
  bin_change_log
  bin_check_recommendations
  bin_features
  bin_move_requests
  bin_watchlist
  bins
  checks
  config
  driver_current_location
  driver_location_history
  driver_location_snapshots
  driver_locations
  fcm_tokens
  move_request_history
  moves
  no_go_zones
  notification_log
  placement_decisions
  route_bins
  route_tasks
  routes
  shift_bins
  shift_history
  shifts
  user_notification_preferences
  user_notifications
  zone_incidents
  zone_risk_overrides
  potential_locations
  users
)

ORG_ID="$("$PSQL" "$DATABASE_URL" -t -A -c \
  "SELECT id FROM organizations WHERE slug = lower('$SLUG')")"
[ -n "$ORG_ID" ] || { echo "no organization with slug '$SLUG'"; exit 1; }

TOTAL_ORGS="$("$PSQL" "$DATABASE_URL" -t -A -c "SELECT count(*) FROM organizations")"
if [ "$TOTAL_ORGS" -le 1 ]; then
  echo "refusing: '$SLUG' is the only organization. Removing it leaves no tenant,"
  echo "and the boot seeds are skipped once tenancy is live."
  exit 1
fi

echo "Organization '$SLUG'  ($ORG_ID)"
echo "Rows owned:"
for t in "${TABLES[@]}"; do
  n="$("$PSQL" "$DATABASE_URL" -t -A -c \
    "SELECT count(*) FROM $t WHERE organization_id = '$ORG_ID'")"
  [ "${n:-0}" -gt 0 ] && printf "    %-34s %s\n" "$t" "$n"
done

GRAND="$("$PSQL" "$DATABASE_URL" -t -A -c "
  SELECT (SELECT count(*) FROM bins   WHERE organization_id='$ORG_ID')
       + (SELECT count(*) FROM checks WHERE organization_id='$ORG_ID')
       + (SELECT count(*) FROM shifts WHERE organization_id='$ORG_ID')")"
if [ "${GRAND:-0}" -gt 50 ] && [ "${FORCE:-0}" != "1" ]; then
  echo
  echo "refusing: this org has $GRAND bins+checks+shifts — that is not a throwaway."
  echo "If you really mean it:  FORCE=1 $0 $SLUG"
  exit 1
fi

echo
printf "Type the slug to confirm IRREVERSIBLE deletion: "
read -r CONFIRM
[ "$CONFIRM" = "$SLUG" ] || { echo "aborted"; exit 1; }

# Build one statement list so it runs as a single transaction.
SQL="SELECT set_config('app.org_id', '$ORG_ID', true);"
for t in "${TABLES[@]}"; do
  SQL="$SQL DELETE FROM $t WHERE organization_id = '$ORG_ID';"
done
SQL="$SQL DELETE FROM organizations WHERE id = '$ORG_ID';"
SQL="$SQL DO \$\$ DECLARE n int; BEGIN
  SELECT count(*) INTO n FROM organizations WHERE id = '$ORG_ID';
  IF n <> 0 THEN RAISE EXCEPTION 'organization row survived deletion'; END IF;
END \$\$;"

"$PSQL" "$DATABASE_URL" --single-transaction -v ON_ERROR_STOP=1 -c "$SQL"

echo
echo "  '$SLUG' removed."
echo "  Now confirm your own tenant is untouched — log in and check the bin count."
