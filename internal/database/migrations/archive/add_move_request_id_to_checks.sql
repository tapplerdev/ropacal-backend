-- Add move_request_id column to checks table to link check-ins with move requests
-- This enables tracking which checks were for move request pickups/dropoffs

ALTER TABLE checks
ADD COLUMN IF NOT EXISTS move_request_id TEXT;

-- Add foreign key constraint
ALTER TABLE checks
ADD CONSTRAINT fk_checks_move_request
FOREIGN KEY (move_request_id) REFERENCES bin_move_requests(id) ON DELETE SET NULL;

-- Add index for faster lookups by move_request_id
CREATE INDEX IF NOT EXISTS idx_checks_move_request ON checks(move_request_id);

COMMENT ON COLUMN checks.move_request_id IS 'ID of the move request if this check was for a pickup or dropoff (NULL for regular bin collections)';
