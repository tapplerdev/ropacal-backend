//go:build pgintegration

// Package pgtest connects the pgintegration tests to a THROWAWAY local
// Postgres, and refuses anything else: those tests write and delete.
//
// Two connections, like production: `app` is the non-superuser app role, so
// row-level security applies exactly as it does live; `admin` is a superuser
// for seeding fixtures and reading what RLS would hide. Testing tenancy as a
// superuser would prove nothing — superusers bypass RLS.
//
//	BINLY_TEST_DATABASE_URL='postgres://binly_app@127.0.0.1:54329/binly?sslmode=disable' \
//	BINLY_TEST_ADMIN_URL='postgres://postgres@127.0.0.1:54329/binly?sslmode=disable' \
//	go test -tags pgintegration ./internal/... -run Integration
package pgtest

import (
	"net/url"
	"os"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

// Connect returns the app-role and superuser connections, closed when the test
// ends. It skips when the URLs are unset and fails on a non-local host.
func Connect(t *testing.T) (app, admin *sqlx.DB) {
	t.Helper()
	appURL, adminURL := os.Getenv("BINLY_TEST_DATABASE_URL"), os.Getenv("BINLY_TEST_ADMIN_URL")
	if appURL == "" || adminURL == "" {
		t.Skip("BINLY_TEST_DATABASE_URL / BINLY_TEST_ADMIN_URL not set")
	}
	requireLocal(t, appURL)
	requireLocal(t, adminURL)
	app, admin = sqlx.MustConnect("postgres", appURL), sqlx.MustConnect("postgres", adminURL)
	t.Cleanup(func() { app.Close(); admin.Close() })
	return app, admin
}

// requireLocal checks the parsed host, not a substring of the URL — a remote
// host named "localhost.example.com", or "127.0.0.1" in a query string, must
// not pass. The URL itself is never printed: it can carry a password.
func requireLocal(t *testing.T, raw string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("unparseable test database URL")
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		t.Fatalf("refusing to run against non-local database host %q", u.Hostname())
	}
}
