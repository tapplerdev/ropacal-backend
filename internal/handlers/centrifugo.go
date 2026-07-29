package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/orgdb"
	"ropacal-backend/internal/services/centrifugo"

	"github.com/jmoiron/sqlx"
)

// CentrifugoSubscribeRequest represents the subscribe proxy request from Centrifugo
type CentrifugoSubscribeRequest struct {
	ClientID  string                 `json:"client"`
	Transport string                 `json:"transport"`
	Protocol  string                 `json:"protocol"`
	Encoding  string                 `json:"encoding"`
	User      string                 `json:"user"`
	Channel   string                 `json:"channel"`
	Token     string                 `json:"token,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// CentrifugoPublishRequest represents the publish proxy request from Centrifugo
type CentrifugoPublishRequest struct {
	ClientID  string                 `json:"client"`
	Transport string                 `json:"transport"`
	Protocol  string                 `json:"protocol"`
	Encoding  string                 `json:"encoding"`
	User      string                 `json:"user"`
	Channel   string                 `json:"channel"`
	Data      map[string]interface{} `json:"data"`
}

// CentrifugoSubscribeResponse represents the subscribe proxy response
type CentrifugoSubscribeResponse struct {
	Result *CentrifugoSubscribeResult `json:"result,omitempty"`
	Error  *CentrifugoError           `json:"error,omitempty"`
}

// CentrifugoSubscribeResult represents successful subscription authorization
type CentrifugoSubscribeResult struct {
	// B64Data can be used to pass custom data to client
	B64Data string `json:"b64data,omitempty"`
	// ExpireAt is optional subscription expiration time (unix timestamp)
	ExpireAt int64 `json:"expire_at,omitempty"`
	// Info is optional JSON with subscription info
	Info map[string]interface{} `json:"info,omitempty"`
}

// CentrifugoPublishResponse represents the publish proxy response
type CentrifugoPublishResponse struct {
	Result *CentrifugoPublishResult `json:"result,omitempty"`
	Error  *CentrifugoError         `json:"error,omitempty"`
}

// CentrifugoPublishResult represents successful publication authorization
// Can include modified data to publish instead of original
type CentrifugoPublishResult struct {
	// Data is the modified data to publish (if we want to change it)
	Data map[string]interface{} `json:"data,omitempty"`
	// SkipHistory tells Centrifugo not to save this to channel history
	SkipHistory bool `json:"skip_history,omitempty"`
}

// CentrifugoError represents an error response
type CentrifugoError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// TENANCY NOTE: this file deliberately keeps the raw *sqlx.DB pool (no
// orgdb shadow). Its endpoints carry no user JWT (server-to-server /
// INTERNAL_API_KEY paths), so there is no organization to bind — under RLS
// with a non-superuser role their queries fail closed (zero rows / NOT NULL
// on insert) rather than crossing tenants. Scoping these paths needs a
// caller-identity -> org mapping first (see the tenancy workers audit,
// sections 4E/4F) and is tracked as follow-up work; do NOT "fix" them by
// adding an unscoped bypass inside orgdb.

// CentrifugoSubscribeProxy handles subscription authorization from Centrifugo
func CentrifugoSubscribeProxy(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CentrifugoSubscribeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ [Centrifugo] Invalid subscribe request: %v", err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoSubscribeResponse{
				Error: &CentrifugoError{
					Code:    400,
					Message: "invalid request",
				},
			})
			return
		}

		// req.User is the identity Centrifugo resolved from the client's
		// authenticated connection token. It is trustworthy ONLY because the
		// CentrifugoProxyAuth middleware verified this request came from the
		// Centrifugo server itself (shared proxy secret) — never trust it on
		// an unguarded route.
		userID := req.User

		log.Printf("🔐 [Centrifugo] Subscribe request: user=%s channel=%s client=%s",
			userID, req.Channel, req.ClientID)

		// Authorize subscription based on channel type
		authorized, err := authorizeSubscription(db, userID, req.Channel)
		if err != nil {
			log.Printf("❌ [Centrifugo] Authorization error for user=%s channel=%s: %v",
				userID, req.Channel, err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoSubscribeResponse{
				Error: &CentrifugoError{
					Code:    403,
					Message: "authorization failed",
				},
			})
			return
		}

		if !authorized {
			log.Printf("🚫 [Centrifugo] Subscription denied: user=%s channel=%s",
				userID, req.Channel)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoSubscribeResponse{
				Error: &CentrifugoError{
					Code:    403,
					Message: "permission denied",
				},
			})
			return
		}

		log.Printf("✅ [Centrifugo] Subscription authorized: user=%s channel=%s",
			userID, req.Channel)

		// Return successful authorization
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CentrifugoSubscribeResponse{
			Result: &CentrifugoSubscribeResult{
				Info: map[string]interface{}{
					"user_id": userID,
					"channel": req.Channel,
				},
			},
		})
	}
}

// authorizeSubscription checks if a user is allowed to subscribe to a channel
func authorizeSubscription(db *sqlx.DB, userID string, channel string) (bool, error) {
	// Parse channel to determine type and resource ID
	parts := strings.Split(channel, ":")
	if len(parts) < 2 {
		return false, fmt.Errorf("invalid channel format: %s", channel)
	}

	namespace := parts[0]   // driver, shift, manager
	channelType := parts[1] // location, updates, notifications

	switch namespace {
	case "driver":
		// Channel format: driver:location:{driverId}
		if channelType == "location" && len(parts) == 3 {
			driverID := parts[2]
			// Check if user is the driver themselves OR a manager
			return canViewDriverLocation(db, userID, driverID)
		}
		// Channel format: driver:events:{driverId}
		// Only the driver themselves can subscribe to their own events
		if channelType == "events" && len(parts) == 3 {
			driverID := parts[2]
			return userID == driverID, nil
		}

	case "shift":
		// Channel format: shift:updates:{shiftId}
		if channelType == "updates" && len(parts) == 3 {
			shiftID := parts[2]
			// Check if user is assigned to this shift OR is a manager
			return canViewShift(db, userID, shiftID)
		}

	case "manager":
		// Channel format: manager:notifications:{managerId}
		if channelType == "notifications" && len(parts) == 3 {
			managerID := parts[2]
			// Only the manager themselves can subscribe
			return userID == managerID, nil
		}

	case "company":
		// Channel format: company:events
		// Only admins and managers can subscribe to company-wide broadcast events
		if channelType == "events" {
			var role string
			err := db.Get(&role, `SELECT role FROM users WHERE id = $1`, userID)
			if err == sql.ErrNoRows {
				return false, nil
			}
			if err != nil {
				return false, fmt.Errorf("failed to get user role: %w", err)
			}
			return role == "admin" || role == "manager", nil
		}
	}

	return false, fmt.Errorf("unknown channel format: %s", channel)
}

// canViewDriverLocation checks if user can view a driver's location
func canViewDriverLocation(db *sqlx.DB, userID string, driverID string) (bool, error) {
	// Allow if user is the driver themselves
	if userID == driverID {
		return true, nil
	}

	// Check if user is a manager (has manager role)
	var role string
	err := db.Get(&role, `SELECT role FROM users WHERE id = $1`, userID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get user role: %w", err)
	}

	// Managers can view all driver locations
	return role == "admin" || role == "manager", nil
}

// canViewShift checks if user can view shift updates
func canViewShift(db *sqlx.DB, userID string, shiftID string) (bool, error) {
	// Check if user is assigned to this shift
	var driverID sql.NullString
	err := db.Get(&driverID, `SELECT driver_id FROM shifts WHERE id = $1`, shiftID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get shift: %w", err)
	}

	// Allow if user is assigned to this shift
	if driverID.Valid && driverID.String == userID {
		return true, nil
	}

	// Check if user is a manager
	var role string
	err = db.Get(&role, `SELECT role FROM users WHERE id = $1`, userID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get user role: %w", err)
	}

	return role == "admin" || role == "manager", nil
}

// CentrifugoPublishProxy handles publication authorization from Centrifugo
func CentrifugoPublishProxy(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CentrifugoPublishRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ [Centrifugo] Invalid publish request: %v", err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    400,
					Message: "invalid request",
				},
			})
			return
		}

		// Same identity rule as the subscribe proxy: req.User comes from the
		// client's authenticated connection token and is trustworthy only
		// behind the CentrifugoProxyAuth shared-secret guard.
		userID := req.User

		log.Printf("🔐 [Centrifugo] Publish request: user=%s channel=%s client=%s",
			userID, req.Channel, req.ClientID)

		// Authorize publication based on channel type
		authorized, err := authorizePublication(db, userID, req.Channel)
		if err != nil {
			log.Printf("❌ [Centrifugo] Authorization error for user=%s channel=%s: %v",
				userID, req.Channel, err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    403,
					Message: "authorization failed",
				},
			})
			return
		}

		if !authorized {
			log.Printf("🚫 [Centrifugo] Publication denied: user=%s channel=%s",
				userID, req.Channel)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    403,
					Message: "permission denied",
				},
			})
			return
		}

		log.Printf("✅ [Centrifugo] Publication authorized: user=%s channel=%s",
			userID, req.Channel)

		// Return successful authorization
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CentrifugoPublishResponse{
			Result: &CentrifugoPublishResult{},
		})
	}
}

// authorizePublication checks if a user is allowed to publish to a channel
func authorizePublication(db *sqlx.DB, userID string, channel string) (bool, error) {
	// Parse channel to determine type and resource ID
	parts := strings.Split(channel, ":")
	if len(parts) < 2 {
		return false, fmt.Errorf("invalid channel format: %s", channel)
	}

	namespace := parts[0]   // driver, shift, manager
	channelType := parts[1] // location, updates, notifications

	switch namespace {
	case "driver":
		// Channel format: driver:location:{driverId}
		if channelType == "location" && len(parts) == 3 {
			driverID := parts[2]
			// Only the driver themselves can publish to their own location channel
			return userID == driverID, nil
		}

	case "shift":
		// Drivers cannot publish to shift channels (backend only)
		return false, nil

	case "manager":
		// Drivers cannot publish to manager channels (backend only)
		return false, nil
	}

	return false, fmt.Errorf("unknown channel format: %s", channel)
}

// CentrifugoConnectionTokenResponse represents token response
type CentrifugoConnectionTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// GetCentrifugoToken generates a connection token for the authenticated user
func GetCentrifugoToken(centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get user claims from context (set by auth middleware)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		userID := userClaims.UserID

		// The caller's organization rides in the Centrifugo token's `meta`
		// claim so the proxy endpoints (server-to-server, no user JWT) can
		// re-establish tenant scope. Empty while tenancy is dark (the request
		// handle is a passthrough) → the claim is omitted and the token is
		// byte-identical to the pre-tenancy shape.
		orgID := orgdb.From(r).OrgID()

		// Generate token valid for 24 hours
		expiresAt := time.Now().Add(24 * time.Hour)
		token, err := centrifugoClient.GenerateConnectionToken(userID, orgID, expiresAt)
		if err != nil {
			log.Printf("❌ [Centrifugo] Failed to generate token for user %s: %v", userID, err)
			http.Error(w, "failed to generate token", http.StatusInternalServerError)
			return
		}

		log.Printf("🔑 [Centrifugo] Generated connection token for user %s (expires: %s)",
			userID, expiresAt.Format(time.RFC3339))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CentrifugoConnectionTokenResponse{
			Token:     token,
			ExpiresAt: expiresAt.Unix(),
		})
	}
}
