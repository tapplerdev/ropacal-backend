package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"google.golang.org/api/option"
)

// FCMService handles Firebase Cloud Messaging
type FCMService struct {
	client    *messaging.Client
	projectID string
}

// NewFCMService creates a new FCM service instance from a credentials file
func NewFCMService(credentialsFile string) (*FCMService, error) {
	ctx := context.Background()

	log.Printf("🔐 [FCM-INIT] Initializing from file: %s", credentialsFile)

	opt := option.WithCredentialsFile(credentialsFile)
	app, err := firebase.NewApp(ctx, nil, opt)
	if err != nil {
		log.Printf("❌ [FCM-INIT] firebase.NewApp failed: %v", err)
		return nil, fmt.Errorf("error initializing Firebase app: %w", err)
	}

	client, err := app.Messaging(ctx)
	if err != nil {
		log.Printf("❌ [FCM-INIT] app.Messaging failed: %v", err)
		return nil, fmt.Errorf("error getting messaging client: %w", err)
	}

	log.Printf("✅ [FCM-INIT] Firebase Messaging client ready (from file)")
	return &FCMService{client: client}, nil
}

// NewFCMServiceFromBase64 creates a new FCM service instance from base64-encoded credentials.
// Uses GOOGLE_APPLICATION_CREDENTIALS (Application Default Credentials) — the most standard
// and battle-tested credential path in all Google Cloud SDKs.
func NewFCMServiceFromBase64(credentialsBase64 string) (*FCMService, error) {
	ctx := context.Background()

	log.Printf("🔐 [FCM-INIT] ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("🔐 [FCM-INIT] Initializing from base64 credentials")
	log.Printf("🔐 [FCM-INIT] Base64 input length: %d chars", len(credentialsBase64))

	// Decode base64 credentials
	credentialsJSON, err := base64.StdEncoding.DecodeString(credentialsBase64)
	if err != nil {
		log.Printf("❌ [FCM-INIT] Base64 decode FAILED: %v", err)
		return nil, fmt.Errorf("error decoding base64 credentials: %w", err)
	}

	log.Printf("🔐 [FCM-INIT] Decoded JSON length: %d bytes", len(credentialsJSON))

	// Validate the decoded JSON structure
	var creds map[string]interface{}
	if jsonErr := json.Unmarshal(credentialsJSON, &creds); jsonErr != nil {
		log.Printf("❌ [FCM-INIT] Decoded bytes are NOT valid JSON: %v", jsonErr)
		return nil, fmt.Errorf("decoded credentials JSON is invalid: %w", jsonErr)
	}

	// Log all fields (except private_key value)
	for _, field := range []string{"type", "project_id", "private_key_id", "client_email", "token_uri"} {
		if v, ok := creds[field]; ok {
			log.Printf("🔐 [FCM-INIT] %s = %v", field, v)
		} else {
			log.Printf("❌ [FCM-INIT] MISSING required field: %s", field)
		}
	}

	// Validate private_key integrity
	if pk, ok := creds["private_key"].(string); ok {
		hasBegin := strings.Contains(pk, "BEGIN PRIVATE KEY")
		hasEnd := strings.Contains(pk, "END PRIVATE KEY")
		log.Printf("🔐 [FCM-INIT] private_key: %d chars, BEGIN=%v, END=%v", len(pk), hasBegin, hasEnd)
	} else {
		log.Printf("❌ [FCM-INIT] private_key is MISSING or not a string!")
	}

	projectID, _ := creds["project_id"].(string)

	// Write credentials to a temp file and use GOOGLE_APPLICATION_CREDENTIALS.
	// This is Google's recommended approach for cloud deployments — uses the
	// Application Default Credentials (ADC) path, the most tested credential
	// flow in all Google SDKs.
	tmpFile, err := os.CreateTemp("", "firebase-creds-*.json")
	if err != nil {
		log.Printf("❌ [FCM-INIT] Failed to create temp file: %v", err)
		return nil, fmt.Errorf("error creating temp credentials file: %w", err)
	}

	if _, err := tmpFile.Write(credentialsJSON); err != nil {
		log.Printf("❌ [FCM-INIT] Failed to write temp file: %v", err)
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return nil, fmt.Errorf("error writing credentials to temp file: %w", err)
	}
	tmpFile.Close()

	log.Printf("🔐 [FCM-INIT] Wrote credentials to temp file: %s", tmpFile.Name())

	// Set GOOGLE_APPLICATION_CREDENTIALS so the SDK uses ADC
	os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", tmpFile.Name())
	log.Printf("🔐 [FCM-INIT] Set GOOGLE_APPLICATION_CREDENTIALS=%s", tmpFile.Name())

	// Initialize Firebase app with default credentials (reads from GOOGLE_APPLICATION_CREDENTIALS)
	log.Printf("🔐 [FCM-INIT] Calling firebase.NewApp with Application Default Credentials...")
	app, err := firebase.NewApp(ctx, nil)
	if err != nil {
		log.Printf("❌ [FCM-INIT] firebase.NewApp FAILED: %v", err)
		return nil, fmt.Errorf("error initializing Firebase app: %w", err)
	}
	log.Printf("✅ [FCM-INIT] firebase.NewApp succeeded")

	// Get messaging client
	log.Printf("🔐 [FCM-INIT] Calling app.Messaging...")
	client, err := app.Messaging(ctx)
	if err != nil {
		log.Printf("❌ [FCM-INIT] app.Messaging FAILED: %v", err)
		return nil, fmt.Errorf("error getting messaging client: %w", err)
	}
	log.Printf("✅ [FCM-INIT] Messaging client created")

	// Send a dry-run test message to verify credentials work end-to-end
	log.Printf("🔐 [FCM-INIT] Sending dry-run test to validate credentials...")
	testMsg := &messaging.Message{
		Topic: "__fcm_init_test__",
		Data:  map[string]string{"test": "1"},
	}
	testResp, testErr := client.SendDryRun(ctx, testMsg)
	if testErr != nil {
		log.Printf("❌ [FCM-INIT] Dry-run FAILED: %v (type: %T)", testErr, testErr)
	} else {
		log.Printf("✅ [FCM-INIT] Dry-run PASSED: %s", testResp)
	}

	log.Printf("✅ [FCM-INIT] Firebase Messaging client ready (project: %s, credential path: ADC)", projectID)
	log.Printf("🔐 [FCM-INIT] ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	return &FCMService{client: client, projectID: projectID}, nil
}

// SendRouteAssignedNotification sends a notification when a route is assigned
func (s *FCMService) SendRouteAssignedNotification(token, routeID string, totalBins int) error {
	ctx := context.Background()

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

	log.Printf("📱 [FCM-SEND] Sending route_assigned to token: %s...%s", token[:min(10, len(token))], token[max(0, len(token)-6):])
	response, err := s.client.Send(ctx, message)
	if err != nil {
		log.Printf("❌ [FCM-SEND] route_assigned FAILED: %v (error type: %T)", err, err)
		return fmt.Errorf("error sending FCM message: %w", err)
	}

	log.Printf("✅ [FCM-SEND] route_assigned sent: %s", response)
	return nil
}

// SendShiftUpdateNotification sends a notification for shift updates.
func (s *FCMService) SendShiftUpdateNotification(token, shiftID, eventType string, extraData map[string]string) error {
	ctx := context.Background()

	data := map[string]string{
		"type":     eventType,
		"shift_id": shiftID,
	}
	for k, v := range extraData {
		data[k] = v
	}

	title, body := shiftNotificationText(eventType, extraData)

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

	log.Printf("📱 [FCM-SEND] Sending %s to token: %s...%s (title: %q)", eventType, token[:min(10, len(token))], token[max(0, len(token)-6):], title)
	response, err := s.client.Send(ctx, message)
	if err != nil {
		log.Printf("❌ [FCM-SEND] %s FAILED: %v (error type: %T)", eventType, err, err)
		return fmt.Errorf("error sending FCM message: %w", err)
	}

	log.Printf("✅ [FCM-SEND] %s sent: %s", eventType, response)
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

	log.Printf("📱 [FCM-SEND] Sending multicast to %d tokens (title: %q)", len(tokens), title)
	response, err := s.client.SendEachForMulticast(ctx, message)
	if err != nil {
		log.Printf("❌ [FCM-SEND] Multicast FAILED: %v (error type: %T)", err, err)
		return fmt.Errorf("error sending multicast message: %w", err)
	}

	log.Printf("✅ [FCM-SEND] Multicast sent: %d success, %d failures", response.SuccessCount, response.FailureCount)
	return nil
}
