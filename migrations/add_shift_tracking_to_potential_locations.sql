-- Add shift tracking to potential_locations conversions
-- This allows us to distinguish between:
-- 1. Manager manual conversions (converted_via_shift_id = NULL)
-- 2. Driver placement conversions during shifts (converted_via_shift_id = shift_id)

ALTER TABLE potential_locations
ADD COLUMN converted_via_shift_id TEXT REFERENCES shifts(id) ON DELETE SET NULL;

-- Index for querying conversions by shift
CREATE INDEX IF NOT EXISTS idx_potential_locations_shift_conversion
    ON potential_locations(converted_via_shift_id)
    WHERE converted_via_shift_id IS NOT NULL;
