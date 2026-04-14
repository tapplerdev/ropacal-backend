package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"ropacal-backend/internal/services/centrifugo"

	"github.com/jmoiron/sqlx"
)

// AirtagMonitor periodically checks AirTag positions against stored bin locations.
// If a bin has drifted >500m and is NOT in transit, it alerts admins via Centrifugo + FCM.
type AirtagMonitor struct {
	db               *sqlx.DB
	fcmService       *FCMService
	centrifugoClient *centrifugo.Client
	bridgeURL        string
	ticker           *time.Ticker
	stopChan         chan bool
}

// AirtagAPIResponse is the response from the FindMy bridge /api/airtag-locations endpoint.
type AirtagAPIResponse struct {
	Data     []AirtagEntry `json:"data"`
	Count    int           `json:"count"`
	LastSync *string       `json:"last_sync_at"`
}

// AirtagEntry represents a single AirTag location from the FindMy bridge.
type AirtagEntry struct {
	ID            string  `json:"id"`
	BinNumber     int     `json:"bin_number"`
	Name          string  `json:"name"`
	Latitude      float64 `json:"latitude"`
	Longitude     float64 `json:"longitude"`
	Address       string  `json:"address"`
	City          string  `json:"city"`
	LastSeen      string  `json:"last_seen"`
	BatteryStatus int     `json:"battery_status"` // 0=Full, 1=Medium, 2=Low, 3=Critical
}

