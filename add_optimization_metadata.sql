-- Add optimization_metadata column to shifts table
-- This stores HERE Maps optimization data (distance, duration, estimated completion)

ALTER TABLE shifts
ADD COLUMN IF NOT EXISTS optimization_metadata JSONB;

-- Create index for faster queries on shifts with optimization data
CREATE INDEX IF NOT EXISTS idx_shifts_optimization_metadata
ON shifts USING GIN (optimization_metadata)
WHERE optimization_metadata IS NOT NULL;
