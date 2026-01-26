-- Add stop_type and move_request_id columns to shift_bins table
-- This enables differentiation between collection, pickup, and dropoff stops

ALTER TABLE shift_bins
ADD COLUMN IF NOT EXISTS stop_type VARCHAR(20) NOT NULL DEFAULT 'collection',
ADD COLUMN IF NOT EXISTS move_request_id VARCHAR(100);

-- Add index for faster lookups by move_request_id
CREATE INDEX IF NOT EXISTS idx_shift_bins_move_request ON shift_bins(move_request_id);

-- Add check constraint to ensure stop_type is valid
ALTER TABLE shift_bins
ADD CONSTRAINT IF NOT EXISTS chk_stop_type
CHECK (stop_type IN ('collection', 'pickup', 'dropoff'));

COMMENT ON COLUMN shift_bins.stop_type IS 'Type of stop: collection (regular bin), pickup (move request pickup), or dropoff (move request dropoff)';
COMMENT ON COLUMN shift_bins.move_request_id IS 'ID of the move request if this stop is for a bin relocation (pickup or dropoff)';
