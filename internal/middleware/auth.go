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
		// log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		// log.Printf("🔐 AUTH MIDDLEWARE: %s %s", r.Method, r.URL.Path)

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			log.Println("❌ No authorization header")
			log.Printf("   Request headers: %v", r.Header)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// log.Printf("   Authorization header present: %s...", authHeader[:20])

		// Extract Bearer token
		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			log.Printf("❌ Invalid authorization header format (parts: %d)", len(parts))
			if len(parts) > 0 {
				log.Printf("   First part: %s", parts[0])
			}
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		tokenString := parts[1]
		// log.Printf("   Token extracted (length: %d)", len(tokenString))
		// log.Printf("   Token preview: %s...", tokenString[:20])

		// Get JWT secret
		jwtSecret := os.Getenv("APP_JWT_SECRET")
		if jwtSecret == "" {
			log.Println("❌ JWT secret not configured")
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		// log.Printf("   JWT secret configured: YES (length: %d)", len(jwtSecret))

		// Parse and validate token
		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			// Validate signing method
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				log.Printf("   ⚠️  Invalid signing method: %v", token.Method)
				return nil, jwt.ErrSignatureInvalid
			}
			// log.Printf("   ✓ Signing method valid: %v", token.Method)
			return []byte(jwtSecret), nil
		})

		if err != nil || !token.Valid {
			log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			log.Printf("❌ Invalid token: %v", err)
			if err != nil {
				log.Printf("   Error type: %T", err)
				log.Printf("   Error details: %+v", err)
			}
			log.Printf("   Token valid: %v", token.Valid)

			// DETAILED EXPIRATION DEBUGGING
			if claims, ok := token.Claims.(jwt.MapClaims); ok {
				log.Printf("🔍 TOKEN DETAILS:")

				// Get user info
				if email, ok := claims["email"].(string); ok {
					log.Printf("   User: %s", email)
				}
				if userID, ok := claims["user_id"].(string); ok {
					log.Printf("   User ID: %s", userID)
				}
				if role, ok := claims["role"].(string); ok {
					log.Printf("   Role: %s", role)
				}

				// Get token timing info
				now := jwt.NewNumericDate(time.Now())
				log.Printf("   Current server time: %s (unix: %d)", now.Time.Format(time.RFC3339), now.Unix())

				if iat, ok := claims["iat"].(float64); ok {
					iatTime := time.Unix(int64(iat), 0)
					log.Printf("   Token issued at (iat): %s (unix: %d)", iatTime.Format(time.RFC3339), int64(iat))
					log.Printf("   Token age: %s", time.Since(iatTime).Round(time.Second))
				}

				if exp, ok := claims["exp"].(float64); ok {
					expTime := time.Unix(int64(exp), 0)
					log.Printf("   Token expires at (exp): %s (unix: %d)", expTime.Format(time.RFC3339), int64(exp))

					if expTime.Before(time.Now()) {
						expiredDuration := time.Since(expTime).Round(time.Second)
						log.Printf("   ⏰ Token EXPIRED %s ago", expiredDuration)
					} else {
						validDuration := time.Until(expTime).Round(time.Second)
						log.Printf("   ⏰ Token still valid for %s", validDuration)
					}
				} else {
					log.Printf("   ⚠️  No expiration claim (exp) in token!")
				}
			}
			log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// log.Println("   ✓ Token is valid")

		// Extract claims
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			log.Println("❌ Failed to parse claims")
			log.Printf("   Claims type: %T", token.Claims)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// log.Printf("   ✓ Claims extracted: %v", claims)

		// Convert to UserClaims struct
		userClaims := UserClaims{
			UserID: claims["user_id"].(string),
			Email:  claims["email"].(string),
			Role:   claims["role"].(string),
		}

		// log.Printf("✅ Authenticated: %s (%s)", userClaims.Email, userClaims.Role)
		// log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

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
