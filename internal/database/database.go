package database

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

func Connect(dbURL string) (*sqlx.DB, error) {
	db, err := sqlx.Connect("postgres", dbURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// Test the connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	log.Println("✓ Connected to PostgreSQL database")
	return db, nil
}

func Migrate(db *sqlx.DB) error {
	migrations := []string{
		// Create users table
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			password TEXT NOT NULL,
			name TEXT NOT NULL,
			role TEXT NOT NULL CHECK(role IN ('driver', 'admin')),
			created_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,
			updated_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT
		)`,

		// Create bins table
		`CREATE TABLE IF NOT EXISTS bins (
			id TEXT PRIMARY KEY,
			bin_number INT NOT NULL UNIQUE,
			current_street TEXT NOT NULL,
			city TEXT NOT NULL,
			zip TEXT NOT NULL,
			last_moved BIGINT,
			last_checked BIGINT,
			status TEXT NOT NULL,
			fill_percentage INT,
			checked INT NOT NULL DEFAULT 0,
			move_requested INT NOT NULL DEFAULT 0,
			latitude DOUBLE PRECISION,
			longitude DOUBLE PRECISION,
			created_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,
			updated_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT
		)`,

		// Create moves table
		`CREATE TABLE IF NOT EXISTS moves (
			id SERIAL PRIMARY KEY,
			bin_id TEXT NOT NULL,
			moved_from TEXT NOT NULL,
			moved_to TEXT NOT NULL,
			moved_on BIGINT NOT NULL,
			FOREIGN KEY (bin_id) REFERENCES bins(id) ON DELETE CASCADE
		)`,

		// Create checks table
		`CREATE TABLE IF NOT EXISTS checks (
			id SERIAL PRIMARY KEY,
			bin_id TEXT NOT NULL,
			checked_from TEXT NOT NULL,
			fill_percentage INT NOT NULL,
			checked_on BIGINT NOT NULL,
			FOREIGN KEY (bin_id) REFERENCES bins(id) ON DELETE CASCADE
		)`,

		// Create shifts table
		`CREATE TABLE IF NOT EXISTS shifts (
			id TEXT PRIMARY KEY,
			driver_id TEXT NOT NULL,
			route_id TEXT,
			status TEXT NOT NULL CHECK(status IN ('inactive', 'ready', 'active', 'paused')),
			start_time BIGINT,
			end_time BIGINT,
			total_pause_seconds INT DEFAULT 0,
			pause_start_time BIGINT,
			total_bins INT DEFAULT 0,
			completed_bins INT DEFAULT 0,
			created_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,
			updated_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,
			FOREIGN KEY (driver_id) REFERENCES users(id) ON DELETE CASCADE,
			CHECK (completed_bins <= total_bins),
			CHECK (total_pause_seconds >= 0)
		)`,

		// Create FCM tokens table
		`CREATE TABLE IF NOT EXISTS fcm_tokens (
			id SERIAL PRIMARY KEY,
			user_id TEXT NOT NULL,
			token TEXT NOT NULL UNIQUE,
			device_type TEXT NOT NULL CHECK(device_type IN ('ios', 'android')),
			created_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,
			updated_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		)`,

		// Create route_bins table
		`CREATE TABLE IF NOT EXISTS route_bins (
			id SERIAL PRIMARY KEY,
			shift_id TEXT NOT NULL,
			bin_id TEXT NOT NULL,
			sequence_order INT NOT NULL,
			is_completed INT NOT NULL DEFAULT 0,
			completed_at BIGINT,
			created_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,
			FOREIGN KEY (shift_id) REFERENCES shifts(id) ON DELETE CASCADE,
			FOREIGN KEY (bin_id) REFERENCES bins(id) ON DELETE CASCADE
		)`,

		// Create driver_current_location table (stores only latest position per driver)
		// This table has exactly 1 row per driver, updated via UPSERT
		// Primary tracking is via WebSocket broadcasts - DB is fallback for disconnections
		`CREATE TABLE IF NOT EXISTS driver_current_location (
			driver_id TEXT PRIMARY KEY,
			latitude DOUBLE PRECISION NOT NULL,
			longitude DOUBLE PRECISION NOT NULL,
			heading DOUBLE PRECISION,
			speed DOUBLE PRECISION,
			accuracy DOUBLE PRECISION,
			shift_id TEXT,
			timestamp BIGINT NOT NULL,
			is_connected BOOLEAN NOT NULL DEFAULT TRUE,
			updated_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,
			FOREIGN KEY (driver_id) REFERENCES users(id) ON DELETE CASCADE,
			FOREIGN KEY (shift_id) REFERENCES shifts(id) ON DELETE SET NULL
		)`,

		// Create indexes
		`CREATE INDEX IF NOT EXISTS idx_users_email ON users(email)`,
		`CREATE INDEX IF NOT EXISTS idx_moves_bin_id ON moves(bin_id)`,
		`CREATE INDEX IF NOT EXISTS idx_moves_moved_on ON moves(moved_on)`,
		`CREATE INDEX IF NOT EXISTS idx_checks_bin_id ON checks(bin_id)`,
		`CREATE INDEX IF NOT EXISTS idx_checks_checked_on ON checks(checked_on)`,
		`CREATE INDEX IF NOT EXISTS idx_shifts_driver_id ON shifts(driver_id)`,
		`CREATE INDEX IF NOT EXISTS idx_shifts_status ON shifts(status)`,
		`CREATE INDEX IF NOT EXISTS idx_shifts_created_at ON shifts(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_fcm_tokens_user_id ON fcm_tokens(user_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_fcm_tokens_token ON fcm_tokens(token)`,
		`CREATE INDEX IF NOT EXISTS idx_route_bins_shift_id ON route_bins(shift_id)`,
		`CREATE INDEX IF NOT EXISTS idx_route_bins_bin_id ON route_bins(bin_id)`,
		`CREATE INDEX IF NOT EXISTS idx_route_bins_shift_seq ON route_bins(shift_id, sequence_order)`,
		`CREATE INDEX IF NOT EXISTS idx_driver_current_location_shift_id ON driver_current_location(shift_id)`,
		`CREATE INDEX IF NOT EXISTS idx_driver_current_location_is_connected ON driver_current_location(is_connected)`,

		// Add 'ended' and 'cancelled' status values to CHECK constraint
		// Drop old constraint and add new one with additional values
		`ALTER TABLE shifts DROP CONSTRAINT IF EXISTS shifts_status_check`,
		`ALTER TABLE shifts ADD CONSTRAINT shifts_status_check CHECK(status IN ('inactive', 'ready', 'active', 'paused', 'ended', 'cancelled'))`,
	}

	for _, migration := range migrations {
		if _, err := db.Exec(migration); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}

	log.Println("✓ Database migrations completed")
	return nil
}

// Helper functions for time conversion
func TimeToUnix(t interface{}) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{Valid: false}
	}
	return sql.NullInt64{Int64: 0, Valid: false} // Will be set properly in handlers
}
