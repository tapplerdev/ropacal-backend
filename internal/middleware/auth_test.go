package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Auth is the gate every driver and manager passes through. jwt.Parse validates
// `exp` when the claim is PRESENT but accepts a token that simply omits it — so
// without WithExpirationRequired, a token with no expiry is valid forever. The
// 7-day AccessTokenTTL was being honoured by the minter alone, and there is no
// refresh endpoint and no revocation list, so "never expires" is the whole
// lifetime.
//
// platform.go had already required this, with a comment describing the same
// finding; the tenant path never got the fix. These tests exist so it cannot be
// dropped again silently — the only symptom would be tokens that outlive their
// TTL, which nothing observes.

func signTenantToken(t *testing.T, secret string, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("signing test token: %v", err)
	}
	return s
}

// serveWithToken runs the Auth middleware over a request bearing the token and
// reports the status the client would see.
func serveWithToken(t *testing.T, secret, token string) int {
	t.Helper()
	t.Setenv("APP_JWT_SECRET", secret)

	reached := false
	h := Auth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/bins", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK && !reached {
		t.Fatal("200 without reaching the handler — the test is not measuring what it thinks")
	}
	return rec.Code
}

func TestAuth_RejectsTokenWithoutExp(t *testing.T) {
	const secret = "test-secret-not-a-real-key"

	// A token that omits exp entirely. Correctly signed, so the ONLY thing that
	// can reject it is the expiration requirement.
	token := signTenantToken(t, secret, jwt.MapClaims{
		"user_id": "11111111-1111-1111-1111-111111111111",
		"email":   "driver@example.test",
		"role":    "driver",
		"iat":     time.Now().Unix(),
	})

	if got := serveWithToken(t, secret, token); got != http.StatusUnauthorized {
		t.Errorf("token with no exp got %d, want 401 — such a token never expires, "+
			"and there is no refresh or revocation path to recover from that", got)
	}
}

func TestAuth_AcceptsNormallyMintedToken(t *testing.T) {
	const secret = "test-secret-not-a-real-key"

	// Exactly what handlers/auth.go mints, so requiring exp cannot lock out a
	// live user. Every minter in the repo sets exp; this pins that assumption
	// rather than trusting a grep of them.
	token := signTenantToken(t, secret, jwt.MapClaims{
		"user_id": "11111111-1111-1111-1111-111111111111",
		"email":   "driver@example.test",
		"role":    "driver",
		"iat":     time.Now().Unix(),
		"exp":     time.Now().Add(7 * 24 * time.Hour).Unix(),
		"org_id":  "22222222-2222-2222-2222-222222222222",
	})

	if got := serveWithToken(t, secret, token); got != http.StatusOK {
		t.Errorf("a normally minted token got %d, want 200 — requiring exp must "+
			"not lock out the tokens production actually issues", got)
	}
}

func TestAuth_RejectsExpiredToken(t *testing.T) {
	const secret = "test-secret-not-a-real-key"

	token := signTenantToken(t, secret, jwt.MapClaims{
		"user_id": "11111111-1111-1111-1111-111111111111",
		"role":    "driver",
		"exp":     time.Now().Add(-1 * time.Hour).Unix(),
	})

	if got := serveWithToken(t, secret, token); got != http.StatusUnauthorized {
		t.Errorf("expired token got %d, want 401", got)
	}
}

func TestAuth_RejectsTokenSignedWithTheWrongSecret(t *testing.T) {
	token := signTenantToken(t, "some-other-secret", jwt.MapClaims{
		"user_id": "11111111-1111-1111-1111-111111111111",
		"role":    "driver",
		"exp":     time.Now().Add(time.Hour).Unix(),
	})

	if got := serveWithToken(t, "test-secret-not-a-real-key", token); got != http.StatusUnauthorized {
		t.Errorf("foreign-signed token got %d, want 401", got)
	}
}

func TestAuth_MissingSecretIsNotAnOpenDoor(t *testing.T) {
	// A blank APP_JWT_SECRET must fail closed. HS256 with an empty key is a
	// valid signature over attacker-chosen claims, so falling through here
	// would authenticate anyone.
	os.Unsetenv("APP_JWT_SECRET")
	token := signTenantToken(t, "", jwt.MapClaims{
		"user_id": "11111111-1111-1111-1111-111111111111",
		"role":    "admin",
		"exp":     time.Now().Add(time.Hour).Unix(),
	})

	if got := serveWithToken(t, "", token); got == http.StatusOK {
		t.Error("an unset APP_JWT_SECRET authenticated a request — it must fail closed")
	}
}
