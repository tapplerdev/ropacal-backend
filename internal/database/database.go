package database

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

func Connect(dbURL string) (*sqlx.DB, error) {
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Println("🔌 DATABASE CONNECTION ATTEMPT")
	log.Printf("   📍 Database URL length: %d characters", len(dbURL))
	log.Printf("   📍 URL prefix: %s...", dbURL[:min(30, len(dbURL))])
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	log.Println("🔄 Step 1: Attempting sqlx.Connect()...")
	db, err := sqlx.Connect("postgres", dbURL)
	if err != nil {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("❌ DATABASE CONNECTION FAILED AT sqlx.Connect()")
		log.Printf("   Error type: %T", err)
		log.Printf("   Error message: %v", err)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	log.Println("✅ Step 1 Complete: sqlx.Connect() succeeded")

	log.Println("🔄 Step 2: Testing connection with Ping()...")
	if err := db.Ping(); err != nil {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Println("❌ DATABASE CONNECTION FAILED AT Ping()")
		log.Printf("   Error type: %T", err)
		log.Printf("   Error message: %v", err)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}
	log.Println("✅ Step 2 Complete: Ping() succeeded")

	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Println("✅ DATABASE CONNECTION SUCCESSFUL")
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	return db, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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

		// Create shift_bins table (formerly route_bins - renamed for clarity)
		`CREATE TABLE IF NOT EXISTS shift_bins (
			id SERIAL PRIMARY KEY,
			shift_id TEXT NOT NULL,
			bin_id TEXT NOT NULL,
			sequence_order INT NOT NULL,
			is_completed INT NOT NULL DEFAULT 0,
			completed_at BIGINT,
			updated_fill_percentage INT,
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

		// Migration: Add updated_fill_percentage column to existing shift_bins table
		`ALTER TABLE shift_bins ADD COLUMN IF NOT EXISTS updated_fill_percentage INT`,

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
		`CREATE INDEX IF NOT EXISTS idx_shift_bins_shift_id ON shift_bins(shift_id)`,
		`CREATE INDEX IF NOT EXISTS idx_shift_bins_bin_id ON shift_bins(bin_id)`,
		`CREATE INDEX IF NOT EXISTS idx_shift_bins_shift_seq ON shift_bins(shift_id, sequence_order)`,
		`CREATE INDEX IF NOT EXISTS idx_driver_current_location_shift_id ON driver_current_location(shift_id)`,
		`CREATE INDEX IF NOT EXISTS idx_driver_current_location_is_connected ON driver_current_location(is_connected)`,

		// Add 'ended' and 'cancelled' status values to CHECK constraint
		// Drop old constraint and add new one with additional values
		`ALTER TABLE shifts DROP CONSTRAINT IF EXISTS shifts_status_check`,
		`ALTER TABLE shifts ADD CONSTRAINT shifts_status_check CHECK(status IN ('inactive', 'ready', 'active', 'paused', 'ended', 'cancelled'))`,

		// Create shift_history table for completed shifts
		`CREATE TABLE IF NOT EXISTS shift_history (
			id TEXT PRIMARY KEY,
			driver_id TEXT NOT NULL,
			route_id TEXT,

			-- Shift timing
			start_time BIGINT,
			end_time BIGINT,
			created_at BIGINT NOT NULL,
			ended_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,

			-- Performance metrics
			total_pause_seconds INT DEFAULT 0,
			total_bins INT DEFAULT 0,
			completed_bins INT DEFAULT 0,
			completion_rate DECIMAL(5,2) NOT NULL,

			-- Incident tracking
			incidents_reported INT DEFAULT 0,
			field_observations INT DEFAULT 0,

			-- End reason tracking
			end_reason TEXT NOT NULL CHECK(end_reason IN ('completed', 'manual_end', 'manager_ended', 'manager_cancelled', 'driver_disconnected', 'system_timeout')),
			ended_by_user_id TEXT,
			end_reason_metadata JSONB,

			-- Foreign keys
			FOREIGN KEY (driver_id) REFERENCES users(id) ON DELETE CASCADE,
			FOREIGN KEY (ended_by_user_id) REFERENCES users(id) ON DELETE SET NULL
		)`,

		// Create indexes for shift_history
		`CREATE INDEX IF NOT EXISTS idx_shift_history_driver_id ON shift_history(driver_id)`,
		`CREATE INDEX IF NOT EXISTS idx_shift_history_ended_at ON shift_history(ended_at)`,
		`CREATE INDEX IF NOT EXISTS idx_shift_history_end_reason ON shift_history(end_reason)`,
		`CREATE INDEX IF NOT EXISTS idx_shift_history_completion_rate ON shift_history(completion_rate)`,

		// Create no_go_zones table
		`CREATE TABLE IF NOT EXISTS no_go_zones (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			center_latitude DOUBLE PRECISION NOT NULL,
			center_longitude DOUBLE PRECISION NOT NULL,
			radius_meters INT NOT NULL DEFAULT 500,
			conflict_score INT NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active', 'monitoring', 'resolved')),
			created_by_user_id TEXT,
			created_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,
			updated_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,
			resolved_by_user_id TEXT,
			resolved_at BIGINT,
			resolution_notes TEXT,
			FOREIGN KEY (created_by_user_id) REFERENCES users(id) ON DELETE SET NULL,
			FOREIGN KEY (resolved_by_user_id) REFERENCES users(id) ON DELETE SET NULL
		)`,

		// Create zone_incidents table
		`CREATE TABLE IF NOT EXISTS zone_incidents (
			id TEXT PRIMARY KEY,
			zone_id TEXT NOT NULL,
			bin_id TEXT NOT NULL,
			incident_type TEXT NOT NULL CHECK(incident_type IN ('vandalism', 'landlord_complaint', 'theft', 'relocation_request', 'missing', 'damaged', 'vandalized', 'inaccessible')),
			reported_by_user_id TEXT,
			reported_at BIGINT NOT NULL,
			description TEXT,
			photo_url TEXT,
			check_id INT,
			move_id INT,
			shift_id TEXT,
			reporter_latitude DOUBLE PRECISION,
			reporter_longitude DOUBLE PRECISION,
			is_field_observation BOOLEAN NOT NULL DEFAULT FALSE,
			verified_by_user_id TEXT,
			verified_at BIGINT,
			status TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open', 'resolved', 'investigating')),
			FOREIGN KEY (zone_id) REFERENCES no_go_zones(id) ON DELETE CASCADE,
			FOREIGN KEY (bin_id) REFERENCES bins(id) ON DELETE CASCADE,
			FOREIGN KEY (reported_by_user_id) REFERENCES users(id) ON DELETE SET NULL,
			FOREIGN KEY (check_id) REFERENCES checks(id) ON DELETE SET NULL,
			FOREIGN KEY (move_id) REFERENCES moves(id) ON DELETE SET NULL,
			FOREIGN KEY (shift_id) REFERENCES shifts(id) ON DELETE SET NULL,
			FOREIGN KEY (verified_by_user_id) REFERENCES users(id) ON DELETE SET NULL
		)`,

		// Create zone_risk_overrides table
		`CREATE TABLE IF NOT EXISTS zone_risk_overrides (
			id TEXT PRIMARY KEY,
			zone_id TEXT NOT NULL,
			bin_id TEXT NOT NULL,
			manager_id TEXT NOT NULL,
			override_reason TEXT NOT NULL,
			override_at BIGINT NOT NULL,
			expires_at BIGINT,
			status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active', 'expired', 'revoked')),
			incident_count INT NOT NULL DEFAULT 0,
			last_incident_id TEXT,
			FOREIGN KEY (zone_id) REFERENCES no_go_zones(id) ON DELETE CASCADE,
			FOREIGN KEY (bin_id) REFERENCES bins(id) ON DELETE CASCADE,
			FOREIGN KEY (manager_id) REFERENCES users(id) ON DELETE CASCADE
		)`,

		// ALTER TABLE migrations for existing zone_incidents table
		// These add new columns if they don't exist (for production database)
		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
						   WHERE table_name='zone_incidents' AND column_name='shift_id') THEN
				ALTER TABLE zone_incidents ADD COLUMN shift_id TEXT;
			END IF;
		END $$`,

		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
						   WHERE table_name='zone_incidents' AND column_name='reporter_latitude') THEN
				ALTER TABLE zone_incidents ADD COLUMN reporter_latitude DOUBLE PRECISION;
			END IF;
		END $$`,

		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
						   WHERE table_name='zone_incidents' AND column_name='reporter_longitude') THEN
				ALTER TABLE zone_incidents ADD COLUMN reporter_longitude DOUBLE PRECISION;
			END IF;
		END $$`,

		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
						   WHERE table_name='zone_incidents' AND column_name='is_field_observation') THEN
				ALTER TABLE zone_incidents ADD COLUMN is_field_observation BOOLEAN NOT NULL DEFAULT FALSE;
			END IF;
		END $$`,

		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
						   WHERE table_name='zone_incidents' AND column_name='verified_by_user_id') THEN
				ALTER TABLE zone_incidents ADD COLUMN verified_by_user_id TEXT;
			END IF;
		END $$`,

		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
						   WHERE table_name='zone_incidents' AND column_name='verified_at') THEN
				ALTER TABLE zone_incidents ADD COLUMN verified_at BIGINT;
			END IF;
		END $$`,

		// Add foreign key constraints if they don't exist
		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.table_constraints
						   WHERE constraint_name='zone_incidents_shift_id_fkey'
						   AND table_name='zone_incidents') THEN
				ALTER TABLE zone_incidents ADD CONSTRAINT zone_incidents_shift_id_fkey
					FOREIGN KEY (shift_id) REFERENCES shifts(id) ON DELETE SET NULL;
			END IF;
		END $$`,

		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.table_constraints
						   WHERE constraint_name='zone_incidents_verified_by_user_id_fkey'
						   AND table_name='zone_incidents') THEN
				ALTER TABLE zone_incidents ADD CONSTRAINT zone_incidents_verified_by_user_id_fkey
					FOREIGN KEY (verified_by_user_id) REFERENCES users(id) ON DELETE SET NULL;
			END IF;
		END $$`,

		// ALTER TABLE migrations for existing shift_history table
		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
						   WHERE table_name='shift_history' AND column_name='incidents_reported') THEN
				ALTER TABLE shift_history ADD COLUMN incidents_reported INT DEFAULT 0;
			END IF;
		END $$`,

		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
						   WHERE table_name='shift_history' AND column_name='field_observations') THEN
				ALTER TABLE shift_history ADD COLUMN field_observations INT DEFAULT 0;
			END IF;
		END $$`,

		// ALTER TABLE migration to make fill_percentage nullable in checks table
		// This allows incident-only check-ins where fill cannot be assessed
		`DO $$
		BEGIN
			-- Check if fill_percentage is currently NOT NULL
			IF EXISTS (SELECT 1 FROM information_schema.columns
					   WHERE table_name='checks'
					   AND column_name='fill_percentage'
					   AND is_nullable='NO') THEN
				ALTER TABLE checks ALTER COLUMN fill_percentage DROP NOT NULL;
			END IF;
		END $$`,

		// Add zone merge tracking columns to no_go_zones table
		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
						   WHERE table_name='no_go_zones' AND column_name='merged_into_zone_id') THEN
				ALTER TABLE no_go_zones ADD COLUMN merged_into_zone_id TEXT;
			END IF;
		END $$`,

		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
						   WHERE table_name='no_go_zones' AND column_name='resolution_type') THEN
				ALTER TABLE no_go_zones ADD COLUMN resolution_type TEXT CHECK(resolution_type IN ('merged', 'manual_resolution'));
			END IF;
		END $$`,

		// Add foreign key constraint for merged_into_zone_id
		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.table_constraints
						   WHERE constraint_name='no_go_zones_merged_into_zone_id_fkey'
						   AND table_name='no_go_zones') THEN
				ALTER TABLE no_go_zones ADD CONSTRAINT no_go_zones_merged_into_zone_id_fkey
					FOREIGN KEY (merged_into_zone_id) REFERENCES no_go_zones(id) ON DELETE SET NULL;
			END IF;
		END $$`,

		// Create indexes for no_go_zones
		`CREATE INDEX IF NOT EXISTS idx_no_go_zones_status ON no_go_zones(status)`,
		`CREATE INDEX IF NOT EXISTS idx_no_go_zones_created_by ON no_go_zones(created_by_user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_no_go_zones_location ON no_go_zones(center_latitude, center_longitude)`,
		`CREATE INDEX IF NOT EXISTS idx_no_go_zones_merged_into ON no_go_zones(merged_into_zone_id)`,

		// Create indexes for zone_incidents
		`CREATE INDEX IF NOT EXISTS idx_zone_incidents_zone ON zone_incidents(zone_id)`,
		`CREATE INDEX IF NOT EXISTS idx_zone_incidents_bin ON zone_incidents(bin_id)`,
		`CREATE INDEX IF NOT EXISTS idx_zone_incidents_date ON zone_incidents(reported_at)`,
		`CREATE INDEX IF NOT EXISTS idx_zone_incidents_type ON zone_incidents(incident_type)`,
		`CREATE INDEX IF NOT EXISTS idx_zone_incidents_bin_zone ON zone_incidents(bin_id, zone_id)`,
		`CREATE INDEX IF NOT EXISTS idx_zone_incidents_field_observation ON zone_incidents(is_field_observation)`,
		`CREATE INDEX IF NOT EXISTS idx_zone_incidents_verification ON zone_incidents(verified_by_user_id, verified_at)`,
		`CREATE INDEX IF NOT EXISTS idx_zone_incidents_shift ON zone_incidents(shift_id)`,

		// Create indexes for zone_risk_overrides
		`CREATE INDEX IF NOT EXISTS idx_zone_risk_overrides_zone ON zone_risk_overrides(zone_id)`,
		`CREATE INDEX IF NOT EXISTS idx_zone_risk_overrides_bin ON zone_risk_overrides(bin_id)`,
		`CREATE INDEX IF NOT EXISTS idx_zone_risk_overrides_manager ON zone_risk_overrides(manager_id)`,
		`CREATE INDEX IF NOT EXISTS idx_zone_risk_overrides_status ON zone_risk_overrides(status)`,

		// Migration: Update zone_incidents incident_type constraint to include new types
		`ALTER TABLE zone_incidents DROP CONSTRAINT IF EXISTS zone_incidents_incident_type_check`,
		`ALTER TABLE zone_incidents ADD CONSTRAINT zone_incidents_incident_type_check CHECK(incident_type IN ('vandalism', 'landlord_complaint', 'theft', 'relocation_request', 'missing', 'damaged', 'vandalized', 'inaccessible'))`,
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
