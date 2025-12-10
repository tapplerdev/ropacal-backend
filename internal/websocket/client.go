package websocket

import (
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jmoiron/sqlx"
)

const (
	// Time allowed to write a message to the peer
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer
	pongWait = 60 * time.Second

	// Send pings to peer with this period (must be less than pongWait)
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer
	maxMessageSize = 2048 // Increased for location_update messages
)

// Client represents a WebSocket client connection
type Client struct {
	ID       string
	UserID   string
	UserRole string // User's role: "driver" or "admin"
	conn     *websocket.Conn
	hub      *Hub
	send     chan []byte
	db       interface{} // Database connection (will be *sqlx.DB)
}

// IncomingMessage represents a message from the client
type IncomingMessage struct {
	Type      string                 `json:"type"`
	Timestamp string                 `json:"timestamp"`
	Data      map[string]interface{} `json:"data"` // For location_update data
}

// NewClient creates a new WebSocket client
func NewClient(userID string, userRole string, conn *websocket.Conn, hub *Hub, db interface{}) *Client {
	return &Client{
		UserID:   userID,
		UserRole: userRole,
		conn:     conn,
		hub:      hub,
		send:     make(chan []byte, 256),
		db:       db,
	}
}

// ReadPump pumps messages from the WebSocket connection to the hub
func (c *Client) ReadPump() {
	defer func() {
		// Mark driver as disconnected in database when WebSocket closes
		c.markAsDisconnected()
		c.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}

		// Parse incoming message
		var msg IncomingMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("Invalid message format: %v", err)
			continue
		}

		// Handle different message types
		switch msg.Type {
		case "ping":
			// Respond with pong
			response := map[string]interface{}{
				"type":      "pong",
				"timestamp": time.Now().Format(time.RFC3339),
			}
			responseData, _ := json.Marshal(response)
			c.send <- responseData

		case "location_update":
			// Handle driver location update
			c.handleLocationUpdate(msg.Data)
		}
	}
}

// WritePump pumps messages from the hub to the WebSocket connection
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Hub closed the channel
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Add queued messages to the current WebSocket message
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write([]byte{'\n'})
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// handleLocationUpdate processes driver location updates received via WebSocket
func (c *Client) handleLocationUpdate(data map[string]interface{}) {
	log.Printf("📍 Received location_update from driver %s", c.UserID)
	log.Printf("   Data: %+v", data)

	// Extract fields from data
	latitude, ok := data["latitude"].(float64)
	if !ok {
		log.Printf("❌ Invalid latitude in location update")
		return
	}

	longitude, ok := data["longitude"].(float64)
	if !ok {
		log.Printf("❌ Invalid longitude in location update")
		return
	}

	// Optional fields (may be nil)
	var heading, speed, accuracy *float64
	if h, ok := data["heading"].(float64); ok {
		heading = &h
	}
	if s, ok := data["speed"].(float64); ok {
		speed = &s
	}
	if a, ok := data["accuracy"].(float64); ok {
		accuracy = &a
	}

	// shift_id and timestamp
	var shiftID *string
	if sid, ok := data["shift_id"].(string); ok {
		shiftID = &sid
	}

	timestamp, ok := data["timestamp"].(float64)
	if !ok {
		log.Printf("❌ Invalid timestamp in location update")
		return
	}

	// OPTIMIZATION: Snap to roads with all optimizations
	snappedLat := latitude
	snappedLng := longitude
	accuracyValue := 100.0 // Default if not provided
	if accuracy != nil {
		accuracyValue = *accuracy
	}

	// Try to snap coordinates (will auto-optimize based on accuracy)
	if c.hub.roadsClient != nil {
		newLat, newLng, err := c.hub.roadsClient.SnapToRoad(latitude, longitude, accuracyValue)
		if err == nil {
			snappedLat = newLat
			snappedLng = newLng
		}
	}

	// Get database connection
	db, ok := c.db.(*sqlx.DB)
	if !ok || db == nil {
		log.Printf("❌ Database connection not available")
		return
	}

	// UPSERT location into database (update if exists, insert if not)
	// Store ORIGINAL coordinates in database (for legal/audit)
	query := `
		INSERT INTO driver_current_location (
			driver_id, latitude, longitude, heading, speed, accuracy, shift_id, timestamp, is_connected, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE, EXTRACT(EPOCH FROM NOW())::BIGINT)
		ON CONFLICT (driver_id)
		DO UPDATE SET
			latitude = EXCLUDED.latitude,
			longitude = EXCLUDED.longitude,
			heading = EXCLUDED.heading,
			speed = EXCLUDED.speed,
			accuracy = EXCLUDED.accuracy,
			shift_id = EXCLUDED.shift_id,
			timestamp = EXCLUDED.timestamp,
			is_connected = TRUE,
			updated_at = EXTRACT(EPOCH FROM NOW())::BIGINT
		RETURNING updated_at
	`

	var updatedAt int64

	// Save ORIGINAL coordinates to database
	err := db.QueryRow(
		query,
		c.UserID,
		latitude,      // Original GPS
		longitude,     // Original GPS
		heading,
		speed,
		accuracy,
		shiftID,
		int64(timestamp),
	).Scan(&updatedAt)

	if err != nil {
		log.Printf("❌ Error saving location to database: %v", err)
		return
	}

	log.Printf("✅ Location updated in database for driver %s", c.UserID)

	// Broadcast SNAPPED coordinates to managers (better visual display)
	locationUpdate := map[string]interface{}{
		"type": "driver_location_update",
		"data": map[string]interface{}{
			"driver_id":  c.UserID,
			"latitude":   snappedLat,   // SNAPPED coordinates for display
			"longitude":  snappedLng,   // SNAPPED coordinates for display
			"heading":    heading,
			"speed":      speed,
			"accuracy":   accuracy,
			"shift_id":   shiftID,
			"timestamp":  int64(timestamp),
			"updated_at": updatedAt,
		},
	}

	// Broadcast to all managers (users with role "admin")
	c.hub.BroadcastToRole("admin", locationUpdate)
	log.Printf("📤 Broadcasted location update to all managers (snapped if needed)")
}

// markAsDisconnected marks the driver as disconnected in the database
// This preserves their last known location for managers to see
func (c *Client) markAsDisconnected() {
	// Only mark drivers as disconnected (not managers)
	if c.UserRole != "driver" {
		return
	}

	db, ok := c.db.(*sqlx.DB)
	if !ok || db == nil {
		log.Printf("❌ Database connection not available for disconnect handler")
		return
	}

	query := `
		UPDATE driver_current_location
		SET is_connected = FALSE,
		    updated_at = EXTRACT(EPOCH FROM NOW())::BIGINT
		WHERE driver_id = $1
	`

	_, err := db.Exec(query, c.UserID)
	if err != nil {
		log.Printf("❌ Error marking driver as disconnected: %v", err)
		return
	}

	log.Printf("🔴 Driver %s marked as disconnected (last position preserved)", c.UserID)
}
