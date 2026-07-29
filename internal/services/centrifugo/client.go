package centrifugo

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/centrifugal/gocent/v3"
	"github.com/golang-jwt/jwt/v5"
)

// Client wraps Centrifugo gocent client for real-time messaging
type Client struct {
	client          *gocent.Client
	tokenSecret     []byte
	tokenExpiration time.Duration
}

// NewClient creates a new Centrifugo client
func NewClient() (*Client, error) {
	apiURL := os.Getenv("CENTRIFUGO_API_URL")
	if apiURL == "" {
		apiURL = "https://binly-centrifugo-service-production.up.railway.app/api"
		log.Printf("⚠️  CENTRIFUGO_API_URL not set - using default: %s", apiURL)
	}

	apiKey := os.Getenv("CENTRIFUGO_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("CENTRIFUGO_API_KEY environment variable is required")
	}

	tokenSecret := os.Getenv("CENTRIFUGO_TOKEN_HMAC_SECRET_KEY")
	if tokenSecret == "" {
		return nil, fmt.Errorf("CENTRIFUGO_TOKEN_HMAC_SECRET_KEY environment variable is required")
	}

	// DISABLED: Verbose initialization logs
	// log.Printf("🔧 [Centrifugo] Initializing client - API URL: %s, API Key length: %d chars",
	// 	apiURL, len(apiKey))

	// Create gocent client
	client := gocent.New(gocent.Config{
		Addr: apiURL,
		Key:  apiKey,
	})

	// DISABLED: Verbose initialization logs
	// log.Printf("✅ [Centrifugo] Client initialized successfully")

	return &Client{
		client:          client,
		tokenSecret:     []byte(tokenSecret),
		tokenExpiration: 24 * time.Hour, // Default 24-hour token expiration
	}, nil
}

// PublishDriverLocation publishes driver location update to Centrifugo
func (c *Client) PublishDriverLocation(ctx context.Context, driverID string, location DriverLocation) error {
	channel := fmt.Sprintf("driver:location:%s", driverID)

	// Marshal data to JSON
	data, err := json.Marshal(location)
	if err != nil {
		return fmt.Errorf("failed to marshal location data: %w", err)
	}

	// DISABLED: Verbose publish logs (too frequent)
	// log.Printf("🔄 [Centrifugo] Attempting to publish to channel: %s", channel)

	result, err := c.client.Publish(ctx, channel, data)
	if err != nil {
		log.Printf("❌ [Centrifugo] Publish failed - Channel: %s, Error: %v", channel, err)
		return fmt.Errorf("failed to publish to channel %s: %w", channel, err)
	}

	// DISABLED: Verbose publish logs (too frequent)
	// log.Printf("✅ [Centrifugo] Successfully published to %s (offset: %d, epoch: %s)",
	// 	channel, result.Offset, result.Epoch)

	// Keep track of result to avoid unused variable warning
	_ = result

	return nil
}

// PublishShiftUpdate publishes shift update to Centrifugo
func (c *Client) PublishShiftUpdate(ctx context.Context, shiftID string, update interface{}) error {
	channel := fmt.Sprintf("shift:updates:%s", shiftID)

	// Marshal data to JSON
	data, err := json.Marshal(update)
	if err != nil {
		return fmt.Errorf("failed to marshal shift update data: %w", err)
	}

	result, err := c.client.Publish(ctx, channel, data)
	if err != nil {
		return fmt.Errorf("failed to publish to channel %s: %w", channel, err)
	}

	// DISABLED: Verbose publish logs
	// log.Printf("📡 [Centrifugo] Published shift update to %s (offset: %d)", channel, result.Offset)

	// Keep track of result to avoid unused variable warning
	_ = result

	return nil
}

// PublishToChannel publishes arbitrary data to a specific channel
func (c *Client) PublishToChannel(ctx context.Context, channel string, data interface{}) error {
	// Marshal data to JSON
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal data: %w", err)
	}

	result, err := c.client.Publish(ctx, channel, jsonData)
	if err != nil {
		return fmt.Errorf("failed to publish to channel %s: %w", channel, err)
	}

	// Keep track of result to avoid unused variable warning
	_ = result

	return nil
}

