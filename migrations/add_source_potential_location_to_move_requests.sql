-- Add source_potential_location_id to bin_move_requests
-- This tracks when a warehouse redeployment uses a potential location as the destination,
-- so we can mark the potential location as "converted" when the dropoff is completed.

ALTER TABLE bin_move_requests ADD COLUMN IF NOT EXISTS source_potential_location_id TEXT REFERENCES potential_locations(id);

-- Add index for faster lookups
CREATE INDEX IF NOT EXISTS idx_bin_move_requests_source_potential_location
ON bin_move_requests(source_potential_location_id)
WHERE source_potential_location_id IS NOT NULL;
