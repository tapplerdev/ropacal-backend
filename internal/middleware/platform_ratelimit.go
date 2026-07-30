package middleware

import (
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// Rate limiting for platform login.
//
// The Phase 1 review measured 50 attempts/second against this endpoint with no
// throttle of any kind. Two consequences, both bad:
//
//   - TOTP becomes brute-forceable. totp.Validate accepts 3 of 10^6 codes at any
//     instant (Skew=1), so once a password is known the expected work is ~333k
//     attempts — roughly 1.8 hours at the observed rate. A second factor that
//     can be guessed in an afternoon is not a second factor.
//   - It is a cheap CPU-exhaustion vector: every attempt burns a cost-12 bcrypt
//     (~190ms) in the same process that serves the tenant API, so hammering
//     platform login degrades every customer.
//
// Deliberately in-process rather than Redis-backed. Redis is optional in this
// deployment (the client is nil when unreachable and the app degrades happily),
// and a rate limiter that silently stops limiting when a dependency is down is
// worse than none. One process owns the pool here, so in-process is accurate.

const (
	// Per-IP. Tightened from 5 to 3 when TOTP became optional: with
	// password-only accounts this limiter is the ONLY thing between a guessed
	// password and every tenant's data.
	platformLoginPerIPLimit  = 3
	platformLoginPerIPWindow = 15 * time.Minute

	// Global: a botnet spreads across many IPs, so the per-IP bucket alone is
	// not a real ceiling. This one is deliberately low — the legitimate load on
	// this endpoint is a handful of Binly staff logging in.
	platformLoginGlobalLimit  = 20
	platformLoginGlobalWindow = 15 * time.Minute
)

type loginAttempts struct {
	mu     sync.Mutex
	perIP  map[string][]time.Time
	global []time.Time
}

var platformLogins = &loginAttempts{perIP: make(map[string][]time.Time)}

// allow reports whether an attempt from ip may proceed, and records it.
func (a *loginAttempts) allow(ip string, now time.Time) (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	prune := func(ts []time.Time, window time.Duration) []time.Time {
		cutoff := now.Add(-window)
		kept := ts[:0]
		for _, t := range ts {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		return kept
	}

	a.global = prune(a.global, platformLoginGlobalWindow)
	if len(a.global) >= platformLoginGlobalLimit {
		return false, "global"
	}

	ipAttempts := prune(a.perIP[ip], platformLoginPerIPWindow)
	if len(ipAttempts) >= platformLoginPerIPLimit {
		a.perIP[ip] = ipAttempts
		return false, "per-ip"
	}

	a.perIP[ip] = append(ipAttempts, now)
	a.global = append(a.global, now)

	// Bound memory: a flood from many IPs must not grow this map without limit.
	if len(a.perIP) > 10000 {
		for k, v := range a.perIP {
			if len(prune(v, platformLoginPerIPWindow)) == 0 {
				delete(a.perIP, k)
			}
		}
	}
	return true, ""
}

// PlatformLoginRateLimit throttles platform authentication attempts.
//
// Counts EVERY attempt, not just failures. Counting only failures lets an
// attacker with one valid credential interleave successes to reset the bucket,
// and the legitimate volume here is low enough that it costs nothing.
func PlatformLoginRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if ok, scope := platformLogins.allow(ip, time.Now()); !ok {
			log.Printf("🚫 [PlatformLogin] rate limited (%s) from %s", scope, ip)
			w.Header().Set("Retry-After", "900")
			http.Error(w, "Too many attempts", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP prefers the proxy-supplied original address. Railway terminates TLS
// and proxies, so RemoteAddr is the edge, not the caller.
//
// X-Forwarded-For is client-controllable, so a determined attacker can rotate it
// and defeat the per-IP bucket — which is exactly why the global limit above
// exists and is not derived from the header.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return trimSpace(xff[:i])
			}
		}
		return trimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// AllowPlatformLoginAttempt records and rate-limits one platform authentication
// attempt, for callers outside this package.
//
// Exists because platform admins now sign in through the ORDINARY
// /api/auth/login route, which lives in the handlers package. That route must
// share this bucket rather than have its own — otherwise the limit is trivially
// bypassed by using whichever entry point is not throttled.
//
// Returns false and the scope that tripped ("per-ip" or "global") when the
// attempt must be refused.
func AllowPlatformLoginAttempt(r *http.Request) (bool, string) {
	return platformLogins.allow(clientIP(r), time.Now())
}
