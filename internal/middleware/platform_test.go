package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"time"
)

// The mutual-exclusion and secret-separation rules are the whole security model
// of the platform surface. They were verified once by hand during Phase 1; these
// tests exist so they are verified on every change instead.

func TestAssertPlatformSecretDistinct(t *testing.T) {
	cases := []struct {
		name       string
		platform   string
		app        string
		wantErr    bool
		wantReason string
	}{
		{"disabled surface is fine", "", "tenant-secret-value-long-enough-x", false, ""},
		{"distinct and long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", false, ""},
		// The case the review demonstrated: identical secrets let anyone who can
		// mint a tenant token forge a platform token.
		{"identical secrets rejected", "same-secret-value-that-is-long-enough", "same-secret-value-that-is-long-enough", true, "identical"},
		{"too short rejected", "short", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", true, "short"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PLATFORM_JWT_SECRET", c.platform)
			t.Setenv("APP_JWT_SECRET", c.app)
			err := AssertPlatformSecretDistinct()
			if c.wantErr && err == nil {
				t.Fatalf("expected an error (%s), got nil", c.wantReason)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

// A platform token must never be usable where a tenant token belongs. Two
// existing guards already reject one as a side effect (no org_id, no users row);
// this asserts the EXPLICIT check, so the rule survives someone relaxing either.
func TestTenantAuthRejectsPlatformToken(t *testing.T) {
	secret := "tenant-secret-value-long-enough-here"
	t.Setenv("APP_JWT_SECRET", secret)

	tok := signHS256(t, secret, map[string]interface{}{
		"platform": true,
		"admin_id": "abc",
		// Deliberately ALSO carries a full set of tenant claims: a hybrid token
		// is the interesting attack, not a bare platform one.
		"user_id": "u1", "email": "x@y.z", "role": "admin",
		"org_id": "00000000-0000-0000-0000-000000000001",
		"exp":    time.Now().Add(time.Hour).Unix(),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/bins", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	reached := false
	Auth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })).ServeHTTP(rec, req)

	if reached {
		t.Fatal("a platform token reached a tenant handler")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestParsePlatformTokenRequiresExpiry(t *testing.T) {
	secret := "platform-secret-value-long-enough-x"

	// No exp at all: previously accepted forever.
	noExp := signHS256(t, secret, map[string]interface{}{"platform": true, "admin_id": "a1"})
	if _, err := parsePlatformToken(bearer(noExp), secret); err == nil {
		t.Fatal("a token with no exp was accepted")
	}

	expired := signHS256(t, secret, map[string]interface{}{
		"platform": true, "admin_id": "a1",
		"exp": time.Now().Add(-time.Minute).Unix(),
	})
	if _, err := parsePlatformToken(bearer(expired), secret); err == nil {
		t.Fatal("an expired token was accepted")
	}

	good := signHS256(t, secret, map[string]interface{}{
		"platform": true, "admin_id": "a1",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	c, err := parsePlatformToken(bearer(good), secret)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if c.AdminID != "a1" {
		t.Fatalf("admin_id = %q", c.AdminID)
	}
}

func TestParsePlatformTokenRejectsNonPlatform(t *testing.T) {
	secret := "platform-secret-value-long-enough-x"
	for _, claims := range []map[string]interface{}{
		{"admin_id": "a1", "exp": time.Now().Add(time.Hour).Unix()},                     // no platform claim
		{"platform": "true", "admin_id": "a1", "exp": time.Now().Add(time.Hour).Unix()}, // string, not bool
		{"platform": 1, "admin_id": "a1", "exp": time.Now().Add(time.Hour).Unix()},      // number
		{"platform": true, "exp": time.Now().Add(time.Hour).Unix()},                     // no admin_id
		{"platform": true, "admin_id": 42, "exp": time.Now().Add(time.Hour).Unix()},     // admin_id wrong type
		{"platform": true, "admin_id": "", "exp": time.Now().Add(time.Hour).Unix()},     // empty admin_id
	} {
		if _, err := parsePlatformToken(bearer(signHS256(t, secret, claims)), secret); err == nil {
			t.Errorf("accepted a token it should not have: %v", claims)
		}
	}
}

func TestParsePlatformTokenRejectsWrongSecret(t *testing.T) {
	good := signHS256(t, "platform-secret-value-long-enough-x", map[string]interface{}{
		"platform": true, "admin_id": "a1", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := parsePlatformToken(bearer(good), "a-completely-different-secret-value"); err == nil {
		t.Fatal("a token signed with another key was accepted")
	}
}

// The rate limiter is the only thing standing between a 6-digit second factor
// and an offline-speed search.
func TestPlatformLoginRateLimit(t *testing.T) {
	platformLogins.mu.Lock()
	platformLogins.perIP = map[string][]time.Time{}
	platformLogins.global = nil
	platformLogins.mu.Unlock()

	now := time.Now()
	for i := 0; i < platformLoginPerIPLimit; i++ {
		if ok, _ := platformLogins.allow("1.2.3.4", now); !ok {
			t.Fatalf("attempt %d was blocked before the limit", i+1)
		}
	}
	if ok, scope := platformLogins.allow("1.2.3.4", now); ok || scope != "per-ip" {
		t.Fatalf("attempt past the per-IP limit was allowed (scope=%q)", scope)
	}
	// A different IP is independent, until the global ceiling.
	if ok, _ := platformLogins.allow("5.6.7.8", now); !ok {
		t.Fatal("a different IP was blocked by another IP's bucket")
	}
	// The window expires.
	if ok, _ := platformLogins.allow("1.2.3.4", now.Add(platformLoginPerIPWindow+time.Second)); !ok {
		t.Fatal("the per-IP window never expired")
	}
}

func TestPlatformLoginRateLimitGlobalCeiling(t *testing.T) {
	platformLogins.mu.Lock()
	platformLogins.perIP = map[string][]time.Time{}
	platformLogins.global = nil
	platformLogins.mu.Unlock()

	now := time.Now()
	// Spread across many IPs so the per-IP bucket never fires — this is the
	// botnet shape the global ceiling exists for.
	blocked := false
	for i := 0; i < platformLoginGlobalLimit+5; i++ {
		ip := string(rune('a'+i%26)) + ".x"
		if ok, scope := platformLogins.allow(ip, now); !ok {
			if scope != "global" {
				t.Fatalf("blocked by %q, expected the global ceiling", scope)
			}
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatal("the global ceiling never fired across many IPs")
	}
}

func TestClientIPPrefersForwardedFor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	if got := clientIP(r); got != "10.0.0.1" {
		t.Fatalf("without XFF: got %q", got)
	}
	r.Header.Set("X-Forwarded-For", " 203.0.113.7 , 10.0.0.9 ")
	if got := clientIP(r); got != "203.0.113.7" {
		t.Fatalf("with XFF: got %q", got)
	}
}

// The surface must be off unless deliberately enabled.
func TestPlatformSecretDefaultsOff(t *testing.T) {
	os.Unsetenv("PLATFORM_JWT_SECRET")
	if PlatformSecret() != "" {
		t.Fatal("platform surface is enabled with no secret configured")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func signHS256(t *testing.T, secret string, claims map[string]interface{}) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims(claims))
	s, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func bearer(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/platform/whoami", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}
