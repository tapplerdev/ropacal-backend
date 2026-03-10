package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
)

// FCMService handles Firebase Cloud Messaging
type FCMService struct {
	client *messaging.Client
}

// NewFCMService creates a new FCM service instance from a credentials file
func NewFCMService(credentialsFile string) (*FCMService, error) {
	ctx := context.Background()

	// Initialize Firebase app
	opt := option.WithCredentialsFile(credentialsFile)
	app, err := firebase.NewApp(ctx, nil, opt)
	if err != nil {
		return nil, fmt.Errorf("error initializing Firebase app: %w", err)
	}

	// Get messaging client
	client, err := app.Messaging(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting messaging client: %w", err)
	}

	return &FCMService{client: client}, nil
}

// NewFCMServiceFromBase64 creates a new FCM service instance from base64-encoded credentials
// This is useful for cloud deployments (Railway, Fly.io, Render) where you can't upload files easily
func NewFCMServiceFromBase64(credentialsBase64 string) (*FCMService, error) {
	ctx := context.Background()

	log.Printf("🔐 [FCM-INIT] Base64 input length: %d chars", len(credentialsBase64))

	// Decode base64 credentials
	credentialsJSON, err := base64.StdEncoding.DecodeString(credentialsBase64)
	if err != nil {
		log.Printf("❌ [FCM-INIT] Base64 decode failed: %v", err)
		return nil, fmt.Errorf("error decoding base64 credentials: %w", err)
	}

	log.Printf("🔐 [FCM-INIT] Decoded JSON length: %d bytes", len(credentialsJSON))

	// Validate the decoded JSON structure
	var creds map[string]interface{}
	if jsonErr := json.Unmarshal(credentialsJSON, &creds); jsonErr != nil {
		log.Printf("❌ [FCM-INIT] Decoded JSON is INVALID: %v", jsonErr)
		return nil, fmt.Errorf("decoded credentials JSON is invalid: %w", jsonErr)
	}

	for _, field := range []string{"type", "project_id", "private_key_id", "client_email", "token_uri"} {
		if v, ok := creds[field]; ok {
			log.Printf("🔐 [FCM-INIT] %s = %v", field, v)
		} else {
			log.Printf("❌ [FCM-INIT] MISSING field: %s", field)
		}
	}

	// Validate private_key
	if pk, ok := creds["private_key"].(string); ok {
		log.Printf("🔐 [FCM-INIT] private_key length: %d chars", len(pk))
		log.Printf("🔐 [FCM-INIT] private_key starts with: %s", pk[:min(40, len(pk))])
		log.Printf("🔐 [FCM-INIT] private_key ends with: %s", pk[max(0, len(pk)-30):])
		if !strings.Contains(pk, "BEGIN PRIVATE KEY") {
			log.Printf("❌ [FCM-INIT] private_key does NOT contain 'BEGIN PRIVATE KEY' — likely corrupted!")
		}
		if !strings.Contains(pk, "END PRIVATE KEY") {
			log.Printf("❌ [FCM-INIT] private_key does NOT contain 'END PRIVATE KEY' — likely truncated!")
		}
	} else {
		log.Printf("❌ [FCM-INIT] private_key is MISSING or not a string!")
	}

	// Test OAuth2 token exchange BEFORE initializing Firebase
	// This catches credential issues immediately instead of failing on first Send()
	conf, err := google.JWTConfigFromJSON(credentialsJSON, "https://www.googleapis.com/auth/firebase.messaging")
	if err != nil {
		log.Printf("❌ [FCM-INIT] Failed to parse JWT config from credentials: %v", err)
		return nil, fmt.Errorf("invalid service account credentials: %w", err)
	}
	log.Printf("🔐 [FCM-INIT] JWT config parsed — requesting OAuth2 token from %s...", conf.TokenURL)

	token, err := conf.TokenSource(ctx).Token()
	if err != nil {
		log.Printf("❌ [FCM-INIT] OAuth2 token exchange FAILED: %v", err)
		return nil, fmt.Errorf("OAuth2 token exchange failed (credentials may be revoked): %w", err)
	}
	log.Printf("✅ [FCM-INIT] OAuth2 token obtained! Type: %s, Expires: %v", token.TokenType, token.Expiry)

	// Initialize Firebase app with JSON credentials
	opt := option.WithCredentialsJSON(credentialsJSON)
	app, err := firebase.NewApp(ctx, nil, opt)
	if err != nil {
		return nil, fmt.Errorf("error initializing Firebase app: %w", err)
	}

	// Get messaging client
	client, err := app.Messaging(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting messaging client: %w", err)
	}

	log.Printf("✅ [FCM-INIT] Firebase Messaging client ready (OAuth2 credentials verified)")
	return &FCMService{client: client}, nil
}

