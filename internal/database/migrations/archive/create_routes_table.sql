-- Migration: Create routes and route_template_bins tables
-- Purpose: Enable route blueprint management for dashboard
-- Date: 2025-12-28

-- Step 1: Create routes table (route blueprints/templates)
CREATE TABLE IF NOT EXISTS routes (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT,
    geographic_area TEXT NOT NULL,
    schedule_pattern TEXT,
    bin_count INT DEFAULT 0,
    estimated_duration_hours DECIMAL(4,2) DEFAULT 0,
    created_by_user_id TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    CONSTRAINT fk_routes_user FOREIGN KEY (created_by_user_id) REFERENCES users(id) ON DELETE SET NULL
);

-- Step 2: Rename existing route_bins to shift_bins (for active shifts)
-- Note: This step is now handled in database.go (shift_bins is created directly)
-- Keeping this here for reference, but it's idempotent with IF EXISTS
-- ALTER TABLE IF EXISTS route_bins RENAME TO shift_bins;

-- Step 3: Create NEW route_bins table (junction table for route blueprints)
-- This is the template/blueprint bins, not shift bins
CREATE TABLE IF NOT EXISTS route_bins (
    id SERIAL PRIMARY KEY,
    route_id TEXT NOT NULL,
    bin_id TEXT NOT NULL,
    sequence_order INT NOT NULL,
    created_at BIGINT NOT NULL,
    CONSTRAINT fk_route_bins_route FOREIGN KEY (route_id) REFERENCES routes(id) ON DELETE CASCADE,
    CONSTRAINT fk_route_bins_bin FOREIGN KEY (bin_id) REFERENCES bins(id) ON DELETE CASCADE
);

-- Step 4: Add indexes for performance
CREATE INDEX IF NOT EXISTS idx_routes_geographic_area ON routes(geographic_area);
CREATE INDEX IF NOT EXISTS idx_routes_created_by ON routes(created_by_user_id);
CREATE INDEX IF NOT EXISTS idx_routes_created_at ON routes(created_at);
CREATE INDEX IF NOT EXISTS idx_route_bins_route_id ON route_bins(route_id);
CREATE INDEX IF NOT EXISTS idx_route_bins_bin_id ON route_bins(bin_id);

-- Step 5: Add unique constraint to prevent duplicate bins in same route
CREATE UNIQUE INDEX IF NOT EXISTS idx_route_bins_unique ON route_bins(route_id, bin_id);

-- Step 6: Verify the migration
-- Run this to confirm tables exist:
-- SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('routes', 'route_bins', 'shift_bins');

-- Expected result:
-- routes (route blueprints)
-- route_bins (bins in route templates)
-- shift_bins (bins in active shifts - formerly route_bins)
