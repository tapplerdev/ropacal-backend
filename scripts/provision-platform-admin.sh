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
# The password and the TOTP secret are written to SEPARATE mode-600 files rather
# than printed. Putting both factors in one terminal scrollback, CI log or tmux
# buffer defeats the point of having two.
#
# The platform surface stays DISABLED until PLATFORM_JWT_SECRET is set on the
# backend — a separate secret from APP_JWT_SECRET, and the server refuses to boot
# if they match. Generate one with:
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

# rounds=10 pins this to bcrypt.DefaultCost on the Go side. Python's gensalt()
# defaults to 12, and the login handler's timing-equalisation placeholder is
# generated at DefaultCost — a mismatch makes a missing admin measurably faster
# than a real one (measured 50ms vs 195ms), which is an oracle for which Binly
# staff exist.
#
# The TOTP secret is 160 bits (os.urandom(20)), which is RFC 6238's
# recommendation for SHA-1.
CREDS="$(PASSWORD="$PASSWORD" python3 -c '
import os, base64, bcrypt
print(
    bcrypt.hashpw(os.environ["PASSWORD"].encode(), bcrypt.gensalt(rounds=10)).decode(),
    base64.b32encode(os.urandom(20)).decode().rstrip("="),
)')"
HASH="${CREDS%% *}"
TOTP_SECRET="${CREDS##* }"
[ -n "$HASH" ] && [ -n "$TOTP_SECRET" ] || { echo "failed to generate credentials (pip install bcrypt)"; exit 1; }

# Secrets go over STDIN, not through `psql -v`. -v puts its values in the
# process argument list, where any other user on the host can read a bcrypt hash
# and a complete second factor with `ps`. A heredoc is piped on stdin and never
# appears there.
#
# (An earlier attempt used current_setting('BINLY_HASH') with the values in the
# environment — that does not work: Postgres only resolves custom GUCs, not
# arbitrary environment variables.)
#
# Values are single-quote-escaped because NAME and EMAIL come from the caller.
sqlq() { printf "%s" "$1" | sed "s/'/''/g"; }

"$PSQL" "$DATABASE_URL" --single-transaction -v ON_ERROR_STOP=1 <<SQL
INSERT INTO platform_admins (id, email, password, name, totp_secret, status, created_at, updated_at)
VALUES ('$(sqlq "$ADMIN_ID")',
        lower('$(sqlq "$EMAIL")'),
        '$(sqlq "$HASH")',
        '$(sqlq "$NAME")',
        '$(sqlq "$TOTP_SECRET")',
        'active',
        EXTRACT(EPOCH FROM NOW())::BIGINT,
        EXTRACT(EPOCH FROM NOW())::BIGINT);
SQL

ISSUER="Binly%20Platform"
URI="otpauth://totp/${ISSUER}:${EMAIL}?secret=${TOTP_SECRET}&issuer=${ISSUER}&algorithm=SHA1&digits=6&period=30"

PW_FILE="$(mktemp -t binly-platform-pw)"
TOTP_FILE="$(mktemp -t binly-platform-totp)"
chmod 600 "$PW_FILE" "$TOTP_FILE"
printf '%s\n' "$PASSWORD" > "$PW_FILE"
printf 'secret: %s\nuri:    %s\n' "$TOTP_SECRET" "$URI" > "$TOTP_FILE"

cat <<EOF

  Platform admin created.
    email      $EMAIL
    admin_id   $ADMIN_ID

  The two factors are in SEPARATE files (mode 600), not printed here — one
  captured terminal must not yield both:

    password    $PW_FILE
    totp / URI  $TOTP_FILE

  Move them into a password manager, then delete both files.

  Login requires ALL THREE — email, password, and a current 6-digit code:
    POST /api/platform/auth/login  {"email","password","totp_code"}

  Codes are single-use: a replayed code is refused even inside its 30s window.

  The platform surface returns 404 until PLATFORM_JWT_SECRET is set on the
  backend, and the server refuses to boot if it equals APP_JWT_SECRET.
EOF
