package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	fcmSendEndpoint = "https://fcm.googleapis.com/v1/projects/%s/messages:send"
	fcmScope        = "https://www.googleapis.com/auth/firebase.messaging"
)

// FCMService handles Firebase Cloud Messaging via direct HTTP v1 API.
// Bypasses the Firebase Admin SDK to avoid its internal transport auth issues.
type FCMService struct {
	tokenSource oauth2.TokenSource
	projectID   string
	httpClient  *http.Client
	db          *sqlx.DB // for cleaning up invalid tokens
}

// SetDB provides a database handle for automatic dead-token cleanup.
// Call this after construction in main.go.
// TENANCY NOTE: FCM keeps the RAW pool deliberately — dead-token cleanup deletes
// by the globally-unique token value (fcm_tokens.token is global per migration 6c,
// a device install is not a tenant). Under the non-superuser app role RLS makes
// these deletes inert (dead tokens accumulate, harmless); a system-role sweep can
// reclaim them later. Do NOT hand this service an org-bound handle.
func (s *FCMService) SetDB(db *sqlx.DB) {
	s.db = db
}

// ── FCM v1 API request/response types ──────────────────────────────────────

type fcmSendRequest struct {
	ValidateOnly bool       `json:"validate_only,omitempty"`
	Message      fcmMessage `json:"message"`
}

type fcmMessage struct {
	Token        string            `json:"token,omitempty"`
	Topic        string            `json:"topic,omitempty"`
	Notification *fcmNotification  `json:"notification,omitempty"`
	Data         map[string]string `json:"data,omitempty"`
	Android      *fcmAndroidConfig `json:"android,omitempty"`
	APNS         *fcmAPNSConfig    `json:"apns,omitempty"`
}

type fcmNotification struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

type fcmAndroidConfig struct {
	Priority string `json:"priority,omitempty"`
}

type fcmAPNSConfig struct {
	Headers map[string]string `json:"headers,omitempty"`
	Payload *fcmAPNSPayload   `json:"payload,omitempty"`
}

type fcmAPNSPayload struct {
	Aps *fcmAps `json:"aps,omitempty"`
}

type fcmAps struct {
	Alert          *fcmApsAlert `json:"alert,omitempty"`
	MutableContent int          `json:"mutable-content,omitempty"`
	Sound          string       `json:"sound,omitempty"`
}

type fcmApsAlert struct {
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	Body     string `json:"body,omitempty"`
}

type fcmSendResponse struct {
	Name  string       `json:"name"`
	Error *fcmAPIError `json:"error,omitempty"`
}

type fcmAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// ── Constructors ───────────────────────────────────────────────────────────

// NewFCMService creates a new FCM service from a credentials file.
func NewFCMService(credentialsFile string) (*FCMService, error) {
	ctx := context.Background()
	log.Printf("🔐 [FCM-INIT] Initializing from file: %s", credentialsFile)

	credentialsJSON, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("error reading credentials file: %w", err)
	}

	return newFCMServiceFromJSON(ctx, credentialsJSON)
}

