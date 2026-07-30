#!/usr/bin/env bash
# Create a Binly PLATFORM admin — an operator with cross-tenant access.
#
# READ THIS FIRST. This credential reaches EVERY tenant's data. It is not an
# admin of an organization; it is above all of them. Create as few as possible,
# and disable rather than forget the ones you stop using:
#
#   UPDATE platform_admins SET status='disabled' WHERE email='...';
#
# That takes effect on the operator's very next request — middleware.PlatformAuth
# re-checks the row every time, with no cache, precisely so revocation of
# cross-tenant access is immediate.
#
# Usage:
#   DATABASE_URL='postgres://...' ./scripts/provision-platform-admin.sh omar@binly.com "Omar Gabr"
#
# Prints the password ONCE and a TOTP enrollment URI. There is no recovery path
# for either: the password is stored bcrypt-hashed, and the TOTP secret is only
# shown here.
#
# The platform surface stays DISABLED until PLATFORM_JWT_SECRET is set on the
# backend — deliberately a separate secret from APP_JWT_SECRET, so a leak of the
# tenant signing key cannot mint platform tokens. Generate one with:
#   openssl rand -base64 48
set -euo pipefail

EMAIL="${1:?usage: provision-platform-admin.sh <email> <name>}"
NAME="${2:?missing name}"
: "${DATABASE_URL:?DATABASE_URL must be set}"

PSQL="${PSQL:-psql}"
command -v "$PSQL" >/dev/null || { echo "psql not found (set PSQL=/path/to/psql)"; exit 1; }

[[ "$EMAIL" == *"@"* ]] || { echo "email must be an email address: got '$EMAIL'"; exit 1; }

ADMIN_ID="$(uuidgen | tr 'A-Z' 'a-z')"
PASSWORD="$(openssl rand -base64 24 | tr -d '/+=' | cut -c1-24)"

# bcrypt + a base32 TOTP secret. python3 with bcrypt is the only dependency,
# matching provision-org.sh.
read -r HASH TOTP_SECRET <<<"$(python3 - "$PASSWORD" <<'PY'
import sys, base64, os, bcrypt
print(
    bcrypt.hashpw(sys.argv[1].encode(), bcrypt.gensalt()).decode(),
    base64.b32encode(os.urandom(20)).decode().rstrip("=")
)
PY
)"
[ -n "$HASH" ] && [ -n "$TOTP_SECRET" ] || { echo "failed to generate credentials (pip install bcrypt)"; exit 1; }

"$PSQL" "$DATABASE_URL" --single-transaction -v ON_ERROR_STOP=1 \
  -v id="$ADMIN_ID" -v email="$EMAIL" -v nm="$NAME" -v hash="$HASH" -v totp="$TOTP_SECRET" <<'SQL'
INSERT INTO platform_admins (id, email, password, name, totp_secret, status, created_at, updated_at)
VALUES (:'id', lower(:'email'), :'hash', :'nm', :'totp', 'active',
        EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT);
SQL

ISSUER="Binly%20Platform"
URI="otpauth://totp/${ISSUER}:${EMAIL}?secret=${TOTP_SECRET}&issuer=${ISSUER}&algorithm=SHA1&digits=6&period=30"

cat <<EOF

  Platform admin created.
    email      $EMAIL
    password   $PASSWORD        <-- shown ONCE
    admin_id   $ADMIN_ID

  Add this to an authenticator app (1Password, Authy, Google Authenticator):

    $URI

  Or enter the secret manually: $TOTP_SECRET

  Login requires ALL THREE — email, password, and a current 6-digit code:
    POST /api/platform/auth/login  {"email","password","totp_code"}

  The platform surface returns 404 until PLATFORM_JWT_SECRET is set on the
  backend. Until then these credentials do nothing.
EOF
