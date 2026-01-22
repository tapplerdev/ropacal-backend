-- ============================================================================
-- FIX ASSIGNMENT TYPE ISSUES
-- ============================================================================

-- 1. CHECK CURRENT SCHEMA
-- ============================================================================
\d+ bin_move_requests

-- Check the default value for assignment_type column
SELECT column_name, column_default, is_nullable, data_type
FROM information_schema.columns
WHERE table_name = 'bin_move_requests'
  AND column_name = 'assignment_type';

-- 2. VIEW CURRENT DATA STATE
-- ============================================================================
-- See all move requests with their assignment data
SELECT
    id,
    bin_id,
    status,
    assignment_type,
    assigned_shift_id,
    assigned_user_id,
    CASE
        WHEN assigned_shift_id IS NOT NULL THEN 'Has Shift'
        WHEN assigned_user_id IS NOT NULL THEN 'Has User'
        ELSE 'No Assignment'
    END as actual_assignment
FROM bin_move_requests
ORDER BY created_at DESC
LIMIT 20;

-- Count by status and assignment type
SELECT
    status,
    assignment_type,
    COUNT(*) as count
FROM bin_move_requests
GROUP BY status, assignment_type
ORDER BY status, assignment_type;

-- 3. FIX THE SCHEMA
-- ============================================================================
-- Update the column to have a proper default value
ALTER TABLE bin_move_requests
    ALTER COLUMN assignment_type SET DEFAULT 'shift';

-- Make sure it's not nullable
ALTER TABLE bin_move_requests
    ALTER COLUMN assignment_type SET NOT NULL;

-- 4. FIX EXISTING DATA
-- ============================================================================

-- Fix records that have shift assignments but empty assignment_type
UPDATE bin_move_requests
SET assignment_type = 'shift'
WHERE (assignment_type IS NULL OR assignment_type = '')
  AND assigned_shift_id IS NOT NULL;

-- Fix records that have user assignments but empty assignment_type
UPDATE bin_move_requests
SET assignment_type = 'manual'
WHERE (assignment_type IS NULL OR assignment_type = '')
  AND assigned_user_id IS NOT NULL;

-- Fix all remaining empty assignment_type to default 'shift'
UPDATE bin_move_requests
SET assignment_type = 'shift'
WHERE assignment_type IS NULL OR assignment_type = '';

-- 5. VERIFY THE FIXES
-- ============================================================================
-- Should show no empty assignment_type values
SELECT
    id,
    status,
    assignment_type,
    assigned_shift_id IS NOT NULL as has_shift,
    assigned_user_id IS NOT NULL as has_user
FROM bin_move_requests
WHERE assignment_type = '' OR assignment_type IS NULL;

-- Final summary
SELECT
    status,
    assignment_type,
    COUNT(*) as count,
    COUNT(assigned_shift_id) as with_shift,
    COUNT(assigned_user_id) as with_user
FROM bin_move_requests
GROUP BY status, assignment_type
ORDER BY status, assignment_type;

-- ============================================================================
-- NOTES:
-- - Run sections 1-2 first to diagnose
-- - Then run sections 3-4 to fix
-- - Finally run section 5 to verify
-- ============================================================================