// NewFCMServiceFromBase64 creates a new FCM service from base64-encoded credentials.
// Uses direct HTTP to the FCM v1 API with OAuth2 tokens from the service account.
func NewFCMServiceFromBase64(credentialsBase64 string) (*FCMService, error) {
	ctx := context.Background()

	log.Printf("🔐 [FCM-INIT] ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("🔐 [FCM-INIT] Initializing from base64 credentials (direct HTTP mode)")
	log.Printf("🔐 [FCM-INIT] Base64 input length: %d chars", len(credentialsBase64))

	credentialsJSON, err := base64.StdEncoding.DecodeString(credentialsBase64)
	if err != nil {
		log.Printf("❌ [FCM-INIT] Base64 decode FAILED: %v", err)
		return nil, fmt.Errorf("error decoding base64 credentials: %w", err)
	}

	log.Printf("🔐 [FCM-INIT] Decoded JSON length: %d bytes", len(credentialsJSON))

	return newFCMServiceFromJSON(ctx, credentialsJSON)
}

// newFCMServiceFromJSON is the shared init path for both constructors.
func newFCMServiceFromJSON(ctx context.Context, credentialsJSON []byte) (*FCMService, error) {
	// Validate JSON structure
	var creds map[string]interface{}
	if err := json.Unmarshal(credentialsJSON, &creds); err != nil {
		log.Printf("❌ [FCM-INIT] Credentials JSON is invalid: %v", err)
		return nil, fmt.Errorf("invalid credentials JSON: %w", err)
	}

	// Log credential fields for diagnostics
	for _, field := range []string{"type", "project_id", "private_key_id", "client_email", "token_uri"} {
		if v, ok := creds[field]; ok {
			log.Printf("🔐 [FCM-INIT] %s = %v", field, v)
		} else {
			log.Printf("❌ [FCM-INIT] MISSING field: %s", field)
		}
	}

	if pk, ok := creds["private_key"].(string); ok {
		hasBegin := strings.Contains(pk, "BEGIN PRIVATE KEY")
		hasEnd := strings.Contains(pk, "END PRIVATE KEY")
		log.Printf("🔐 [FCM-INIT] private_key: %d chars, BEGIN=%v, END=%v", len(pk), hasBegin, hasEnd)
	}

	projectID, _ := creds["project_id"].(string)
	if projectID == "" {
		return nil, fmt.Errorf("credentials JSON missing project_id")
	}

	// Create OAuth2 token source from service account credentials
	googleCreds, err := google.CredentialsFromJSON(ctx, credentialsJSON, fcmScope)
	if err != nil {
		log.Printf("❌ [FCM-INIT] Failed to create credentials: %v", err)
		return nil, fmt.Errorf("error creating Google credentials: %w", err)
	}

	tokenSource := googleCreds.TokenSource
	log.Printf("✅ [FCM-INIT] OAuth2 token source created")

	// Verify we can actually obtain a token
	token, err := tokenSource.Token()
	if err != nil {
		log.Printf("❌ [FCM-INIT] Failed to obtain initial token: %v", err)
		return nil, fmt.Errorf("error obtaining OAuth2 token: %w", err)
	}
	log.Printf("✅ [FCM-INIT] Initial token obtained (expires: %s)", token.Expiry.Format(time.RFC3339))

	svc := &FCMService{
		tokenSource: tokenSource,
		projectID:   projectID,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}

	// Validate with a dry-run send
	log.Printf("🔐 [FCM-INIT] Sending dry-run test...")
	testResp, testErr := svc.sendRaw(ctx, &fcmSendRequest{
		ValidateOnly: true,
		Message: fcmMessage{
			Topic: "__fcm_init_test__",
			Data:  map[string]string{"test": "1"},
		},
	})
	if testErr != nil {
		log.Printf("❌ [FCM-INIT] Dry-run FAILED: %v", testErr)
	} else {
		log.Printf("✅ [FCM-INIT] Dry-run PASSED: %s", testResp)
	}

	log.Printf("✅ [FCM-INIT] FCM service ready (project: %s, mode: direct HTTP)", projectID)
	log.Printf("🔐 [FCM-INIT] ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	return svc, nil
}

// ── Core send method ───────────────────────────────────────────────────────

// sendRaw sends a message to the FCM v1 HTTP API directly.
func (s *FCMService) sendRaw(ctx context.Context, req *fcmSendRequest) (string, error) {
	// Get fresh OAuth2 token
	token, err := s.tokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("failed to get OAuth2 token: %w", err)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf(fcmSendEndpoint, s.projectID)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+token.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json; UTF-8")

	log.Printf("📡 [FCM-HTTP] POST %s (token expires: %s, body: %d bytes, validateOnly: %v)",
		url, token.Expiry.Format(time.RFC3339), len(body), req.ValidateOnly)

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		// Log full response on one line for Railway visibility
		compactBody := strings.ReplaceAll(strings.ReplaceAll(string(respBody), "\n", " "), "  ", "")
		log.Printf("❌ [FCM-HTTP] Status %d | Full response: %s", resp.StatusCode, compactBody)
		return "", fmt.Errorf("FCM API returned status %d: %s", resp.StatusCode, compactBody)
	}

	var result fcmSendResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	log.Printf("✅ [FCM-HTTP] Success: %s", result.Name)
	return result.Name, nil
}

// send is the main entry point for sending a single message.
func (s *FCMService) send(ctx context.Context, msg fcmMessage) (string, error) {
	return s.sendRaw(ctx, &fcmSendRequest{Message: msg})
}

// ── Public API (unchanged signatures) ──────────────────────────────────────

// SendRouteAssignedNotification sends a notification when a route is assigned.
func (s *FCMService) SendRouteAssignedNotification(token, routeID string, totalBins int) error {
	ctx := context.Background()

	title := "New Route Assigned!"
	body := fmt.Sprintf("You have %d bins to collect today.", totalBins)

	msg := fcmMessage{
		Token: token,
		Data: map[string]string{
			"type":       "route_assigned",
			"route_id":   routeID,
			"total_bins": strconv.Itoa(totalBins),
			"subtitle":   "Route Update",
		},
		Android: &fcmAndroidConfig{Priority: "high"},
		APNS: &fcmAPNSConfig{
			Headers: map[string]string{
				"apns-push-type": "alert",
				"apns-priority":  "10",
			},
			Payload: &fcmAPNSPayload{
				Aps: &fcmAps{
					Alert:          &fcmApsAlert{Title: "Binly", Subtitle: title, Body: body},
					MutableContent: 1,
					Sound:          "default",
				},
			},
		},
	}

	log.Printf("📱 [FCM-SEND] Sending route_assigned to token: %s...%s", token[:min(10, len(token))], token[max(0, len(token)-6):])
	resp, err := s.send(ctx, msg)
	if err != nil {
		log.Printf("❌ [FCM-SEND] route_assigned FAILED: %v", err)
		return fmt.Errorf("error sending FCM message: %w", err)
	}

	log.Printf("✅ [FCM-SEND] route_assigned sent: %s", resp)
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

	title, body := ShiftNotificationText(eventType, extraData)

	msg := fcmMessage{
		Token:   token,
		Data:    data,
		Android: &fcmAndroidConfig{Priority: "high"},
		APNS: &fcmAPNSConfig{
			Headers: map[string]string{
				"apns-push-type": "alert",
				"apns-priority":  "10",
			},
			Payload: &fcmAPNSPayload{
				Aps: &fcmAps{
					Alert:          &fcmApsAlert{Title: "Binly", Subtitle: title, Body: body},
					MutableContent: 1,
					Sound:          "default",
				},
			},
		},
	}

	log.Printf("📱 [FCM-SEND] Sending %s to token: %s...%s (title: %q)", eventType, token[:min(10, len(token))], token[max(0, len(token)-6):], title)
	resp, err := s.send(ctx, msg)
	if err != nil {
		log.Printf("❌ [FCM-SEND] %s FAILED: %v", eventType, err)
		return fmt.Errorf("error sending FCM message: %w", err)
	}

	log.Printf("✅ [FCM-SEND] %s sent: %s", eventType, resp)
	return nil
}

// ShiftNotificationText returns iOS-visible title & body that match
// the mobile NotificationRegistry for the given event type.
// Exported so handlers can reuse the same text when calling CreateNotificationForUsers.
func ShiftNotificationText(eventType string, extraData map[string]string) (string, string) {
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
	case "route_assigned":
		return "New Route Assigned!", "You have a new route. Tap to view."
	default:
		return "Shift Update", "Your shift has been updated."
	}
}

