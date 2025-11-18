package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"ropacal-backend/internal/models"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"
)

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type LoginResponse struct {
	OK    bool                `json:"ok"`
	Token string              `json:"token,omitempty"`
	User  *models.UserResponse `json:"user,omitempty"`
}

func Login(db *sqlx.DB) http.HandlerFunc {
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
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(LoginResponse{OK: false})
			return
		}

		// Find user by email
		var user models.User
		query := "SELECT * FROM users WHERE email = ?"
		if err := db.Get(&user, query, req.Email); err != nil {
			log.Printf("❌ User not found: %s", req.Email)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(LoginResponse{OK: false})
			return
		}

		// Verify password
		if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
			log.Printf("❌ Invalid password for: %s", req.Email)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(LoginResponse{OK: false})
			return
		}

		// Create JWT token with user info
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"user_id": user.ID,
			"email":   user.Email,
			"role":    user.Role,
			"iat":     time.Now().Unix(),
			"exp":     time.Now().Add(7 * 24 * time.Hour).Unix(),
		})

		tokenString, err := token.SignedString([]byte(jwtSecret))
		if err != nil {
			log.Println("❌ Failed to create token")
			http.Error(w, "Failed to create token", http.StatusInternalServerError)
			return
		}

		userResponse := user.ToUserResponse()
		log.Printf("✅ Login successful: %s (%s)", user.Email, user.Role)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(LoginResponse{
			OK:    true,
			Token: tokenString,
			User:  &userResponse,
		})
	}
}
