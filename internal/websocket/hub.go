package websocket

import (
	"encoding/json"
	"log"
	"sync"

	"ropacal-backend/internal/services/roads"
)

// Hub maintains active WebSocket connections and broadcasts messages
type Hub struct {
	// Registered clients (userID -> Client)
	clients map[string]*Client

	// Inbound messages from clients
	broadcast chan *Message

	// Register requests from clients
	register chan *Client

	// Unregister requests from clients
	unregister chan *Client

	// Roads API client for snap-to-roads functionality
	roadsClient *roads.RoadsClient

	// Mutex for thread-safe client map access
	mu sync.RWMutex
}

// Message represents a message to broadcast to a specific user
type Message struct {
	UserID string
	Data   interface{}
}

// NewHub creates a new Hub instance
func NewHub() *Hub {
	return &Hub{
		clients:     make(map[string]*Client),
		broadcast:   make(chan *Message, 256),
		register:    make(chan *Client),
		unregister:  make(chan *Client),
		roadsClient: roads.NewRoadsClient(),
	}
}

// Run starts the hub's main loop
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client.UserID] = client
			h.mu.Unlock()
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			log.Printf("✅ [WEBSOCKET] Client CONNECTED")
			log.Printf("   User ID: %s", client.UserID)
			log.Printf("   Role: %s", client.UserRole)
			log.Printf("   Total connected clients: %d", len(h.clients))
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client.UserID]; ok {
				delete(h.clients, client.UserID)
				close(client.send)
				log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
				log.Printf("🔴 [WEBSOCKET] Client DISCONNECTED")
				log.Printf("   User ID: %s", client.UserID)
				log.Printf("   Role: %s", client.UserRole)
				log.Printf("   Remaining connected clients: %d", len(h.clients))
				log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			}
			h.mu.Unlock()

		case message := <-h.broadcast:
			h.mu.RLock()
			if client, ok := h.clients[message.UserID]; ok {
				data, err := json.Marshal(message.Data)
				if err != nil {
					log.Printf("❌ Failed to marshal message: %v", err)
					h.mu.RUnlock()
					continue
				}

				select {
				case client.send <- data:
					// log.Printf("📤 Message sent to %s: %s", message.UserID, string(data))
				default:
					// Client buffer full, disconnect
					close(client.send)
					delete(h.clients, client.UserID)
					log.Printf("⚠️ Client buffer full, disconnecting: %s", message.UserID)
				}
			} else {
				// log.Printf("⚠️ No client found for user: %s", message.UserID)
			}
			h.mu.RUnlock()
		}
	}
}

// BroadcastToUser sends a message to a specific user
func (h *Hub) BroadcastToUser(userID string, data interface{}) {
	h.broadcast <- &Message{
		UserID: userID,
		Data:   data,
	}
}

// BroadcastToRole sends a message to all users with a specific role
func (h *Hub) BroadcastToRole(role string, data interface{}) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	// log.Printf("🔊 BroadcastToRole CALLED")
	// log.Printf("   Target role: '%s'", role)
	// log.Printf("   Total connected clients: %d", len(h.clients))

	// Log all connected clients and their roles
	// for userID, client := range h.clients {
	// 	log.Printf("   Client: %s (role: %s) %s", userID, client.UserRole,
	// 		map[bool]string{true: "✅ MATCH", false: "❌ no match"}[client.UserRole == role])
	// }

	dataBytes, err := json.Marshal(data)
	if err != nil {
		log.Printf("❌ Failed to marshal broadcast message: %v", err)
		return
	}

	// log.Printf("   Message size: %d bytes", len(dataBytes))
	// log.Printf("   Message preview: %s", string(dataBytes[:min(len(dataBytes), 200)]))

	// sentCount := 0
	for _, client := range h.clients {
		if client.UserRole == role {
			select {
			case client.send <- dataBytes:
				// sentCount++
				// log.Printf("   ✅ Sent to %s", userID)
			default:
				// log.Printf("⚠️ Client buffer full, skipping: %s", userID)
			}
		}
	}

	// log.Printf("📤 Broadcast to role '%s': sent to %d clients", role, sentCount)
	// log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}

// GetClientCount returns the number of connected clients
func (h *Hub) GetClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// IsUserConnected checks if a user is currently connected
func (h *Hub) IsUserConnected(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.clients[userID]
	return ok
}

// GetConnectedClientIDs returns a list of all connected client IDs
func (h *Hub) GetConnectedClientIDs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]string, 0, len(h.clients))
	for userID := range h.clients {
		ids = append(ids, userID)
	}
	return ids
}
