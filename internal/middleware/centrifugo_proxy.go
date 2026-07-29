package middleware

import (
	"crypto/subtle"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"ropacal-backend/internal/orgdb"

	"github.com/jmoiron/sqlx"
)

// CentrifugoProxyHeader is the header Centrifugo must send on every proxy
// call (configured statically in its subscribe/publish proxy settings).
const CentrifugoProxyHeader = "X-Centrifugo-Proxy-Secret"

// CentrifugoProxyAuth guards the Centrifugo server-to-server proxy endpoints
// (/api/centrifugo/subscribe|publish|publish-location) with a shared static
// secret. These endpoints are called BY THE CENTRIFUGO SERVER, not by
// clients, so they carry no user JWT — the `user` field in their payload is
// the identity Centrifugo resolved from the client's authenticated connection
// token, and it is only trustworthy once this middleware has proven the
// caller actually is Centrifugo.
//
// The guard is MANDATORY once tenancy is live, and only transitionally
// fail-open before that:
//
//   - secret set                     -> enforced (always).
//   - unset + ONE organization       -> pass through with a loud boot warning.
//     A forged payload can only name the single existing org, so the exposure
//     is exactly the historical one (spoof a driver) and shipping the guard
//     must not break realtime before ops has configured Centrifugo's header.
//   - unset + MORE THAN ONE org      -> DENY every proxy request.
//
// That last case is not caution, it is necessity. The proxies derive their
// tenant from the connection `meta` in the REQUEST BODY (see
// handlers.proxyOrgDB): org_id is an attacker-controlled parameter, and RLS
// cannot help — it is handed the forged org rather than asked to validate it.
// With two or more tenants an unauthenticated caller could therefore pick a
// VICTIM org and publish GPS into its scope, or trip the proximity auto-end
// path against its shifts. Denying loudly (realtime down, obvious,
// self-healing the instant the header is configured) is the only acceptable
// failure once that is reachable.
//
// Same escalation rule as database.AssertRLSEnforced: one tenant is a warning,
// several is a refusal.
func CentrifugoProxyAuth(root *sqlx.DB) func(http.Handler) http.Handler {
	secret := os.Getenv("CENTRIFUGO_PROXY_SECRET")
	if secret == "" {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("⚠️  CENTRIFUGO_PROXY_SECRET is not set — Centrifugo proxy endpoints are UNPROTECTED")
		log.Println("   Anyone can POST forged subscribe/publish/GPS payloads to /api/centrifugo/*.")
		log.Printf("   Set the var and configure Centrifugo to send the %s header.", CentrifugoProxyHeader)
		log.Println("   Realtime will be DENIED outright as soon as a SECOND organization exists,")
		log.Println("   because org_id then becomes an attacker-selectable victim.")
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	}

	// Cached multi-tenant check. Provisioning a new org is rare and deliberate,
	// so a short TTL keeps steady-state at zero queries while still hardening
	// on its own within seconds of org #2 appearing.
	var (
		mu          sync.Mutex
		multiTenant bool
		checkedAt   time.Time
	)
	isMultiTenant := func() bool {
		if !orgdb.Migrated() {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		if time.Since(checkedAt) < 30*time.Second {
			return multiTenant
		}
		var n int
		if err := root.Get(&n, `SELECT count(*) FROM organizations`); err != nil {
			// Cannot prove single-tenant -> assume the dangerous case.
			log.Printf("⚠️  [Centrifugo] org count failed (%v) — treating as multi-tenant", err)
			multiTenant = true
		} else {
			multiTenant = n > 1
		}
		checkedAt = time.Now()
		return multiTenant
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret == "" {
				if isMultiTenant() {
					log.Printf("🚫 [Centrifugo] Proxy request DENIED: %s is not configured and this "+
						"database now hosts multiple organizations", CentrifugoProxyHeader)
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r) // single tenant: historical behavior
				return
			}
			provided := r.Header.Get(CentrifugoProxyHeader)
			// Constant-time compare: this is a long-lived static secret.
			if subtle.ConstantTimeCompare([]byte(provided), []byte(secret)) != 1 {
				// A non-200 is correct here: the caller is not Centrifugo,
				// so there is no proxy protocol to speak. Real-but-
				// misconfigured Centrifugo surfaces this as a proxy error,
				// which is the diagnosable outcome we want.
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
