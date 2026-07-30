package handlers

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"

	"ropacal-backend/pkg/utils"
)

// Provisioning a tenant over HTTP, mirroring scripts/provision-org.sh.
//
// Behind INTERNAL_API_KEY rather than a user JWT, deliberately: creating an
// organization is a cross-tenant act, and there is no "super admin" role in this
// system — every authenticated identity belongs to exactly one org. Gating this
// on an admin JWT would therefore mean an admin of org A could mint org B, which
// is precisely the boundary the whole tenancy migration exists to draw.
//
// READ THIS BEFORE CALLING IT. Creating a SECOND organization changes behaviour
// for the FIRST one, immediately and irreversibly-ish:
//   - Login stops accepting a missing `organization` slug and returns 400. Every
//     client must already be sending it.
//   - Centrifugo proxy requests are denied unless the connection carries org
//     meta (verified working 2026-07-30, but it becomes load-bearing here).
//   - The proxy secret becomes mandatory rather than advisory.
//
// Those gates are documented in TENANCY_BACKLOG.md under "The org-count rule".
// They cache for ~30s, so the change lands seconds after this returns.

type CreateOrganizationRequest struct {
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	AdminEmail string `json:"admin_email"`
	// Optional warehouse. Supply it whenever you know it — there is no
	// dashboard UI for setting a warehouse, so a tenant created without one has
	// no self-service way to fix it and every route optimization returns 412.
	WarehouseAddress   string   `json:"warehouse_address,omitempty"`
	WarehouseLatitude  *float64 `json:"warehouse_latitude,omitempty"`
	WarehouseLongitude *float64 `json:"warehouse_longitude,omitempty"`
}

