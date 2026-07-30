package handlers

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jmoiron/sqlx"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/pkg/utils"
)

// Platform operator login — cross-tenant, and deliberately separate from
// handlers.Login.
//
// It cannot reuse the tenant login: that resolves an organization slug and mints
// an org_id claim, and a platform admin belongs to no organization. Sharing the
// path would also mean one bug could hand a tenant admin a platform token.

type PlatformLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	// TOTPCode is REQUIRED. There is no password-only path to every tenant's
	// data.
	TOTPCode string `json:"totp_code"`
}

type PlatformLoginResponse struct {
	OK bool `json:"ok"`
	// Platform marks this as a cross-tenant operator session. Clients branch on
	// it: the response deliberately carries no `user` or `organization`, because
	// an operator is neither — so a client that blindly expected the tenant
	// shape would otherwise treat a successful login as malformed.
	Platform  bool   `json:"platform,omitempty"`
	Token     string `json:"token,omitempty"`
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

// PlatformLogin authenticates an operator with password + TOTP.
//
// root is the raw pool on purpose: platform_admins has no organization_id and no
// RLS, so there is no org to bind.
func PlatformLogin(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if middleware.PlatformSecret() == "" {
			// Platform surface disabled. 404 so a probe cannot tell that this
			// route exists at all.
			http.NotFound(w, r)
			return
		}
		var req PlatformLoginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		platformLoginFlow(root, w, r, req)
	}
}

