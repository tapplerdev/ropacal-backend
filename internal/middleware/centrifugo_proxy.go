package middleware

import (
	"crypto/subtle"
	"log"
	"net/http"
	"os"

	"ropacal-backend/internal/orgdb"
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
//   - secret set                  -> enforced (always).
//   - unset + tenancy DARK        -> pass through with a loud boot warning.
//     Single-tenant, so this is exactly the historical status quo: forged
//     payloads can spoof a driver, which they already could. Deploying the
//     guard shouldn't break realtime before ops has configured the header.
//   - unset + tenancy LIVE        -> DENY every proxy request.
//
// That last case is not caution, it is necessity. Once tenancy is live the
// proxies derive their tenant from the connection `meta` in the REQUEST BODY
// (see handlers.proxyOrgDB): org_id becomes an attacker-controlled parameter.
// RLS cannot help — it is handed the forged org rather than asked to validate
// it — so an unauthenticated caller could pick a victim tenant and publish
// GPS into its scope, or trip the proximity auto-end path against its shifts.
// Fail-open would therefore upgrade single-tenant spoofing into cross-tenant
// steering. Denying loudly (realtime down, obvious, self-healing the moment
// the header is configured) is the only acceptable failure here.
func CentrifugoProxyAuth() func(http.Handler) http.Handler {
	secret := os.Getenv("CENTRIFUGO_PROXY_SECRET")
	if secret == "" {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("⚠️  CENTRIFUGO_PROXY_SECRET is not set — Centrifugo proxy endpoints are UNPROTECTED")
		log.Println("   Anyone can POST forged subscribe/publish/GPS payloads to /api/centrifugo/*.")
		log.Printf("   Set the var and configure Centrifugo to send the %s header.", CentrifugoProxyHeader)
		if orgdb.Migrated() {
			log.Println("   🚨 TENANCY IS LIVE: proxy requests are being DENIED rather than trusted,")
			log.Println("      because an unauthenticated caller could otherwise name ANY tenant in")
			log.Println("      the connection meta. Realtime stays down until the secret is set.")
		}
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret == "" {
				// Mode is frozen at boot, but read it per-request so ordering
				// between InitTenancy and route registration can never matter.
				if orgdb.Migrated() {
					log.Printf("🚫 [Centrifugo] Proxy request DENIED: tenancy is live and %s is not configured",
						CentrifugoProxyHeader)
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r) // dark mode: historical behavior
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
