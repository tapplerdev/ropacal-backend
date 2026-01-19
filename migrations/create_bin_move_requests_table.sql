-- Migration: Create bin_move_requests table
-- Purpose: Enable manager scheduling of bin moves (urgent or future)
-- Date: 2026-01-16

-- Create bin_move_requests table
CREATE TABLE IF NOT EXISTS bin_move_requests (
    id TEXT PRIMARY KEY,
    bin_id TEXT NOT NULL,
    scheduled_date BIGINT NOT NULL, -- Unix timestamp for when move should happen
    urgency TEXT NOT NULL CHECK(urgency IN ('urgent', 'scheduled')),
    requested_by TEXT NOT NULL, -- User ID who requested the move
    status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending', 'in_progress', 'completed', 'cancelled')),

    -- Original location (where bin currently is)
    original_latitude DOUBLE PRECISION NOT NULL,
    original_longitude DOUBLE PRECISION NOT NULL,
    original_address TEXT NOT NULL,

    -- New location (nullable for pickup-only moves)
    new_latitude DOUBLE PRECISION,
    new_longitude DOUBLE PRECISION,
    new_address TEXT,

    -- Move metadata
    move_type TEXT NOT NULL CHECK(move_type IN ('pickup_only', 'relocation')),
    disposal_action TEXT CHECK(disposal_action IN ('retire', 'store')), -- For pickup_only moves
    reason TEXT,
    notes TEXT,

    -- Assigned shift info (when move is assigned to a driver)
    assigned_shift_id TEXT,
    completed_at BIGINT,

    -- Timestamps
    created_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,
    updated_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,

    -- Foreign keys
    CONSTRAINT fk_bin_move_requests_bin FOREIGN KEY (bin_id) REFERENCES bins(id) ON DELETE CASCADE,
    CONSTRAINT fk_bin_move_requests_user FOREIGN KEY (requested_by) REFERENCES users(id) ON DELETE SET NULL,
    CONSTRAINT fk_bin_move_requests_shift FOREIGN KEY (assigned_shift_id) REFERENCES shifts(id) ON DELETE SET NULL
);

-- Add indexes for performance
CREATE INDEX IF NOT EXISTS idx_bin_move_requests_bin_id ON bin_move_requests(bin_id);
CREATE INDEX IF NOT EXISTS idx_bin_move_requests_status ON bin_move_requests(status);
CREATE INDEX IF NOT EXISTS idx_bin_move_requests_urgency ON bin_move_requests(urgency);
CREATE INDEX IF NOT EXISTS idx_bin_move_requests_scheduled_date ON bin_move_requests(scheduled_date);
CREATE INDEX IF NOT EXISTS idx_bin_move_requests_requested_by ON bin_move_requests(requested_by);
CREATE INDEX IF NOT EXISTS idx_bin_move_requests_assigned_shift ON bin_move_requests(assigned_shift_id);

-- Add composite index for pending urgent moves (most common query)
CREATE INDEX IF NOT EXISTS idx_bin_move_requests_urgent_pending ON bin_move_requests(urgency, status) WHERE urgency = 'urgent' AND status = 'pending';

-- Update bins table to add new status enum values
ALTER TABLE bins DROP CONSTRAINT IF EXISTS bins_status_check;
ALTER TABLE bins ADD CONSTRAINT bins_status_check CHECK(status IN ('active', 'retired', 'in_storage', 'pending_move', 'needs_check'));

-- Add new fields to bins table
ALTER TABLE bins ADD COLUMN IF NOT EXISTS created_by_user_id TEXT;
ALTER TABLE bins ADD COLUMN IF NOT EXISTS last_checked_at BIGINT;

-- Add foreign key for created_by_user_id
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'fk_bins_created_by_user'
    ) THEN
        ALTER TABLE bins ADD CONSTRAINT fk_bins_created_by_user
        FOREIGN KEY (created_by_user_id) REFERENCES users(id) ON DELETE SET NULL;
    END IF;
END $$;

-- Add index for created_by_user_id
CREATE INDEX IF NOT EXISTS idx_bins_created_by_user ON bins(created_by_user_id);
CREATE INDEX IF NOT EXISTS idx_bins_last_checked_at ON bins(last_checked_at);
CREATE INDEX IF NOT EXISTS idx_bins_status ON bins(status);

-- Verify the migration
-- Run this to confirm table exists:
-- SELECT column_name, data_type, is_nullable
-- FROM information_schema.columns
-- WHERE table_name = 'bin_move_requests'
-- ORDER BY ordinal_position;
