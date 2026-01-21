-- Migration: Add Manual Move Support
-- Purpose: Enable both shift-based and manual one-off move workflows
-- Date: 2025-01-21

-- Step 1: Add assignment_type to bin_move_requests
-- This tracks whether move is assigned to a shift or manually to a user
ALTER TABLE bin_move_requests
ADD COLUMN assignment_type VARCHAR(20) DEFAULT 'shift'
CHECK(assignment_type IN ('shift', 'manual'));

-- Step 2: Add assigned_user_id to bin_move_requests
-- For manual moves, this tracks which user is responsible (not tied to shift)
ALTER TABLE bin_move_requests
ADD COLUMN assigned_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;

-- Step 3: Create index for assigned_user_id lookups
CREATE INDEX idx_bin_move_requests_assigned_user
ON bin_move_requests(assigned_user_id);

-- Step 4: Enhance moves table with richer context
ALTER TABLE moves
ADD COLUMN move_type VARCHAR(20) DEFAULT 'shift'
CHECK(move_type IN ('shift', 'manual'));

ALTER TABLE moves
ADD COLUMN from_street TEXT;

ALTER TABLE moves
ADD COLUMN from_city TEXT;

ALTER TABLE moves
ADD COLUMN from_zip TEXT;

ALTER TABLE moves
ADD COLUMN to_street TEXT;

ALTER TABLE moves
ADD COLUMN to_city TEXT;

ALTER TABLE moves
ADD COLUMN to_zip TEXT;

ALTER TABLE moves
ADD COLUMN move_request_id TEXT REFERENCES bin_move_requests(id) ON DELETE SET NULL;

ALTER TABLE moves
ADD COLUMN completed_by_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE moves
ADD COLUMN shift_id TEXT REFERENCES shifts(id) ON DELETE SET NULL;

-- Step 5: Create indexes for new moves columns
CREATE INDEX idx_moves_move_type ON moves(move_type);
CREATE INDEX idx_moves_move_request_id ON moves(move_request_id);
CREATE INDEX idx_moves_completed_by_user_id ON moves(completed_by_user_id);
CREATE INDEX idx_moves_shift_id ON moves(shift_id);

-- Step 6: Backfill existing move requests as 'shift' type (already default)
-- No action needed - default value handles this

-- Step 7: Backfill existing moves as 'shift' type (already default)
-- No action needed - default value handles this

-- Migration complete
-- New workflows enabled:
-- 1. Shift-based: assignment_type='shift', assigned_shift_id populated
-- 2. Manual: assignment_type='manual', assigned_user_id populated
