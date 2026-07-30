package handlers

import (
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/pkg/utils"
)

// Provisioning a PLATFORM admin over HTTP — the operator equivalent of
// POST /api/internal/organizations.
//
// Behind INTERNAL_API_KEY rather than any user session, and deliberately NOT
// behind a platform token: an operator must not be able to mint more operators.
// That is the same reasoning that keeps org creation off the tenant surface —
// the account that can grant cross-tenant access has to sit outside the system
// it grants access to.
//
// This is the one credential in Binly that reaches every customer's data. Create
// as few as possible.

type CreatePlatformAdminRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	// EnableTOTP enrols a second factor. Off by default because the owner chose
	// password-only, but strongly recommended: without it the login rate limiter
	// is the only thing between a guessed password and every tenant's data.
	EnableTOTP bool `json:"enable_totp,omitempty"`
	// CanWrite defaults to true. Set false for staff who should see every tenant
	// but change nothing; enforced in middleware.PlatformAuth.
	CanWrite *bool `json:"can_write,omitempty"`
}

type CreatePlatformAdminResponse struct {
	AdminID string `json:"admin_id"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	// Password is returned ONCE and is not recoverable — the column stores a
	// bcrypt hash.
	Password string `json:"password"`
	CanWrite bool   `json:"can_write"`
	// TOTPSecret and TOTPURI are present only when a second factor was enrolled.
	TOTPSecret string `json:"totp_secret,omitempty"`
	TOTPURI    string `json:"totp_uri,omitempty"`
	Warning    string `json:"warning,omitempty"`
}

// CreatePlatformAdmin provisions a cross-tenant operator.
func CreatePlatformAdmin(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CreatePlatformAdminRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req.Email = strings.TrimSpace(strings.ToLower(req.Email))
		req.Name = strings.TrimSpace(req.Name)

		if !strings.Contains(req.Email, "@") {
			utils.RespondError(w, http.StatusBadRequest, "email is required and must be an email address")
			return
		}
		if req.Name == "" {
			utils.RespondError(w, http.StatusBadRequest, "name is required")
			return
		}
		// The support-identity domain belongs to the tenancy machinery, not to
		// people. Allowing an operator account under it would collide with the
		// per-organization support users.
		if strings.HasSuffix(req.Email, "@"+middleware.SupportUserEmailDomain) {
			utils.RespondError(w, http.StatusBadRequest, "that email domain is reserved")
			return
		}

		var existing int
		if err := root.Get(&existing,
			`SELECT count(*) FROM platform_admins WHERE LOWER(email) = $1`, req.Email); err != nil {
			log.Printf("❌ [CreatePlatformAdmin] precheck failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not verify availability")
			return
		}
		if existing > 0 {
			utils.RespondError(w, http.StatusConflict, "a platform admin with that email already exists")
			return
		}

		password, err := generatePlatformPassword()
		if err != nil {
			log.Printf("❌ [CreatePlatformAdmin] password generation failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not generate credentials")
			return
		}
		// bcrypt.DefaultCost matches the login handler's timing-equalisation
		// placeholder. A mismatch here reopens the oracle that distinguishes a
		// real operator email from an unknown one.
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			log.Printf("❌ [CreatePlatformAdmin] hashing failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not hash password")
			return
		}

		var totpSecret sql.NullString
		var totpURI string
		if req.EnableTOTP {
			// 160 bits, per RFC 6238's recommendation for SHA-1.
			buf := make([]byte, 20)
			if _, err := rand.Read(buf); err != nil {
				utils.RespondError(w, http.StatusInternalServerError, "could not generate TOTP secret")
				return
			}
			s := strings.TrimRight(base32.StdEncoding.EncodeToString(buf), "=")
			totpSecret = sql.NullString{String: s, Valid: true}
			totpURI = "otpauth://totp/Binly%20Platform:" + req.Email +
				"?secret=" + s + "&issuer=Binly%20Platform&algorithm=SHA1&digits=6&period=30"
		}

		canWrite := true
		if req.CanWrite != nil {
			canWrite = *req.CanWrite
		}

		adminID := uuid.NewString()
		now := time.Now().Unix()
		if _, err := root.Exec(
			`INSERT INTO platform_admins
			   (id, email, password, name, totp_secret, status, can_write, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, 'active', $6, $7, $7)`,
			adminID, req.Email, string(hash), req.Name, totpSecret, canWrite, now); err != nil {
			log.Printf("❌ [CreatePlatformAdmin] insert failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not create platform admin")
			return
		}

		log.Printf("🛰️  [CreatePlatformAdmin] provisioned CROSS-TENANT operator %s (totp=%v write=%v)",
			req.Email, req.EnableTOTP, canWrite)

		resp := CreatePlatformAdminResponse{
			AdminID: adminID, Email: req.Email, Name: req.Name,
			Password: password, CanWrite: canWrite,
		}
		if totpSecret.Valid {
			resp.TOTPSecret = totpSecret.String
			resp.TOTPURI = totpURI
		} else {
			resp.Warning = "No second factor. This credential reaches EVERY tenant's data and the login " +
				"rate limiter is the only thing protecting it — pass enable_totp:true to enrol one."
		}
		if middleware.PlatformSecret() == "" {
			resp.Warning = strings.TrimSpace(resp.Warning +
				" PLATFORM_JWT_SECRET is not set, so the platform surface is disabled and these credentials cannot be used yet.")
		}

		utils.RespondJSON(w, http.StatusCreated, resp)
	}
}

// generatePlatformPassword is kept distinct from the org one so a change to
// either cannot silently weaken the other.
func generatePlatformPassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	s := strings.NewReplacer("/", "", "+", "", "=", "").Replace(base64.StdEncoding.EncodeToString(buf))
	if len(s) > 24 {
		s = s[:24]
	}
	return s, nil
}