// PublishManagerNotification publishes notification to manager
func (c *Client) PublishManagerNotification(ctx context.Context, managerID string, notification interface{}) error {
	channel := fmt.Sprintf("manager:notifications:%s", managerID)

	// Marshal data to JSON
	data, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("failed to marshal notification data: %w", err)
	}

	result, err := c.client.Publish(ctx, channel, data)
	if err != nil {
		return fmt.Errorf("failed to publish to channel %s: %w", channel, err)
	}

	// DISABLED: Verbose publish logs
	// log.Printf("📡 [Centrifugo] Published manager notification to %s (offset: %d)", channel, result.Offset)

	// Keep track of result to avoid unused variable warning
	_ = result

	return nil
}

// PublishDriverEvent publishes a driver-specific event (e.g. shift_created, route_assigned)
// Channel: driver:events:{driverID}
// Used for events that target a specific driver BEFORE a shift/channel exists
func (c *Client) PublishDriverEvent(ctx context.Context, driverID string, eventType string, data interface{}) error {
	channel := fmt.Sprintf("driver:events:%s", driverID)

	payload := CompanyEvent{
		Type: eventType,
		Data: data,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal driver event data: %w", err)
	}

	result, err := c.client.Publish(ctx, channel, jsonData)
	if err != nil {
		log.Printf("❌ [Centrifugo] Publish failed - Channel: %s, Type: %s, Error: %v", channel, eventType, err)
		return fmt.Errorf("failed to publish to channel %s: %w", channel, err)
	}

	_ = result
	return nil
}

// CompanyEvent represents a company-wide broadcast event payload
type CompanyEvent struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// PublishCompanyEvent publishes a company-wide event to the company:events channel,
// which all authenticated managers subscribe to for real-time updates.
func (c *Client) PublishCompanyEvent(ctx context.Context, eventType string, data interface{}) error {
	channel := "company:events"

	payload := CompanyEvent{
		Type: eventType,
		Data: data,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal company event data: %w", err)
	}

	result, err := c.client.Publish(ctx, channel, jsonData)
	if err != nil {
		log.Printf("❌ [Centrifugo] Publish failed - Channel: %s, Type: %s, Error: %v", channel, eventType, err)
		return fmt.Errorf("failed to publish to channel %s: %w", channel, err)
	}

	_ = result
	return nil
}

// GenerateConnectionToken generates a JWT token for Centrifugo connection.
//
// orgID (the caller's organizations.id) rides in the token's `meta` claim as
// {"org_id": "..."}. Centrifugo attaches `meta` to the connection SERVER-SIDE
// (it is never exposed to the client) and forwards it to the backend proxy
// endpoints when the Centrifugo service is configured with
// proxy_include_connection_meta — that is how the subscribe/publish proxies
// (which carry no user JWT) learn which tenant they are authorizing for.
// Empty orgID (single-tenant dark mode) omits the claim entirely, keeping the
// token byte-identical to the pre-tenancy shape.
func (c *Client) GenerateConnectionToken(userID string, orgID string, expiresAt time.Time) (string, error) {
	claims := jwt.MapClaims{
		"sub": userID,                // User ID
		"exp": expiresAt.Unix(),      // Expiration time
		"iat": time.Now().Unix(),     // Issued at
		"info": map[string]interface{}{ // Optional user info
			"user_id": userID,
		},
	}
	if orgID != "" {
		claims["meta"] = map[string]interface{}{
			"org_id": orgID,
		}
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(c.tokenSecret)
	if err != nil {
		return "", fmt.Errorf("failed to sign JWT token: %w", err)
	}

	return tokenString, nil
}

// GenerateSubscriptionToken generates a JWT token for channel subscription
func (c *Client) GenerateSubscriptionToken(userID string, channel string, expiresAt time.Time) (string, error) {
	claims := jwt.MapClaims{
		"sub":     userID,           // User ID
		"channel": channel,          // Channel to subscribe to
		"exp":     expiresAt.Unix(), // Expiration time
		"iat":     time.Now().Unix(), // Issued at
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(c.tokenSecret)
	if err != nil {
		return "", fmt.Errorf("failed to sign subscription JWT token: %w", err)
	}

	return tokenString, nil
}

// DriverLocation represents a driver's real-time location.
// Keys MUST match what the Centrifugo publish proxy broadcasts
// ("latitude"/"longitude" — centrifugo_location_proxy.go): both clients
// parse that shape, and the old "lat"/"lng" tags meant every fix published
// through the HTTP path was silently dropped by their parsers.
type DriverLocation struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Heading   float64 `json:"heading,omitempty"`
	Speed     float64 `json:"speed,omitempty"`
	Accuracy  float64 `json:"accuracy,omitempty"`
	Timestamp int64   `json:"timestamp"`
}