type CreateOrganizationResponse struct {
	OrganizationID string `json:"organization_id"`
	Name           string `json:"name"`
	Slug           string `json:"slug"`
	AdminUserID    string `json:"admin_user_id"`
	AdminEmail     string `json:"admin_email"`
	// AdminPassword is returned ONCE and never recoverable — the column stores
	// a bcrypt hash. Capture it from this response or the tenant cannot log in.
	AdminPassword string `json:"admin_password"`
	Warehouse     struct {
		Address   string  `json:"address"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"warehouse"`
	Warning string `json:"warning,omitempty"`
}

// Used in login requests and as a URL/Centrifugo-safe identifier.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}$`)

// CreateOrganization provisions organization + first admin + warehouse in ONE
// transaction.
//
// One transaction is the whole point. All 35 foreign keys pointing at
// `organizations` are ON DELETE RESTRICT, so a half-provisioned org — org row
// created, admin not — cannot simply be deleted afterwards. It would sit there
// permanently AND flip the org-count gates above for the existing tenant. Any
// failure must leave nothing behind.
func CreateOrganization(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CreateOrganizationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		req.Name = strings.TrimSpace(req.Name)
		req.Slug = strings.ToLower(strings.TrimSpace(req.Slug))
		req.AdminEmail = strings.TrimSpace(req.AdminEmail)
		req.WarehouseAddress = strings.TrimSpace(req.WarehouseAddress)

		if req.Name == "" {
			utils.RespondError(w, http.StatusBadRequest, "name is required")
			return
		}
		if !slugPattern.MatchString(req.Slug) {
			utils.RespondError(w, http.StatusBadRequest,
				"slug must be lowercase alphanumeric or hyphen, 2-31 characters, starting with a letter or digit")
			return
		}
		if !strings.Contains(req.AdminEmail, "@") {
			utils.RespondError(w, http.StatusBadRequest, "admin_email is required and must be an email address")
			return
		}

		// Warehouse: validate rather than trust. A wrong coordinate is WORSE
		// than an absent one — 0,0 produces a clean 412 from route
		// optimization, while a plausible-but-wrong lat/lon silently routes
		// drivers to the wrong place.
		lat, lon := 0.0, 0.0
		address := "CHANGE ME"
		if req.WarehouseLatitude != nil || req.WarehouseLongitude != nil {
			if req.WarehouseLatitude == nil || req.WarehouseLongitude == nil {
				utils.RespondError(w, http.StatusBadRequest, "warehouse_latitude and warehouse_longitude must be supplied together")
				return
			}
			lat, lon = *req.WarehouseLatitude, *req.WarehouseLongitude
			if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
				utils.RespondError(w, http.StatusBadRequest, "warehouse coordinates out of range")
				return
			}
			if req.WarehouseAddress == "" {
				utils.RespondError(w, http.StatusBadRequest, "warehouse_address is required when coordinates are supplied")
				return
			}
			address = req.WarehouseAddress
		}

		// Check the slug before doing any work, so the common mistake gets a
		// clear 409 rather than a raw unique-violation.
		var existing int
		if err := root.Get(&existing,
			`SELECT count(*) FROM organizations WHERE lower(slug) = $1`, req.Slug); err != nil {
			log.Printf("❌ [CreateOrg] slug precheck failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not verify slug availability")
			return
		}
		if existing > 0 {
			utils.RespondError(w, http.StatusConflict, fmt.Sprintf("organization slug %q is already taken", req.Slug))
			return
		}

		password, err := generatePassword()
		if err != nil {
			log.Printf("❌ [CreateOrg] password generation failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not generate password")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			log.Printf("❌ [CreateOrg] hashing failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not hash password")
			return
		}

		orgID := uuid.NewString()
		userID := uuid.NewString()
		now := time.Now().Unix()

		tx, err := root.Beginx()
		if err != nil {
			log.Printf("❌ [CreateOrg] begin failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not start transaction")
			return
		}
		defer tx.Rollback() //nolint:errcheck // no-op after Commit

		// Scope the session so organization_id column DEFAULTs resolve and the
		// organizations RLS WITH CHECK (id = current_setting('app.org_id')) is
		// satisfied when running as the non-superuser app role. This is the
		// same set_config the provisioning script issues, and the reason no new
		// database credential is needed.
		if _, err := tx.Exec(`SELECT set_config('app.org_id', $1, true)`, orgID); err != nil {
			log.Printf("❌ [CreateOrg] set_config failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not set organization context")
			return
		}

		if _, err := tx.Exec(
			`INSERT INTO organizations (id, name, slug, status) VALUES ($1, $2, $3, 'active')`,
			orgID, req.Name, req.Slug); err != nil {
			log.Printf("❌ [CreateOrg] organization insert failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not create organization")
			return
		}

		if _, err := tx.Exec(
			`INSERT INTO users (id, email, password, name, role, organization_id, created_at, updated_at)
			 VALUES ($1, $2, $3, 'Admin', 'admin', $4, $5, $5)`,
			userID, req.AdminEmail, string(hash), orgID, now); err != nil {
			log.Printf("❌ [CreateOrg] admin insert failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not create admin user")
			return
		}

		// config.id is a SERIAL INTEGER, not a uuid — omit it. (Passing a uuid
		// here is a mistake that has actually been made; it fails with "invalid
		// input syntax for type integer".)
		if _, err := tx.Exec(
			`INSERT INTO config (key, value, organization_id)
			 VALUES ('warehouse_location',
			         jsonb_build_object('address', $1::text, 'latitude', $2::numeric, 'longitude', $3::numeric),
			         $4)`,
			address, lat, lon, orgID); err != nil {
			log.Printf("❌ [CreateOrg] warehouse insert failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not create warehouse config")
			return
		}

		// Verify inside the transaction rather than trusting the inserts, so a
		// tenant that cannot log in or route never gets committed.
		var users, warehouses int
		if err := tx.Get(&users, `SELECT count(*) FROM users WHERE organization_id = $1`, orgID); err != nil ||
			users != 1 {
			log.Printf("❌ [CreateOrg] post-check: expected 1 admin, got %d (err=%v)", users, err)
			utils.RespondError(w, http.StatusInternalServerError, "provisioning verification failed")
			return
		}
		if err := tx.Get(&warehouses,
			`SELECT count(*) FROM config WHERE organization_id = $1 AND key = 'warehouse_location'`, orgID); err != nil ||
			warehouses != 1 {
			log.Printf("❌ [CreateOrg] post-check: warehouse row missing (err=%v)", err)
			utils.RespondError(w, http.StatusInternalServerError, "provisioning verification failed")
			return
		}

		if err := tx.Commit(); err != nil {
			log.Printf("❌ [CreateOrg] commit failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "could not commit provisioning")
			return
		}

		log.Printf("🏢 [CreateOrg] provisioned %q (slug=%s, id=%s) with admin %s",
			req.Name, req.Slug, orgID, req.AdminEmail)

		resp := CreateOrganizationResponse{
			OrganizationID: orgID,
			Name:           req.Name,
			Slug:           req.Slug,
			AdminUserID:    userID,
			AdminEmail:     req.AdminEmail,
			AdminPassword:  password,
		}
		resp.Warehouse.Address = address
		resp.Warehouse.Latitude = lat
		resp.Warehouse.Longitude = lon
		if lat == 0 && lon == 0 {
			resp.Warning = "warehouse is 0,0 — route optimization returns 412 until it is set, and there is no dashboard UI for it (PATCH /api/config/warehouse as this org's admin)"
		}

		utils.RespondJSON(w, http.StatusCreated, resp)
	}
}

// generatePassword returns a URL-safe random password. crypto/rand, not
// math/rand — this is the tenant's only credential.
func generatePassword() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	s := base64.RawURLEncoding.EncodeToString(buf)
	if len(s) > 20 {
		s = s[:20]
	}
	return s, nil
}
