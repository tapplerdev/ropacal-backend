package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"ropacal-backend/internal/middleware"
	"strings"
	"time"

	"ropacal-backend/internal/models"
	"ropacal-backend/internal/orgdb"
	"ropacal-backend/pkg/utils"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"
)

type CreateUserRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	Role     string `json:"role"` // "driver", "admin", or "manager"
}

type CreateUserResponse struct {
	Success bool                 `json:"success"`
	User    *models.UserResponse `json:"user,omitempty"`
	Message string               `json:"message,omitempty"`
}

// CreateUser creates a new user (admin/manager/driver)
// Requires admin authentication
func CreateUser(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("📥 REQUEST: POST /api/users - Create new user")

		// Parse request body
		var req CreateUserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ Invalid request body: %v", err)
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// Validate required fields
		if req.Email == "" || req.Password == "" || req.Name == "" || req.Role == "" {
			log.Println("❌ Missing required fields")
			utils.RespondError(w, http.StatusBadRequest, "Email, password, name, and role are required")
			return
		}

		// The platform support-identity domain is RESERVED. Without this, a
		// tenant admin could pre-create support+{their-slug}@binly-platform.internal
		// with a password of their choosing; ensureSupportUser resolves that
		// identity by email, would adopt the tenant's row, and every subsequent
		// Binly write in that organization would be attributed to an account the
		// TENANT controls and can log in as. Repudiation would then run both
		// ways — they could act as "Binly Support" and blame us, and our real
		// writes would be indistinguishable from theirs.
		//
		// This is one of two guards; ensureSupportUser independently verifies
		// the row it finds carries the unusable password hash. Either alone is
		// insufficient, because this one does not cover rows created before it
		// existed or by direct SQL.
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(req.Email)), "@"+middleware.SupportUserEmailDomain) {
			log.Printf("🚫 Refused reserved platform domain in user creation: %s", req.Email)
			utils.RespondError(w, http.StatusBadRequest,
				"That email domain is reserved and cannot be used")
			return
		}

		// Validate role.
		//
		// "manager" was accepted here but REJECTED by the users_role_check
		// constraint, which permits only driver|admin. So a request asking for a
		// manager passed validation, reached the INSERT, tripped the CHECK, and
		// came back 500 — a server-fault code for what is plainly a bad request.
		// Retired 2026-07-31: the role is unused (zero such rows in production)
		// and nothing in the dashboard offers it. This list must stay in step
		// with users_role_check; adding a role here without a migration
		// reintroduces the same 500.
		validRoles := map[string]bool{"driver": true, "admin": true}
		if !validRoles[req.Role] {
			log.Printf("❌ Invalid role: %s", req.Role)
			utils.RespondError(w, http.StatusBadRequest, "Role must be 'driver' or 'admin'")
			return
		}

		log.Printf("   📧 Email: %s", req.Email)
		log.Printf("   👤 Name: %s", req.Name)
		log.Printf("   🔑 Role: %s", req.Role)

		// Check if user already exists
		var existingUser models.User
		checkQuery := "SELECT id FROM users WHERE email = $1"
		err := db.Get(&existingUser, checkQuery, req.Email)
		if err == nil {
			log.Printf("❌ User already exists: %s", req.Email)
			utils.RespondError(w, http.StatusConflict, "User with this email already exists")
			return
		}

		// Hash password
		log.Println("🔒 Hashing password...")
		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			log.Printf("❌ Failed to hash password: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to hash password")
			return
		}

		// Create user
		now := time.Now().Unix()
		user := models.User{
			ID:        uuid.New().String(),
			Email:     req.Email,
			Password:  string(hashedPassword),
			Name:      req.Name,
			Role:      req.Role,
			CreatedAt: now,
			UpdatedAt: now,
		}

		// Insert into database
		log.Println("💾 Inserting user into database...")
		insertQuery := `
			INSERT INTO users (id, email, password, name, role, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`
		_, err = db.Exec(
			insertQuery,
			user.ID,
			user.Email,
			user.Password,
			user.Name,
			user.Role,
			user.CreatedAt,
			user.UpdatedAt,
		)
		if err != nil {
			log.Printf("❌ Database error: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to create user")
			return
		}

		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("✅ USER CREATED SUCCESSFULLY")
		log.Printf("   📧 Email: %s", user.Email)
		log.Printf("   👤 Name: %s", user.Name)
		log.Printf("   🔑 Role: %s", user.Role)
		log.Printf("   🆔 ID: %s", user.ID)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		// Return user response (without password)
		userResponse := user.ToUserResponse()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(CreateUserResponse{
			Success: true,
			User:    &userResponse,
			Message: "User created successfully",
		})
	}
}

// GetAllUsers returns all users (drivers, managers, admins)
// GET /api/users
func GetAllUsers(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		log.Println("📤 REQUEST: GET /api/users - Fetch all users")

		// Fetch all users
		var users []models.User
		query := `
			SELECT id, email, name, role, created_at, updated_at
			FROM users
			ORDER BY name ASC
		`
		err := db.Select(&users, query)
		if err != nil {
			log.Printf("❌ Database error: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch users")
			return
		}

		// Convert to user responses (without passwords)
		userResponses := make([]models.UserResponse, len(users))
		for i, user := range users {
			userResponses[i] = user.ToUserResponse()
		}

		log.Printf("✅ Fetched %d users", len(userResponses))

		// Return users
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"users":   userResponses,
		})
	}
}
