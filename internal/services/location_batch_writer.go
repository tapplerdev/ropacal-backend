package services

import (
	"context"
	"encoding/json"
	"log"
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
func (w *LocationBatchWriter) Start() {
	log.Println("📊 [BatchWriter] Starting location batch writer (30-second intervals)")

	go func() {
		for {
			select {
			case <-w.stopChan:
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

	// Prepare batch insert
	query := `
		INSERT INTO driver_location_history
		(driver_id, latitude, longitude, heading, speed, accuracy, shift_id, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, TO_TIMESTAMP($8))
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

		// Insert to PostgreSQL
		_, err := w.db.Exec(query,
			driverID,
			location.Latitude,
			location.Longitude,
			location.Heading,
			location.Speed,
			location.Accuracy,
			location.ShiftID,
			float64(location.Timestamp)/1000.0, // Convert milliseconds to seconds
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