// FetchAirtagLocations fetches AirTag locations from the FindMy bridge.
// Shared by AirtagMonitor, DigestScheduler, and the proxy handler.
func FetchAirtagLocations(bridgeURL string) (*AirtagAPIResponse, error) {
	url := bridgeURL + "/api/airtag-locations"

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("bridge returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var apiResp AirtagAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &apiResp, nil
}

type binRow struct {
	ID            string   `db:"id"`
	BinNumber     int      `db:"bin_number"`
	Latitude      *float64 `db:"latitude"`
	Longitude     *float64 `db:"longitude"`
	Status        string   `db:"status"`
	CurrentStreet string   `db:"current_street"`
	City          string   `db:"city"`
}

const defaultDriftThresholdMeters = 500.0

// NewAirtagMonitor creates a new monitor that checks every 5 minutes.
func NewAirtagMonitor(db *sqlx.DB, fcmService *FCMService, centrifugoClient *centrifugo.Client) *AirtagMonitor {
	bridgeURL := os.Getenv("FINDMY_BRIDGE_URL")
	return &AirtagMonitor{
		db:               db,
		fcmService:       fcmService,
		centrifugoClient: centrifugoClient,
		bridgeURL:        bridgeURL,
		ticker:           time.NewTicker(3 * time.Minute),
		stopChan:         make(chan bool),
	}
}

// Start begins the background monitoring goroutine.
func (m *AirtagMonitor) Start() {
	log.Printf("📡 [AirtagMonitor] Starting drift monitor (3-minute intervals, reads from DB)")

	go func() {
		// Check immediately on startup
		m.checkDrift()

		for {
			select {
			case <-m.stopChan:
				log.Println("🛑 [AirtagMonitor] Stopping...")
				return
			case <-m.ticker.C:
				m.checkDrift()
			}
		}
	}()
}

// Stop halts the monitor.
func (m *AirtagMonitor) Stop() {
	m.ticker.Stop()
	m.stopChan <- true
}

func (m *AirtagMonitor) checkDrift() {
	ctx := context.Background()

	// 0. Read notification settings — skip if drift alerts disabled
	settings := loadNotificationSettings(m.db)
	if !settings.DriftAlertsEnabled {
		return
	}

	threshold := float64(settings.DriftThresholdMeters)
	if threshold < 50 {
		threshold = defaultDriftThresholdMeters
	}

	// 1. Fetch AirTag positions from bridge
	airtags, err := m.fetchAirtagLocations()
	if err != nil {
		log.Printf("⚠️  [AirtagMonitor] Failed to fetch AirTag locations: %v", err)
		return
	}
	if len(airtags) == 0 {
		return
	}

	// 2. Get all bins from DB with coordinates
	var bins []binRow
	err = m.db.Select(&bins, `
		SELECT id, bin_number, latitude, longitude, status, current_street, city
		FROM bins
		WHERE latitude IS NOT NULL AND longitude IS NOT NULL
	`)
	if err != nil {
		log.Printf("⚠️  [AirtagMonitor] Failed to query bins: %v", err)
		return
	}

	// Build bin lookup by bin_number
	binMap := make(map[int]binRow, len(bins))
	for _, b := range bins {
		binMap[b.BinNumber] = b
	}

	// 3. Get bins currently in transit (pickup completed, dropoff not completed)
	inTransitBinIDs, err := m.getInTransitBinIDs()
	if err != nil {
		log.Printf("⚠️  [AirtagMonitor] Failed to query in-transit bins: %v", err)
		return
	}
	inTransitSet := make(map[string]bool, len(inTransitBinIDs))
	for _, id := range inTransitBinIDs {
		inTransitSet[id] = true
	}

	// 4. Compare positions and detect drift
	var alerts []map[string]interface{}

	for _, at := range airtags {
		bin, ok := binMap[at.BinNumber]
		if !ok {
			continue // No matching bin in DB
		}

		if bin.Latitude == nil || bin.Longitude == nil {
			continue
		}

		// Skip bins in transit
		if inTransitSet[bin.ID] {
			continue
		}

		distance := haversineMeters(*bin.Latitude, *bin.Longitude, at.Latitude, at.Longitude)

		if distance > threshold {
			log.Printf("🚨 [AirtagMonitor] Bin %d drifted %.0fm (status: %s)", bin.BinNumber, distance, bin.Status)

			alert := map[string]interface{}{
				"bin_number":       bin.BinNumber,
				"bin_id":           bin.ID,
				"bin_status":       bin.Status,
				"expected_lat":     *bin.Latitude,
				"expected_lng":     *bin.Longitude,
				"actual_lat":       at.Latitude,
				"actual_lng":       at.Longitude,
				"distance_meters":  int(distance),
				"expected_address": formatAddress(bin.CurrentStreet, bin.City),
				"actual_address":   formatAddress(at.Address, at.City),
				"detected_at":      time.Now().UTC().Format(time.RFC3339),
			}
			alerts = append(alerts, alert)
		}
	}

	if len(alerts) == 0 {
		return
	}

	log.Printf("🚨 [AirtagMonitor] Detected %d drift alerts", len(alerts))

	// 5. Send alerts
	m.sendAlerts(ctx, alerts)
}

// GetAirtagLocationsFromDB reads matched AirTag locations from the database.
// Shared by AirtagMonitor, DigestScheduler, and the GetAirtagLocations handler.
func GetAirtagLocationsFromDB(db *sqlx.DB) ([]AirtagEntry, error) {
	var rows []struct {
		ID            string  `db:"id"`
		BinNumber     *int    `db:"bin_number"`
		Name          string  `db:"name"`
		Latitude      float64 `db:"latitude"`
		Longitude     float64 `db:"longitude"`
		Address       string  `db:"address"`
		City          string  `db:"city"`
		LastSeen      string  `db:"last_seen"`
		BatteryStatus int     `db:"battery_status"`
	}

	err := db.Select(&rows, `
		SELECT id, bin_number, name, latitude, longitude, address, city,
		       TO_CHAR(last_seen AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS last_seen,
		       battery_status
		FROM airtag_locations
		WHERE is_matched = TRUE
		ORDER BY bin_number ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query airtag_locations: %w", err)
	}

	entries := make([]AirtagEntry, len(rows))
	for i, row := range rows {
		binNum := 0
		if row.BinNumber != nil {
			binNum = *row.BinNumber
		}
		entries[i] = AirtagEntry{
			ID:            row.ID,
			BinNumber:     binNum,
			Name:          row.Name,
			Latitude:      row.Latitude,
			Longitude:     row.Longitude,
			Address:       row.Address,
			City:          row.City,
			LastSeen:      row.LastSeen,
			BatteryStatus: row.BatteryStatus,
		}
	}
	return entries, nil
}

func (m *AirtagMonitor) fetchAirtagLocations() ([]AirtagEntry, error) {
	// Read from DB (single source of truth)
	entries, err := GetAirtagLocationsFromDB(m.db)
	if err == nil && len(entries) > 0 {
		return entries, nil
	}
	if err != nil {
		log.Printf("⚠️  [AirtagMonitor] DB read failed, falling back to bridge: %v", err)
	}

	// Fallback: fetch from bridge directly
	if m.bridgeURL == "" {
		return nil, fmt.Errorf("no airtag data in DB and FINDMY_BRIDGE_URL not set")
	}
	resp, err := FetchAirtagLocations(m.bridgeURL)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (m *AirtagMonitor) getInTransitBinIDs() ([]string, error) {
	// A bin is "in transit" when its pickup task is completed but its dropoff task is not.
	// Both tasks share the same move_request_id.
	var binIDs []string
	err := m.db.Select(&binIDs, `
		SELECT DISTINCT rt_pickup.bin_id
		FROM route_tasks rt_pickup
		JOIN route_tasks rt_dropoff
			ON rt_pickup.move_request_id = rt_dropoff.move_request_id
		WHERE rt_pickup.task_type IN ('pickup', 'collection')
			AND rt_pickup.is_completed = 1
			AND rt_dropoff.task_type = 'dropoff'
			AND rt_dropoff.is_completed = 0
			AND rt_pickup.is_deleted = false
			AND rt_dropoff.is_deleted = false
			AND rt_pickup.bin_id IS NOT NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("query in-transit bins: %w", err)
	}
	return binIDs, nil
}

func (m *AirtagMonitor) sendAlerts(ctx context.Context, alerts []map[string]interface{}) {
	// Get admin FCM tokens
	var tokens []string
	err := m.db.Select(&tokens, `
		SELECT ft.token FROM fcm_tokens ft
		JOIN users u ON ft.user_id = u.id
		WHERE u.role = 'admin'
	`)
	if err != nil {
		log.Printf("⚠️  [AirtagMonitor] Failed to query admin tokens: %v", err)
	}

	for _, alert := range alerts {
		binNumber := alert["bin_number"].(int)
		binStatus := alert["bin_status"].(string)
		distMeters := alert["distance_meters"].(int)
		actualAddr := alert["actual_address"].(string)
		distStr := formatDistance(distMeters)

		// Build alert message based on bin status
		var title, body string
		switch binStatus {
		case "active":
			title = fmt.Sprintf("Bin %d Moved!", binNumber)
			body = fmt.Sprintf("Detected %s from assigned location", distStr)
		case "in_storage":
			title = fmt.Sprintf("Bin %d Left Warehouse!", binNumber)
			body = fmt.Sprintf("Moved %s from storage", distStr)
		case "missing":
			title = fmt.Sprintf("Bin %d (MISSING) Detected!", binNumber)
			if actualAddr != "" {
				body = fmt.Sprintf("Found at %s (%s away)", actualAddr, distStr)
			} else {
				body = fmt.Sprintf("Detected %s from last known location", distStr)
			}
		default:
			title = fmt.Sprintf("Bin %d Drift Alert", binNumber)
			body = fmt.Sprintf("Moved %s from expected location", distStr)
		}

		// Create per-user notifications first so we have the log ID for FCM deep links
		adminIDs, _ := GetAdminUserIDs(m.db)
		logID, _ := CreateNotificationForUsers(m.db, adminIDs, "bin_drift_alert", title, body, alert)

		// Publish to Centrifugo for dashboard
		if m.centrifugoClient != nil {
			payload := map[string]interface{}{
				"alert": alert,
				"title": title,
				"body":  body,
			}
			if pubErr := m.centrifugoClient.PublishCompanyEvent(ctx, "bin_drift_alert", payload); pubErr != nil {
				log.Printf("⚠️  [AirtagMonitor] Failed to publish drift alert to Centrifugo: %v", pubErr)
			}
		}

		// Send FCM push to admins
		if m.fcmService != nil && len(tokens) > 0 {
			data := map[string]string{
				"type":                "bin_drift_alert",
				"bin_number":          strconv.Itoa(binNumber),
				"bin_id":              alert["bin_id"].(string),
				"bin_status":          binStatus,
				"distance_meters":     strconv.Itoa(distMeters),
				"notification_log_id": logID,
			}
			if sendErr := m.fcmService.SendMulticast(tokens, title, body, data); sendErr != nil {
				log.Printf("⚠️  [AirtagMonitor] Failed to send FCM drift alert: %v", sendErr)
			}
		}

		log.Printf("📢 [AirtagMonitor] Alert sent: %s — %s", title, body)
	}
}

func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const R = 6_371_000.0 // Earth radius in meters
	rlat1 := lat1 * math.Pi / 180
	rlat2 := lat2 * math.Pi / 180
	dlat := (lat2 - lat1) * math.Pi / 180
	dlng := (lng2 - lng1) * math.Pi / 180

	a := math.Sin(dlat/2)*math.Sin(dlat/2) +
		math.Cos(rlat1)*math.Cos(rlat2)*math.Sin(dlng/2)*math.Sin(dlng/2)
	return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// formatDistance renders meters as miles if >= 1 mile, otherwise as meters.
func formatDistance(meters int) string {
	const metersPerMile = 1609.34
	if float64(meters) >= metersPerMile {
		miles := float64(meters) / metersPerMile
		return fmt.Sprintf("%.1f mi", miles)
	}
	return fmt.Sprintf("%dm", meters)
}

// SyncResponse is the response from the FindMy bridge POST /api/sync endpoint.
type SyncResponse struct {
	Status   string `json:"status"`
	Fetched  int    `json:"fetched"`
	Total    int    `json:"total"`
	Duration string `json:"duration,omitempty"`
}

// TriggerAirtagSync triggers an immediate location sync on the FindMy bridge.
func TriggerAirtagSync(bridgeURL, apiKey string) (*SyncResponse, error) {
	url := bridgeURL + "/api/sync"

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("X-Internal-API-Key", apiKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("bridge returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var syncResp SyncResponse
	if err := json.Unmarshal(body, &syncResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &syncResp, nil
}

func formatAddress(street, city string) string {
	if street != "" && city != "" {
		return street + ", " + city
	}
	if street != "" {
		return street
	}
	return city
}
