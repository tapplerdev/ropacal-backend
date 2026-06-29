package services

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"ropacal-backend/internal/services/redis"

	"github.com/jmoiron/sqlx"
)

// LocationBatchWriter periodically writes driver locations from Redis to PostgreSQL
// This allows fast writes to Redis while maintaining a persistent historical record
type LocationBatchWriter struct {
	db          *sqlx.DB
	redisClient *redis.Client
	ticker      *time.Ticker
	stopChan    chan bool
}

// NewLocationBatchWriter creates a new batch writer that runs every 30 seconds
func NewLocationBatchWriter(db *sqlx.DB, redisClient *redis.Client) *LocationBatchWriter {
	return &LocationBatchWriter{
		db:          db,
		redisClient: redisClient,
		ticker:      time.NewTicker(30 * time.Second),
		stopChan:    make(chan bool),
	}
}

// Start begins the background batch writing process
func (w *LocationBatchWriter) Start(ctx context.Context, wg *sync.WaitGroup) {
	log.Println("📊 [BatchWriter] Starting location batch writer (30-second intervals)")

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				log.Println("🛑 [BatchWriter] Stopping...")
				return
			case <-w.ticker.C:
				w.writeBatch()
			}
		}
	}()
}

// Stop halts the batch writer
func (w *LocationBatchWriter) Stop() {
	w.ticker.Stop()
	w.stopChan <- true
}

// writeBatch reads all driver locations from Redis and inserts them into PostgreSQL
func (w *LocationBatchWriter) writeBatch() {
	ctx := context.Background()

	// Get all driver locations from Redis
	locations, err := w.redisClient.GetAllDriverLocations(ctx)
	if err != nil {
		log.Printf("❌ [BatchWriter] Failed to get locations from Redis: %v", err)
		return
	}

	if len(locations) == 0 {
		log.Println("📊 [BatchWriter] No locations to write (no active drivers)")
		return
	}

	log.Printf("📊 [BatchWriter] Writing %d location points to PostgreSQL...", len(locations))

	// Upsert into driver_current_location (one row per driver)
	query := `
		INSERT INTO driver_current_location
		(driver_id, latitude, longitude, heading, speed, accuracy, shift_id, timestamp, updated_at)
		-- timestamp = client GPS epoch ($8); updated_at = SERVER time, so it means
		-- the same thing the HTTP UpdateLocation path writes. Freshness guards
		-- (StartShift/preflight/stale_shift_monitor) compare against now() and must
		-- not be fed a client phone clock.
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, EXTRACT(EPOCH FROM NOW())::BIGINT)
		ON CONFLICT (driver_id) DO UPDATE SET
			latitude = EXCLUDED.latitude,
			longitude = EXCLUDED.longitude,
			heading = EXCLUDED.heading,
			speed = EXCLUDED.speed,
			accuracy = EXCLUDED.accuracy,
			shift_id = EXCLUDED.shift_id,
			timestamp = EXCLUDED.timestamp,
			updated_at = EXCLUDED.updated_at,
			is_connected = true
	`

	successCount := 0
	errorCount := 0

	for driverID, locationJSON := range locations {
		var location struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Accuracy  float64 `json:"accuracy"`
			Heading   float64 `json:"heading"`
			Speed     float64 `json:"speed"`
			ShiftID   *string `json:"shift_id"`
			Timestamp int64   `json:"timestamp"`
		}

		if err := json.Unmarshal([]byte(locationJSON), &location); err != nil {
			log.Printf("⚠️  [BatchWriter] Failed to parse location for driver %s: %v", driverID, err)
			errorCount++
			continue
		}

		// Upsert to PostgreSQL
		timestampSec := location.Timestamp / 1000 // Convert milliseconds to seconds
		_, err := w.db.Exec(query,
			driverID,
			location.Latitude,
			location.Longitude,
			location.Heading,
			location.Speed,
			location.Accuracy,
			location.ShiftID,
			timestampSec,
		)

		if err != nil {
			log.Printf("⚠️  [BatchWriter] Failed to insert location for driver %s: %v", driverID, err)
			errorCount++
		} else {
			successCount++
		}
	}

	log.Printf("✅ [BatchWriter] Batch complete: %d success, %d errors", successCount, errorCount)
}
