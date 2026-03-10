package services

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"strconv"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
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

	// Decode base64 credentials
	credentialsJSON, err := base64.StdEncoding.DecodeString(credentialsBase64)
	if err != nil {
		return nil, fmt.Errorf("error decoding base64 credentials: %w", err)
	}

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

	return &FCMService{client: client}, nil
}

// SendRouteAssignedNotification sends a notification when a route is assigned
func (s *FCMService) SendRouteAssignedNotification(token, routeID string, totalBins int) error {
	ctx := context.Background()

	// Data-only message — Flutter background handler creates the notification
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
			Payload: &messaging.APNSPayload{
				Aps: &messaging.Aps{
					ContentAvailable: true,
					MutableContent:   true,
					Sound:            "default",
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

	// Data-only message: no Notification field.
	// Android/iOS will NOT auto-display anything — the Flutter background
	// handler creates an awesome_notifications OS notification with our
	// custom channels, sound, and styling.
	message := &messaging.Message{
		Token: token,
		Data:  data,
		Android: &messaging.AndroidConfig{
			Priority: "high",
		},
		APNS: &messaging.APNSConfig{
			Payload: &messaging.APNSPayload{
				Aps: &messaging.Aps{
					ContentAvailable: true,
					MutableContent:   true,
					Sound:            "default",
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


// SendMulticast sends the same data-only message to multiple tokens.
// Flutter background handler creates the notification with custom channels.
func (s *FCMService) SendMulticast(tokens []string, title, body string, data map[string]string) error {
	ctx := context.Background()

	message := &messaging.MulticastMessage{
		Tokens: tokens,
		Data:   data,
		Android: &messaging.AndroidConfig{
			Priority: "high",
		},
		APNS: &messaging.APNSConfig{
			Payload: &messaging.APNSPayload{
				Aps: &messaging.Aps{
					ContentAvailable: true,
					MutableContent:   true,
					Sound:            "default",
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
