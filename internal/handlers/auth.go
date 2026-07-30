package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/orgdb"
	"ropacal-backend/pkg/utils"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"
)

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	// Organization is the tenant's slug (organizations.slug), matched
	// case-insensitively. Ignored until the multi-tenancy migration is live.
	// Once live it is required whenever more than one active organization
	// exists; with exactly one active organization it may be omitted
	// (single-org grace, so current login screens keep working until they
	// ship the field).
	Organization string `json:"organization,omitempty"`
}

// LoginOrganization is the organization block returned by a successful login
// once multi-tenancy is live.
// db tags are explicit rather than relying on sqlx's default lowercase name
// mapper, since this struct is now scanned directly in GetAuthStatus.
type LoginOrganization struct {
	ID   string `json:"id" db:"id"`
	Name string `json:"name" db:"name"`
	Slug string `json:"slug" db:"slug"`
}

type LoginResponse struct {
	OK           bool                 `json:"ok"`
	Token        string               `json:"token,omitempty"`
	User         *models.UserResponse `json:"user,omitempty"`
	Organization *LoginOrganization   `json:"organization,omitempty"` // present once tenancy is live
	Error        string               `json:"error,omitempty"`        // only on organization-selection failures (400/403)
}

// writeLoginJSON writes a login response with the given status code.
func writeLoginJSON(w http.ResponseWriter, status int, resp LoginResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

// resolveLoginOrg maps the request's organization slug to a tenant, applying
// the single-org grace when the slug is omitted. A zero status means resolved.
// Unknown slugs return the same opaque 401 as a wrong password so login cannot
// be used to enumerate tenant slugs; a real-but-inactive slug returns 403 with
// a reason (its own users deserve to know why they are locked out).
func resolveLoginOrg(root *sqlx.DB, slug string) (*LoginOrganization, int, string) {
	if slug != "" {
		var row struct {
			ID     string `db:"id"`
			Name   string `db:"name"`
			Slug   string `db:"slug"`
			Status string `db:"status"`
		}
		err := root.Get(&row, `SELECT id, name, slug, status FROM organizations WHERE LOWER(slug) = LOWER($1)`, slug)
		if errors.Is(err, sql.ErrNoRows) {
			log.Printf("❌ Login: unknown organization slug %q", slug)
			return nil, http.StatusUnauthorized, ""
		}
		if err != nil {
			log.Printf("❌ Login: organization lookup failed: %v", err)
			return nil, http.StatusInternalServerError, ""
		}
		if row.Status != "active" {
			log.Printf("❌ Login: organization %q is %s", row.Slug, row.Status)
			return nil, http.StatusForbidden, fmt.Sprintf("organization %q is %s", row.Slug, row.Status)
		}
		return &LoginOrganization{ID: row.ID, Name: row.Name, Slug: row.Slug}, 0, ""
	}

	// No slug supplied: single-org grace.
	var rows []struct {
		ID   string `db:"id"`
		Name string `db:"name"`
		Slug string `db:"slug"`
	}
	if err := root.Select(&rows, `SELECT id, name, slug FROM organizations WHERE status = 'active' ORDER BY created_at, id`); err != nil {
		log.Printf("❌ Login: organization enumeration failed: %v", err)
		return nil, http.StatusInternalServerError, ""
	}
	switch len(rows) {
	case 1:
		log.Printf("ℹ️  Login: no organization supplied; using the only active organization %q (single-org grace)", rows[0].Slug)
		return &LoginOrganization{ID: rows[0].ID, Name: rows[0].Name, Slug: rows[0].Slug}, 0, ""
	case 0:
		log.Println("❌ Login: no active organizations exist")
		return nil, http.StatusUnauthorized, ""
	default:
		return nil, http.StatusBadRequest,
			"organization is required: this server hosts multiple organizations; pass your organization's slug in the 'organization' field"
	}
}

// Login authenticates a user and mints a JWT.
//
// TENANCY: Login is a deliberate public exception — it is what ESTABLISHES the
// org, so it reads users/organizations on the root pool (no app.org_id).
// Pre-migration it behaves exactly as it always has (email-only lookup, no
// org claim); once tenancy is live it resolves (organization, email) and the
// minted token carries org_id.
func Login(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req LoginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		log.Printf("🔐 Login attempt for: %s", req.Email)

		// Get JWT secret
		jwtSecret := os.Getenv("APP_JWT_SECRET")
		if jwtSecret == "" {
			log.Println("❌ JWT secret not configured")
			writeLoginJSON(w, http.StatusInternalServerError, LoginResponse{OK: false})
			return
		}

		var (
			user models.User
			org  *LoginOrganization
		)
		if orgdb.Migrated() {
			resolved, failStatus, failMsg := resolveLoginOrg(root, req.Organization)
			if failStatus != 0 {
				writeLoginJSON(w, failStatus, LoginResponse{OK: false, Error: failMsg})
				return
			}
			org = resolved
			// The users lookup must run ORG-BOUND: under RLS the raw pool has no
			// app.org_id, the users policy fail-closes, and every login would 401
			// (this exact break was caught in the pre-flip gate check, 2026-07-29).
			// orgdb.System sets the GUC for the resolved org; dark mode returns a
			// passthrough so pre-migration behavior is untouched.
			odb, oerr := orgdb.System(root, org.ID)
			if oerr != nil {
				log.Printf("❌ Login org handle for %q: %v", org.Slug, oerr)
				writeLoginJSON(w, http.StatusInternalServerError, LoginResponse{OK: false})
				return
			}
			// Explicit column list: users now also carries organization_id,
			// which models.User deliberately does not scan.
			err := odb.Get(&user,
				`SELECT id, email, password, name, role, created_at, updated_at
				 FROM users WHERE organization_id = $1 AND email = $2`,
				org.ID, req.Email)
			if err != nil {
				log.Printf("❌ User not found in organization %q: %s", org.Slug, req.Email)
				writeLoginJSON(w, http.StatusUnauthorized, LoginResponse{OK: false})
				return
			}
		} else {
			// Single-tenant path — byte-identical to the pre-tenancy build.
			query := "SELECT * FROM users WHERE email = $1"
			if err := root.Get(&user, query, req.Email); err != nil {
				log.Printf("❌ User not found: %s", req.Email)
				writeLoginJSON(w, http.StatusUnauthorized, LoginResponse{OK: false})
				return
			}
		}

		// Verify password
		if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
			log.Printf("❌ Invalid password for: %s", req.Email)
			writeLoginJSON(w, http.StatusUnauthorized, LoginResponse{OK: false})
			return
		}

		// Create JWT token with user info
		claims := jwt.MapClaims{
			"user_id": user.ID,
			"email":   user.Email,
			"role":    user.Role,
			"iat":     time.Now().Unix(),
			"exp":     time.Now().Add(7 * 24 * time.Hour).Unix(),
		}
		if org != nil {
			claims["org_id"] = org.ID
		}
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

		tokenString, err := token.SignedString([]byte(jwtSecret))
		if err != nil {
			log.Println("❌ Failed to create token")
			http.Error(w, "Failed to create token", http.StatusInternalServerError)
			return
		}

		userResponse := user.ToUserResponse()
		if org != nil {
			log.Printf("✅ Login successful: %s (%s) org=%s", user.Email, user.Role, org.Slug)
		} else {
			log.Printf("✅ Login successful: %s (%s)", user.Email, user.Role)
		}

		writeLoginJSON(w, http.StatusOK, LoginResponse{
			OK:           true,
			Token:        tokenString,
			User:         &userResponse,
			Organization: org,
		})
	}
}