// SendMulticast sends the same message to multiple tokens.
// FCM v1 API has no native multicast — sends individually to each token.
func (s *FCMService) SendMulticast(tokens []string, title, body string, data map[string]string) error {
	ctx := context.Background()

	log.Printf("📱 [FCM-SEND] Sending multicast to %d tokens (title: %q)", len(tokens), title)

	successCount := 0
	failureCount := 0

	for _, token := range tokens {
		msg := fcmMessage{
			Token:   token,
			Data:    data,
			Android: &fcmAndroidConfig{Priority: "high"},
			APNS: &fcmAPNSConfig{
				Headers: map[string]string{
					"apns-push-type": "alert",
					"apns-priority":  "10",
				},
				Payload: &fcmAPNSPayload{
					Aps: &fcmAps{
						Alert:          &fcmApsAlert{Title: "Binly", Subtitle: title, Body: body},
						MutableContent: 1,
						Sound:          "default",
					},
				},
			},
		}

		_, err := s.send(ctx, msg)
		if err != nil {
			errStr := err.Error()
			log.Printf("❌ [FCM-SEND] Multicast token %s...%s failed: %v",
				token[:min(10, len(token))], token[max(0, len(token)-6):], err)
			failureCount++

			// Auto-cleanup: delete tokens that FCM says are invalid/unregistered
			if s.db != nil && isTokenInvalidError(errStr) {
				log.Printf("🗑️  [FCM-SEND] Removing dead token %s...%s from database",
					token[:min(10, len(token))], token[max(0, len(token)-6):])
				_, delErr := s.db.Exec(`DELETE FROM fcm_tokens WHERE token = $1`, token)
				if delErr != nil {
					log.Printf("⚠️  [FCM-SEND] Failed to delete dead token: %v", delErr)
				}
			}
		} else {
			successCount++
		}
	}

	log.Printf("✅ [FCM-SEND] Multicast done: %d success, %d failures", successCount, failureCount)

	if failureCount > 0 && successCount == 0 {
		return fmt.Errorf("all %d multicast messages failed", failureCount)
	}
	return nil
}

// isTokenInvalidError returns true if the FCM error indicates
// the token is permanently invalid (unregistered, not found, etc.).
func isTokenInvalidError(errMsg string) bool {
	upper := strings.ToUpper(errMsg)
	return strings.Contains(upper, "NOT_FOUND") ||
		strings.Contains(upper, "UNREGISTERED") ||
		strings.Contains(upper, "INVALID_ARGUMENT")
}