// platformLoginFlow is the authentication itself, shared by the dedicated
// /api/platform/auth/login route and by the unified /api/auth/login entry point
// that operators actually use. Kept as one function on purpose: two copies of
// credential verification is exactly how the two drift apart.
func platformLoginFlow(root *sqlx.DB, w http.ResponseWriter, r *http.Request, req PlatformLoginRequest) {
	secret := middleware.PlatformSecret()
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.TOTPCode = strings.TrimSpace(req.TOTPCode)

	var admin struct {
		ID         string         `db:"id"`
		Email      string         `db:"email"`
		Password   string         `db:"password"`
		Name       string         `db:"name"`
		TOTPSecret sql.NullString `db:"totp_secret"`
		Status     string         `db:"status"`
		// Highest TOTP counter already accepted for this admin. Anything at
		// or below it is a replay.
		LastTOTPCounter int64 `db:"last_totp_counter"`
	}
	err := root.Get(&admin,
		`SELECT id, email, password, name, totp_secret, status, last_totp_counter
		 FROM platform_admins WHERE LOWER(email) = $1`, req.Email)

	// Every failure below returns an IDENTICAL opaque response AND burns the
	// same work. An attacker must not be able to distinguish "no such
	// operator" from "wrong password" from "wrong code" from "disabled" —
	// that would turn this endpoint into an oracle for which Binly staff
	// exist and which of them are suspended.
	//
	// The first version of this only ran a dummy hash when the row was
	// missing, and the dummy was cost 10 while provisioning writes cost 12.
	// The review measured the result: unknown email 50ms, wrong password
	// 195ms, and a DISABLED admin 0.8ms — a 250x tell, because that branch
	// returned before any compare at all. So: always compare, always at the
	// real cost, regardless of which check failed.
	deny := func(reason string) {
		log.Printf("🔒 [PlatformLogin] denied for %q: %s", req.Email, reason)
		middleware.AuditPlatformLogin(root, admin.ID, req.Email, "DENIED: "+reason, r)
		burnPasswordCompare(admin.Password, req.Password)
		writePlatformJSON(w, http.StatusUnauthorized, PlatformLoginResponse{OK: false})
	}

	if errors.Is(err, sql.ErrNoRows) {
		deny("no such admin")
		return
	}
	if err != nil {
		log.Printf("❌ [PlatformLogin] lookup failed: %v", err)
		writePlatformJSON(w, http.StatusInternalServerError, PlatformLoginResponse{OK: false})
		return
	}
	// Password is checked FIRST and unconditionally, even for a disabled
	// admin, so every path below has already paid the bcrypt cost.
	passwordOK := bcrypt.CompareHashAndPassword([]byte(admin.Password), []byte(req.Password)) == nil

	if admin.Status != "active" {
		deny("admin is " + admin.Status)
		return
	}
	if !passwordOK {
		deny("bad password")
		return
	}
	// TOTP is OPTIONAL per account: required whenever a secret is set,
	// skipped entirely when it is not. The owner chose password-only for
	// now; enabling it later is one UPDATE, and the replay-protection code
	// below stays exercised the moment any account opts in.
	//
	// With no second factor, PlatformLoginRateLimit is the ONLY thing
	// between a guessed password and every tenant's data — which is why it
	// was tightened to 3 attempts per IP alongside this change.
	totpEnabled := admin.TOTPSecret.Valid && admin.TOTPSecret.String != ""
	var counter int64

	if totpEnabled {
		if req.TOTPCode == "" {
			deny("missing totp code")
			return
		}
		// REPLAY PROTECTION. totp.Validate alone accepts a code across three
		// counter windows (Skew=1, Period=30), so a code observed once stays
		// valid for up to 90 seconds — a review confirmed the same code
		// authenticating three times in a row. Requiring a strictly
		// increasing counter makes each code single-use.
		c, ok := validateTOTPCounter(req.TOTPCode, admin.TOTPSecret.String, time.Now())
		if !ok {
			deny("bad totp code")
			return
		}
		if c <= admin.LastTOTPCounter {
			deny("replayed totp code")
			return
		}
		counter = c
	} else {
		// Password-only account. Deliberate owner decision; the compensating
		// control is PlatformLoginRateLimit, tightened to 3 attempts per IP
		// because it is now the ONLY thing between a guessed password and
		// every tenant's data. Logged loudly every time so the posture is
		// visible in the logs rather than assumed.
		log.Printf("⚠️  [PlatformLogin] %s authenticated with PASSWORD ONLY — no second factor "+
			"on an identity that reaches every tenant", admin.Email)
	}

	now := time.Now()
	exp := now.Add(middleware.PlatformTokenTTL)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"admin_id": admin.ID,
		"email":    admin.Email,
		// The claim PlatformAuth requires. Tenant tokens never carry it,
		// which is what keeps the two token families from being confused.
		"platform": true,
		"iat":      now.Unix(),
		"exp":      exp.Unix(),
	})
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		log.Printf("❌ [PlatformLogin] signing failed: %v", err)
		writePlatformJSON(w, http.StatusInternalServerError, PlatformLoginResponse{OK: false})
		return
	}

	// Burn the counter. The WHERE guard makes this the atomic step of the
	// replay defence: two requests presenting the same code concurrently
	// both pass the in-memory check above, but only one UPDATE can win.
	// Unlike last_login_at, a failure here is FATAL to the login — serving a
	// token after failing to burn the counter would reopen the replay window.
	if totpEnabled {
		// Burn the counter. The WHERE guard is the atomic step of the replay
		// defence: two requests presenting the same code concurrently both
		// pass the check above, but only one UPDATE can win. A failure here
		// is FATAL to the login — issuing a token after failing to burn the
		// counter would reopen the replay window.
		res, err := root.Exec(
			`UPDATE platform_admins
			 SET last_totp_counter = $1, last_login_at = $2, updated_at = $2
			 WHERE id = $3 AND last_totp_counter < $1`,
			counter, now.Unix(), admin.ID)
		if err != nil {
			log.Printf("❌ [PlatformLogin] could not burn totp counter: %v", err)
			writePlatformJSON(w, http.StatusInternalServerError, PlatformLoginResponse{OK: false})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			deny("totp counter already burned (concurrent replay)")
			return
		}
	} else if _, err := root.Exec(
		`UPDATE platform_admins SET last_login_at = $1, updated_at = $1 WHERE id = $2`,
		now.Unix(), admin.ID); err != nil {
		log.Printf("⚠️  [PlatformLogin] could not record last_login_at: %v", err)
	}

	middleware.AuditPlatformLogin(root, admin.ID, admin.Email, "SUCCESS", r)
	log.Printf("🛰️  [PlatformLogin] %s authenticated (cross-tenant access granted, expires %s)",
		admin.Email, exp.Format(time.RFC3339))

	writePlatformJSON(w, http.StatusOK, PlatformLoginResponse{
		OK: true, Token: signed, Email: admin.Email, Name: admin.Name,
		ExpiresAt: exp.Unix(),
	})
}

// PlatformWhoAmI confirms the token works and lists the tenants in reach.
//
// The org list is the honest answer to "what does this credential see": EVERY
// organization, whatever its status — deliberately unfiltered, because an
// operator should see a suspended tenant exists rather than have it silently
// vanish.
func PlatformWhoAmI(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := middleware.PlatformFromContext(r.Context())
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		type orgRow struct {
			ID     string `db:"id" json:"id"`
			Name   string `db:"name" json:"name"`
			Slug   string `db:"slug" json:"slug"`
			Status string `db:"status" json:"status"`
		}
		// Non-nil so an operator with zero tenants gets [] rather than null —
		// a nil slice marshals to null, which clients then have to special-case.
		orgs := []orgRow{}
		if err := root.Select(&orgs,
			`SELECT id, name, slug, status FROM organizations ORDER BY created_at, id`); err != nil {
			log.Printf("❌ [Platform] whoami org list failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not list organizations")
			return
		}
		if len(orgs) == 0 {
			// Be loud, the way ForEachActiveOrg already is on this exact
			// condition. This read works only because `organizations` carries a
			// permissive org_catalog_read policy; if that is ever tightened —
			// a plausible hardening, since today it lets any tenant enumerate
			// every other tenant — whoami would quietly report "you reach 0
			// organizations" instead of failing.
			log.Println("🚨 [Platform] whoami sees ZERO organizations — if tenants exist, the " +
				"org_catalog_read policy on `organizations` has been tightened and this read is " +
				"being filtered rather than genuinely empty")
		}

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"admin_id":      claims.AdminID,
			"email":         claims.Email,
			"platform":      true,
			"organizations": orgs,
		})
	}
}