// GetAuthStatus returns the current authenticated user's information
func GetAuthStatus(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		log.Printf("📥 REQUEST: GET /api/auth/status")

		// Extract user claims from context (set by Auth middleware)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			log.Println("❌ User claims not found in context")
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("   User ID: %s", userClaims.UserID)

		// Query full user data from database
		var user models.User
		query := "SELECT * FROM users WHERE id = $1"
		if err := db.Get(&user, query, userClaims.UserID); err != nil {
			if err == sql.ErrNoRows {
				log.Printf("❌ User not found: %s", userClaims.UserID)
				utils.RespondError(w, http.StatusNotFound, "User not found")
			} else {
				log.Printf("❌ Database error: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Database error")
			}
			return
		}

		log.Printf("✅ Auth status retrieved for: %s (%s)", user.Email, user.Role)

		resp := map[string]interface{}{
			"success": true,
			"user":    user.ToUserResponse(),
		}

		// Include the organization, mirroring the login response.
		//
		// Clients remember the org SLUG to pre-fill their login form, because
		// once a second tenant exists the slug is mandatory and a wrong one is
		// indistinguishable from a bad password (both are an opaque 401). But
		// the slug was previously only ever learned at LOGIN — so any user
		// holding a valid 7-day token when the second org is provisioned would
		// never have received it, and would land on an empty required field
		// with no way to know what to type. Returning it here lets a
		// session restore backfill it, which is the path every already-signed-in
		// user takes on app launch.
		//
		// organizations is readable on the org-bound handle because it carries a
		// permissive org_catalog_read policy (FOR SELECT USING (true)) alongside
		// its isolation policy. Failure is non-fatal: auth status must keep
		// working even if this lookup does not.
		if orgID := db.OrgID(); orgID != "" {
			var org LoginOrganization
			if err := db.Get(&org,
				`SELECT id, name, slug FROM organizations WHERE id = $1`, orgID); err != nil {
				log.Printf("⚠️  Auth status: could not load organization %s: %v", orgID, err)
			} else {
				resp["organization"] = org
			}
		}

		utils.RespondJSON(w, http.StatusOK, resp)
	}
}
