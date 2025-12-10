package roads

import (
	"crypto/md5"
	"fmt"
	"log"
	"sync"
	"time"
)

// RouteCache handles caching of snapped routes to reduce API calls
// Uses route signatures to match similar routes
type RouteCache struct {
	cache      map[string]*CacheEntry
	mutex      sync.RWMutex
	maxEntries int
	ttl        time.Duration
	stats      CacheStats
}

// CacheEntry represents a cached route
type CacheEntry struct {
	SnappedPoints []LatLng
	CreatedAt     time.Time
	LastAccessed  time.Time
	HitCount      int
}

// CacheStats tracks cache performance
type CacheStats struct {
	Hits          int64
	Misses        int64
	Evictions     int64
	TotalSavings  float64 // Estimated API cost savings
	mutex         sync.RWMutex
}

// NewRouteCache creates a new route cache
func NewRouteCache() *RouteCache {
	cache := &RouteCache{
		cache:      make(map[string]*CacheEntry),
		maxEntries: 1000, // Store up to 1000 unique routes
		ttl:        24 * time.Hour, // Cache for 24 hours
	}

	// Start cleanup goroutine
	go cache.cleanupExpired()

	return cache
}

// GenerateRouteSignature creates a unique signature for a route
// Similar routes will have similar signatures
func (c *RouteCache) GenerateRouteSignature(points []LatLng) string {
	if len(points) == 0 {
		return ""
	}

	// Use start, end, and number of points to create signature
	// This captures route similarity without being too strict
	start := points[0]
	end := points[len(points)-1]
	numPoints := len(points)

	// Create signature string
	signature := fmt.Sprintf(
		"%.4f,%.4f_%.4f,%.4f_%d",
		start.Latitude, start.Longitude,
		end.Latitude, end.Longitude,
		numPoints/10, // Quantize to reduce false misses
	)

	// Hash to create shorter key
	hash := md5.Sum([]byte(signature))
	return fmt.Sprintf("%x", hash[:8]) // Use first 8 bytes
}

// Get retrieves snapped points from cache if available
func (c *RouteCache) Get(signature string) ([]LatLng, bool) {
	c.mutex.RLock()
	entry, found := c.cache[signature]
	c.mutex.RUnlock()

	if !found {
		c.recordMiss()
		return nil, false
	}

	// Check if expired
	if time.Since(entry.CreatedAt) > c.ttl {
		c.mutex.Lock()
		delete(c.cache, signature)
		c.mutex.Unlock()
		c.recordMiss()
		c.recordEviction()
		return nil, false
	}

	// Update access time and hit count
	c.mutex.Lock()
	entry.LastAccessed = time.Now()
	entry.HitCount++
	c.mutex.Unlock()

	c.recordHit()
	return entry.SnappedPoints, true
}

// Set stores snapped points in cache
func (c *RouteCache) Set(signature string, snappedPoints []LatLng) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// Evict oldest entries if cache is full
	if len(c.cache) >= c.maxEntries {
		c.evictOldest()
	}

	c.cache[signature] = &CacheEntry{
		SnappedPoints: snappedPoints,
		CreatedAt:     time.Now(),
		LastAccessed:  time.Now(),
		HitCount:      0,
	}
}

// evictOldest removes the least recently used entry
func (c *RouteCache) evictOldest() {
	var oldestKey string
	var oldestTime time.Time

	for key, entry := range c.cache {
		if oldestKey == "" || entry.LastAccessed.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.LastAccessed
		}
	}

	if oldestKey != "" {
		delete(c.cache, oldestKey)
		c.recordEviction()
		log.Printf("🗑️  Evicted oldest cache entry: %s", oldestKey)
	}
}

// cleanupExpired periodically removes expired entries
func (c *RouteCache) cleanupExpired() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		c.mutex.Lock()
		now := time.Now()
		for key, entry := range c.cache {
			if now.Sub(entry.CreatedAt) > c.ttl {
				delete(c.cache, key)
				c.recordEviction()
			}
		}
		c.mutex.Unlock()
	}
}

// recordHit updates cache hit statistics
func (c *RouteCache) recordHit() {
	c.stats.mutex.Lock()
	defer c.stats.mutex.Unlock()

	c.stats.Hits++
	// Each cache hit saves one API call ($5 per 1000 = $0.005)
	c.stats.TotalSavings += 0.005
}

// recordMiss updates cache miss statistics
func (c *RouteCache) recordMiss() {
	c.stats.mutex.Lock()
	defer c.stats.mutex.Unlock()

	c.stats.Misses++
}

// recordEviction updates eviction statistics
func (c *RouteCache) recordEviction() {
	c.stats.mutex.Lock()
	defer c.stats.mutex.Unlock()

	c.stats.Evictions++
}

// GetStats returns cache statistics
func (c *RouteCache) GetStats() map[string]interface{} {
	c.stats.mutex.RLock()
	defer c.stats.mutex.RUnlock()

	c.mutex.RLock()
	cacheSize := len(c.cache)
	c.mutex.RUnlock()

	hitRate := 0.0
	total := c.stats.Hits + c.stats.Misses
	if total > 0 {
		hitRate = float64(c.stats.Hits) / float64(total) * 100
	}

	return map[string]interface{}{
		"cache_size":     cacheSize,
		"max_entries":    c.maxEntries,
		"hits":           c.stats.Hits,
		"misses":         c.stats.Misses,
		"hit_rate":       fmt.Sprintf("%.2f%%", hitRate),
		"evictions":      c.stats.Evictions,
		"total_savings":  fmt.Sprintf("$%.2f", c.stats.TotalSavings),
		"ttl_hours":      int(c.ttl.Hours()),
	}
}
