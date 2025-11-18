package middleware

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

type contextKey string

const UserContextKey contextKey = "user"

type UserClaims struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
}

// Auth middleware validates JWT token and adds user claims to context
func Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			log.Println("❌ No authorization header")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Extract Bearer token
		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			log.Println("❌ Invalid authorization header format")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		tokenString := parts[1]

		// Get JWT secret
		jwtSecret := os.Getenv("APP_JWT_SECRET")
		if jwtSecret == "" {
			log.Println("❌ JWT secret not configured")
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Parse and validate token
		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			// Validate signing method
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(jwtSecret), nil
		})

		if err != nil || !token.Valid {
			log.Printf("❌ Invalid token: %v", err)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Extract claims
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			log.Println("❌ Failed to parse claims")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Convert to UserClaims struct
		userClaims := UserClaims{
			UserID: claims["user_id"].(string),
			Email:  claims["email"].(string),
			Role:   claims["role"].(string),
		}

		log.Printf("✅ Authenticated: %s (%s)", userClaims.Email, userClaims.Role)

		// Add claims to context
		ctx := context.WithValue(r.Context(), UserContextKey, userClaims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireRole middleware checks if user has required role (must be used after Auth)
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userClaims, ok := r.Context().Value(UserContextKey).(UserClaims)
			if !ok {
				log.Println("❌ User claims not found in context")
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			if userClaims.Role != role {
				log.Printf("❌ Insufficient permissions: required %s, got %s", role, userClaims.Role)
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// GetUserFromContext extracts user claims from request context
func GetUserFromContext(r *http.Request) (UserClaims, bool) {
	userClaims, ok := r.Context().Value(UserContextKey).(UserClaims)
	return userClaims, ok
}
