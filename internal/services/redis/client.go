package redis

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"ropacal-backend/internal/orgdb"
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

// ---------------------------------------------------------------------------
// Driver GPS keys (D3 — tenant partitioning)
//
// Live form:  ropacal:org:{orgID}:driver:{driverID}:location
// Dark form:  ropacal:driver:{driverID}:location   (pre-tenancy only)
//
// There is exactly ONE builder and ONE extractor, and both are derived from the
// same prefix function. That is deliberate. The previous code built keys with a
// Sprintf and read them back with a positional `strings.Split(key, ":")` length-4
// assert, so writer and reader could disagree — and if they ever did, the reader
// returned an EMPTY MAP rather than an error. Empty is indistinguishable from
// "no drivers on shift", so the break would be silent for two hours and then
// auto-end every live shift (the batch writer stops advancing
// driver_current_location, and stale_shift_monitor's 2h threshold fires).
// Keeping build and parse on one prefix makes that drift unrepresentable.
// ---------------------------------------------------------------------------

// orgDriverPrefix returns the key prefix for one organization's driver locations.
// The trailing colon matters: it is what makes CutPrefix yield a bare driver id.
func orgDriverPrefix(orgID string) string {
	if orgID == "" {
		return "ropacal:driver:"
	}
	return fmt.Sprintf("ropacal:org:%s:driver:", orgID)
}

const driverLocationSuffix = ":location"

// driverLocationKey builds the key for one driver under one org.
//
// An empty orgID is only legal while tenancy is dark. Once the migration is
// live it means an unscoped handle reached the GPS path, and returning a
// legacy-shaped key would write somewhere no org-scoped reader looks — GPS that
// silently vanishes. Fail loudly instead.
func driverLocationKey(orgID, driverID string) (string, error) {
	if orgID == "" && orgdb.Migrated() {
		return "", fmt.Errorf("redis: refusing to build an org-less driver location key for driver %s (tenancy is live; caller passed an unscoped handle)", driverID)
	}
	return orgDriverPrefix(orgID) + driverID + driverLocationSuffix, nil
}

// driverIDFromKey is the exact inverse of driverLocationKey, and the ONLY way
// keys are parsed back. It strips the same prefix the writer prepended instead
// of counting colon-separated segments — the old positional parse silently
// dropped any key whose segment count did not equal 4, which is precisely how a
// writer/reader mismatch would have turned into an empty map.
//
// Note it is strict about a NON-EMPTY id: "ropacal:org:X:driver::location" is
// malformed, not a driver whose id is the empty string.
func driverIDFromKey(prefix, key string) (string, bool) {
	id, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return "", false
	}
	id, ok = strings.CutSuffix(id, driverLocationSuffix)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// SaveDriverLocation saves the current location of a driver to Redis.
// TTL: 10 minutes (auto-expires if driver stops publishing).
func (c *Client) SaveDriverLocation(ctx context.Context, orgID, driverID string, locationJSON string) error {
	key, err := driverLocationKey(orgID, driverID)
	if err != nil {
		log.Printf("❌ [Redis] %v", err)
		return err
	}
	return c.Set(ctx, key, locationJSON, 10*time.Minute).Err()
}

// GetDriverLocation retrieves the current location of a driver from Redis.
func (c *Client) GetDriverLocation(ctx context.Context, orgID, driverID string) (string, error) {
	key, err := driverLocationKey(orgID, driverID)
	if err != nil {
		log.Printf("❌ [Redis] %v", err)
		return "", err
	}
	return c.Get(ctx, key).Result()
}

// GetOrgDriverLocations retrieves every driver location belonging to ONE
// organization. Returns map[driverID]locationJSON.
//
// Uses SCAN, not KEYS. KEYS is O(keyspace) and blocks the server for its whole
// duration, and this Redis instance is shared with Centrifugo's engine — so the
// old call stalled realtime message delivery for every tenant, once per active
// org, every 30 seconds from the batch writer. SCAN yields between batches.
//
// Org scoping is now in the match pattern rather than applied after the fact, so
// a second tenant's keys are never even fetched.
func (c *Client) GetOrgDriverLocations(ctx context.Context, orgID string) (map[string]string, error) {
	if orgID == "" && orgdb.Migrated() {
		err := fmt.Errorf("redis: refusing to read driver locations without an org (tenancy is live)")
		log.Printf("❌ [Redis] %v", err)
		return nil, err
	}

	prefix := orgDriverPrefix(orgID)
	pattern := prefix + "*" + driverLocationSuffix

	result := make(map[string]string)
	var cursor uint64
	for {
		keys, next, err := c.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			id, ok := driverIDFromKey(prefix, key)
			if !ok {
				continue
			}

			locationJSON, err := c.Get(ctx, key).Result()
			if err != nil {
				log.Printf("⚠️  Failed to get location for key %s: %v", key, err)
				continue
			}
			result[id] = locationJSON
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}

	return result, nil
}

// DeleteDriverLocation removes a driver's location from Redis
func (c *Client) DeleteDriverLocation(ctx context.Context, orgID, driverID string) error {
	key, err := driverLocationKey(orgID, driverID)
	if err != nil {
		log.Printf("❌ [Redis] %v", err)
		return err
	}
	return c.Del(ctx, key).Err()
}
