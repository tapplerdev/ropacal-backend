-- Create potential_locations table for driver-requested bin locations
-- Supports soft delete (archive) pattern to track conversion history

CREATE TABLE IF NOT EXISTS potential_locations (
    id TEXT PRIMARY KEY,
    address TEXT NOT NULL,
    street TEXT NOT NULL,
    city TEXT NOT NULL,
    zip TEXT NOT NULL,
    latitude REAL,
    longitude REAL,
    requested_by_user_id TEXT NOT NULL,
    requested_by_name TEXT NOT NULL,
    notes TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,

    -- Archive fields (soft delete pattern)
    converted_to_bin_id TEXT,
    converted_at INTEGER,
    converted_by_user_id TEXT,

    FOREIGN KEY (requested_by_user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (converted_to_bin_id) REFERENCES bins(id) ON DELETE SET NULL,
    FOREIGN KEY (converted_by_user_id) REFERENCES users(id) ON DELETE SET NULL
);

-- Indexes for performance
CREATE INDEX IF NOT EXISTS idx_potential_locations_created_at
    ON potential_locations(created_at DESC);

CREATE INDEX IF NOT EXISTS idx_potential_locations_requested_by
    ON potential_locations(requested_by_user_id);

-- Partial index for active (non-converted) locations - faster queries
CREATE INDEX IF NOT EXISTS idx_potential_locations_active
    ON potential_locations(created_at DESC)
    WHERE converted_at IS NULL;

-- Index for converted locations lookup
CREATE INDEX IF NOT EXISTS idx_potential_locations_converted
    ON potential_locations(converted_to_bin_id)
    WHERE converted_to_bin_id IS NOT NULL;
