-- Migration: Add bin retirement tracking fields
-- Purpose: Track when bins are retired and by whom

ALTER TABLE bins ADD COLUMN IF NOT EXISTS retired_at BIGINT;
ALTER TABLE bins ADD COLUMN IF NOT EXISTS retired_by_user_id TEXT;

-- Add foreign key constraint for retired_by
ALTER TABLE bins ADD CONSTRAINT fk_bins_retired_by
    FOREIGN KEY (retired_by_user_id) REFERENCES users(id) ON DELETE SET NULL;

-- Index for querying retired bins
CREATE INDEX IF NOT EXISTS idx_bins_retired_at ON bins(retired_at);
CREATE INDEX IF NOT EXISTS idx_bins_status ON bins(status);
