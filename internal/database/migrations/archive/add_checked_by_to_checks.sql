-- Migration: Add checked_by field to checks table
-- Purpose: Track which driver performed each bin check
-- Date: 2025-01-20

-- Step 1: Add checked_by column (user ID who performed the check)
ALTER TABLE checks ADD COLUMN checked_by TEXT;

-- Step 2: Add foreign key constraint (ensures data integrity)
-- ON DELETE SET NULL: If user is deleted, keep the check record but clear the user reference
ALTER TABLE checks
ADD CONSTRAINT fk_checks_user
FOREIGN KEY (checked_by) REFERENCES users(id) ON DELETE SET NULL;

-- Step 3: Add index for faster queries (performance optimization)
CREATE INDEX idx_checks_checked_by ON checks(checked_by);

-- Step 4: Verify the migration
-- Run this to confirm the column exists:
-- SELECT column_name, data_type, is_nullable
-- FROM information_schema.columns
-- WHERE table_name = 'checks'
-- ORDER BY ordinal_position;

-- Expected result should include:
-- checked_by | text | YES
