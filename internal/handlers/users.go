package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"ropacal-backend/internal/models"
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
func CreateUser(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		// Validate role
		validRoles := map[string]bool{"driver": true, "admin": true, "manager": true}
		if !validRoles[req.Role] {
			log.Printf("❌ Invalid role: %s", req.Role)
			utils.RespondError(w, http.StatusBadRequest, "Role must be 'driver', 'admin', or 'manager'")
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