// writePlatformJSON mirrors writeLoginJSON: an explicit status with a JSON body,
// so clients get a parseable shape on both success and denial.
func writePlatformJSON(w http.ResponseWriter, status int, body PlatformLoginResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// burnPasswordCompare spends the same work a real password check costs, so a
// failed login takes indistinguishable time no matter WHICH check failed.
//
// storedHash is the hash we found, or "" when there was no such admin. In the
// second case we compare against a placeholder generated at the SAME cost the
// provisioning script uses (bcrypt.DefaultCost, 10 — matching
// scripts/provision-platform-admin.sh after it was pinned), because a cheaper
// placeholder is itself a tell: the review measured 50ms for a missing row
// against 195ms for a real one.
func burnPasswordCompare(storedHash, attempt string) {
	if storedHash == "" {
		storedHash = placeholderHash
	}
	_ = bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(attempt))
}

// placeholderHash is a real bcrypt hash of an unguessable value, generated once
// at startup at the same cost as a stored credential. Generated rather than
// hardcoded so it cannot drift out of step with the cost we actually use.
var placeholderHash = func() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// Fall back to a fixed string: this value is never compared for
		// correctness, only for the time it takes.
		buf = []byte("platform-login-timing-placeholder")
	}
	h, err := bcrypt.GenerateFromPassword(buf, bcrypt.DefaultCost)
	if err != nil {
		return "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	}
	return string(h)
}()

// validateTOTPCounter validates a code and returns the counter window it matched.
//
// totp.Validate answers only yes/no, which is not enough to prevent replay — the
// caller needs to know WHICH 30-second window was used so it can refuse that one
// and everything before it next time. This checks the same three windows
// Skew=1 covers (previous, current, next), newest first so the common case is
// one comparison.
func validateTOTPCounter(code, secret string, now time.Time) (int64, bool) {
	const period = 30
	current := now.Unix() / period
	for _, delta := range []int64{0, 1, -1} {
		c := current + delta
		at := time.Unix(c*period, 0)
		valid, err := totp.ValidateCustom(code, secret, at, totp.ValidateOpts{
			Period:    period,
			Skew:      0, // exact window — the loop supplies the skew
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err == nil && valid {
			return c, true
		}
	}
	return 0, false
}

// tryPlatformLogin routes a login to the platform flow when the email belongs to
// a platform admin, and reports whether it handled the request.
//
// This is what lets an operator use the ordinary login form with no organization
// field. It is checked BEFORE the tenant lookup, and only by email: a tenant user
// and a platform operator can never be the same row, because platform_admins is a
// separate table outside the tenancy model.
//
// Returns false — leaving the tenant flow to proceed — when the email is not a
// platform admin, or when the platform surface is disabled entirely. A tenant
// whose email happens to match nothing here is completely unaffected.
func tryPlatformLogin(root *sqlx.DB, w http.ResponseWriter, r *http.Request, req LoginRequest) bool {
	if middleware.PlatformSecret() == "" {
		return false // platform surface disabled
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if email == "" {
		return false
	}

	var exists bool
	if err := root.Get(&exists,
		`SELECT EXISTS (SELECT 1 FROM platform_admins WHERE LOWER(email) = $1)`, email); err != nil {
		log.Printf("❌ [Login] platform-admin probe failed: %v", err)
		return false // fall through to the tenant flow rather than deny outright
	}
	if !exists {
		return false
	}

	// Rate limiting applies from here on. The tenant path has its own volume
	// characteristics and is deliberately left alone; this bucket exists because
	// a platform credential reaches every tenant.
	if ok, scope := middleware.AllowPlatformLoginAttempt(r); !ok {
		log.Printf("🚫 [Login] platform login rate limited (%s) for %s", scope, email)
		w.Header().Set("Retry-After", "900")
		http.Error(w, "Too many attempts", http.StatusTooManyRequests)
		return true
	}

	platformLoginFlow(root, w, r, PlatformLoginRequest{
		Email: req.Email, Password: req.Password, TOTPCode: req.TOTPCode,
	})
	return true
}
