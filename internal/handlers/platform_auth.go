package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jmoiron/sqlx"
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
	OK        bool   `json:"ok"`
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
		secret := middleware.PlatformSecret()
		if secret == "" {
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
		req.Email = strings.TrimSpace(strings.ToLower(req.Email))
		req.TOTPCode = strings.TrimSpace(req.TOTPCode)

		var admin struct {
			ID         string `db:"id"`
			Email      string `db:"email"`
			Password   string `db:"password"`
			Name       string `db:"name"`
			TOTPSecret string `db:"totp_secret"`
			Status     string `db:"status"`
		}
		err := root.Get(&admin,
			`SELECT id, email, password, name, totp_secret, status
			 FROM platform_admins WHERE LOWER(email) = $1`, req.Email)

		// Every failure below returns an IDENTICAL opaque response. An attacker
		// must not be able to distinguish "no such operator" from "wrong
		// password" from "wrong code" from "disabled" — that would turn this
		// endpoint into an oracle for which Binly staff exist.
		deny := func(reason string) {
			log.Printf("🔒 [PlatformLogin] denied for %q: %s", req.Email, reason)
			// Constant-ish cost on the miss path so a missing row is not
			// obviously faster than a bad password.
			if admin.Password == "" {
				_ = bcrypt.CompareHashAndPassword(
					[]byte("$2a$10$xxxxxxxxxxxxxxxxxxxxxuxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"),
					[]byte(req.Password))
			}
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
		if admin.Status != "active" {
			deny("admin is " + admin.Status)
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(admin.Password), []byte(req.Password)) != nil {
			deny("bad password")
			return
		}
		if req.TOTPCode == "" {
			deny("missing totp code")
			return
		}
		// Validate re-derives the expected code and compares; it also accepts
		// the adjacent window to tolerate clock skew.
		if !totp.Validate(req.TOTPCode, admin.TOTPSecret) {
			deny("bad totp code")
			return
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

		if _, err := root.Exec(
			`UPDATE platform_admins SET last_login_at = $1, updated_at = $1 WHERE id = $2`,
			now.Unix(), admin.ID); err != nil {
			log.Printf("⚠️  [PlatformLogin] could not record last_login_at: %v", err)
		}

		log.Printf("🛰️  [PlatformLogin] %s authenticated (cross-tenant access granted, expires %s)",
			admin.Email, exp.Format(time.RFC3339))

		writePlatformJSON(w, http.StatusOK, PlatformLoginResponse{
			OK: true, Token: signed, Email: admin.Email, Name: admin.Name,
			ExpiresAt: exp.Unix(),
		})
	}
}

// PlatformWhoAmI confirms the token works and lists the tenants in reach.
//
// The org list is the honest answer to "what does this credential see": every
// active organization.
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
