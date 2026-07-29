package middleware

import (
	"crypto/subtle"
	"log"
	"net/http"
	"os"
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
// TRANSITIONAL fail-open: if CENTRIFUGO_PROXY_SECRET is unset the guard
// passes everything through, with one loud boot warning. Ops must first add
// the static header to Centrifugo's proxy config (Railway) and set the env
// var here; breaking realtime on deploy day is worse than the status quo for
// one release. A follow-up flips this guard to mandatory.
func CentrifugoProxyAuth() func(http.Handler) http.Handler {
	secret := os.Getenv("CENTRIFUGO_PROXY_SECRET")
	if secret == "" {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("⚠️  CENTRIFUGO_PROXY_SECRET is not set — Centrifugo proxy endpoints are UNPROTECTED")
		log.Println("   Anyone can POST forged subscribe/publish/GPS payloads to /api/centrifugo/*.")
		log.Printf("   Set the var and configure Centrifugo to send the %s header.", CentrifugoProxyHeader)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret != "" {
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
			}
			next.ServeHTTP(w, r)
		})
	}
}
