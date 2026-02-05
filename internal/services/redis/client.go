package redis

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client wraps the Redis client with utility methods
type Client struct {
	*redis.Client
}

// NewClient creates a new Redis client from environment variables
// Expects REDIS_URL environment variable (same as Centrifugo uses)
func NewClient() (*Client, error) {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379" // Default for local development
	}

	log.Printf("🔌 Connecting to Redis: %s", redisURL)

	// Parse Redis URL
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse REDIS_URL: %w", err)
	}

	// Create Redis client
	client := redis.NewClient(opts)

	// Test connection with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	log.Println("✅ Redis connection established")

	return &Client{Client: client}, nil
}

// SaveDriverLocation saves the current location of a driver to Redis
// Key format: ropacal:driver:{driverID}:location
// TTL: 10 minutes (auto-expires if driver stops publishing)
func (c *Client) SaveDriverLocation(ctx context.Context, driverID string, locationJSON string) error {
	key := fmt.Sprintf("ropacal:driver:%s:location", driverID)
	return c.Set(ctx, key, locationJSON, 10*time.Minute).Err()
}

// GetDriverLocation retrieves the current location of a driver from Redis
func (c *Client) GetDriverLocation(ctx context.Context, driverID string) (string, error) {
	key := fmt.Sprintf("ropacal:driver:%s:location", driverID)
	return c.Get(ctx, key).Result()
}

// GetAllDriverLocations retrieves all driver locations from Redis
// Returns map[driverID]locationJSON
func (c *Client) GetAllDriverLocations(ctx context.Context) (map[string]string, error) {
	// Scan for all driver location keys
	pattern := "ropacal:driver:*:location"
	keys, err := c.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)

	for _, key := range keys {
		// Get location JSON
		locationJSON, err := c.Get(ctx, key).Result()
		if err != nil {
			log.Printf("⚠️  Failed to get location for key %s: %v", key, err)
			continue
		}

		// Extract driver ID from key (ropacal:driver:{id}:location)
		// Split by ":" and get the 3rd element (index 2)
		parts := splitKey(key)
		if len(parts) == 4 && parts[0] == "ropacal" && parts[1] == "driver" && parts[3] == "location" {
			driverID := parts[2]
			result[driverID] = locationJSON
		}
	}

	return result, nil
}

// DeleteDriverLocation removes a driver's location from Redis
func (c *Client) DeleteDriverLocation(ctx context.Context, driverID string) error {
	key := fmt.Sprintf("ropacal:driver:%s:location", driverID)
	return c.Del(ctx, key).Err()
}

// Helper function to split Redis key by ":"
func splitKey(key string) []string {
	var parts []string
	var current string

	for _, char := range key {
		if char == ':' {
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
		} else {
			current += string(char)
		}
	}

	if current != "" {
		parts = append(parts, current)
	}

	return parts
}