// SendRouteAssignedNotification sends a notification when a route is assigned
func (s *FCMService) SendRouteAssignedNotification(token, routeID string, totalBins int) error {
	ctx := context.Background()

	// Data payload for Flutter foreground handler + Android background handler
	// APNS Alert ensures iOS displays the notification reliably (data-only
	// messages are treated as silent pushes and throttled by Apple).
	message := &messaging.Message{
		Token: token,
		Data: map[string]string{
			"type":       "route_assigned",
			"route_id":   routeID,
			"total_bins": strconv.Itoa(totalBins),
		},
		Android: &messaging.AndroidConfig{
			Priority: "high",
		},
		APNS: &messaging.APNSConfig{
			Headers: map[string]string{
				"apns-push-type": "alert",
				"apns-priority":  "10",
			},
			Payload: &messaging.APNSPayload{
				Aps: &messaging.Aps{
					Alert: &messaging.ApsAlert{
						Title: "New Route Assigned!",
						Body:  fmt.Sprintf("You have %d bins to collect today.", totalBins),
					},
					MutableContent: true,
					Sound:          "default",
				},
			},
		},
	}

	response, err := s.client.Send(ctx, message)
	if err != nil {
		return fmt.Errorf("error sending FCM message: %w", err)
	}

	log.Printf("✅ FCM data message sent for route_assigned: %s", response)
	return nil
}

// SendShiftUpdateNotification sends a notification for shift updates.
// eventType should match the mobile registry key (e.g. "shift_cancelled",
// "shift_created", "shift_reassigned"). It is passed through as Data["type"]
// so the mobile adapter creates the correct NotificationEvent.
func (s *FCMService) SendShiftUpdateNotification(token, shiftID, eventType string, extraData map[string]string) error {
	ctx := context.Background()

	data := map[string]string{
		"type":     eventType,
		"shift_id": shiftID,
	}
	for k, v := range extraData {
		data[k] = v
	}

	// APNS Alert text — matches the mobile NotificationRegistry titles/bodies.
	title, body := shiftNotificationText(eventType, extraData)

	// Data payload for Flutter foreground + Android background handler.
	// APNS Alert ensures iOS displays reliably (data-only = silent push,
	// throttled by Apple). Android ignores APNS config.
	message := &messaging.Message{
		Token: token,
		Data:  data,
		Android: &messaging.AndroidConfig{
			Priority: "high",
		},
		APNS: &messaging.APNSConfig{
			Headers: map[string]string{
				"apns-push-type": "alert",
				"apns-priority":  "10",
			},
			Payload: &messaging.APNSPayload{
				Aps: &messaging.Aps{
					Alert: &messaging.ApsAlert{
						Title: title,
						Body:  body,
					},
					MutableContent: true,
					Sound:          "default",
				},
			},
		},
	}

	response, err := s.client.Send(ctx, message)
	if err != nil {
		return fmt.Errorf("error sending FCM message: %w", err)
	}

	log.Printf("✅ FCM data message sent for %s: %s", eventType, response)
	return nil
}

// shiftNotificationText returns iOS-visible title & body that match
// the mobile NotificationRegistry for the given event type.
func shiftNotificationText(eventType string, extraData map[string]string) (string, string) {
	switch eventType {
	case "shift_created":
		return "New Shift Assigned", "You have a new shift. Tap to view."
	case "shift_cancelled":
		if cb := extraData["cancelled_by"]; cb != "" {
			return "Shift Cancelled", fmt.Sprintf("Your shift has been cancelled by %s.", cb)
		}
		return "Shift Cancelled", "Your shift has been cancelled by management."
	case "shift_reassigned":
		return "Shift Reassigned", "Your shift has been reassigned to another driver."
	case "shift_deleted":
		return "Shift Cleared", "Your shift has been cleared by management."
	case "move_request_assigned":
		if bn := extraData["bin_number"]; bn != "" {
			return fmt.Sprintf("Move Request: Bin #%s", bn), "You have a new move request. Tap to view."
		}
		return "New Move Request Assigned", "You have a new move request. Tap to view."
	default:
		return "Shift Update", "Your shift has been updated."
	}
}


// SendMulticast sends the same message to multiple tokens.
// APNS Alert ensures iOS displays reliably; Android uses background handler.
func (s *FCMService) SendMulticast(tokens []string, title, body string, data map[string]string) error {
	ctx := context.Background()

	message := &messaging.MulticastMessage{
		Tokens: tokens,
		Data:   data,
		Android: &messaging.AndroidConfig{
			Priority: "high",
		},
		APNS: &messaging.APNSConfig{
			Headers: map[string]string{
				"apns-push-type": "alert",
				"apns-priority":  "10",
			},
			Payload: &messaging.APNSPayload{
				Aps: &messaging.Aps{
					Alert: &messaging.ApsAlert{
						Title: title,
						Body:  body,
					},
					MutableContent: true,
					Sound:          "default",
				},
			},
		},
	}

	response, err := s.client.SendEachForMulticast(ctx, message)
	if err != nil {
		return fmt.Errorf("error sending multicast message: %w", err)
	}

	log.Printf("✅ Multicast sent: %d success, %d failures", response.SuccessCount, response.FailureCount)
	return nil
}
