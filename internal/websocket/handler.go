package websocket

import (
	"log"
	"net/http"
	"os"

	"ropacal-backend/internal/middleware"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// Allow all origins in development
		// TODO: Restrict in production
		return true
	},
}

// HandleWebSocket upgrades HTTP connection to WebSocket
func HandleWebSocket(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Try to get token from query parameter first (for WebSocket connections)
		tokenString := r.URL.Query().Get("token")

		var userClaims middleware.UserClaims

		if tokenString != "" {
			// Validate token from query parameter
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
				log.Printf("❌ Invalid token in query parameter: %v", err)
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
			userClaims = middleware.UserClaims{
				UserID: claims["user_id"].(string),
				Email:  claims["email"].(string),
				Role:   claims["role"].(string),
			}
		} else {
			// Fallback: Get user from context (set by Auth middleware)
			var ok bool
			userClaims, ok = middleware.GetUserFromContext(r)
			if !ok {
				log.Println("❌ No user in context for WebSocket connection")
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}

		// Upgrade HTTP connection to WebSocket
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("❌ WebSocket upgrade failed: %v", err)
			return
		}

		// Create client
		client := NewClient(userClaims.UserID, conn, hub)

		// Register client
		hub.register <- client

		// Start pumps in separate goroutines
		go client.WritePump()
		go client.ReadPump()

		log.Printf("✅ WebSocket connection established for user: %s (%s)", userClaims.Email, userClaims.UserID)
	}
}
