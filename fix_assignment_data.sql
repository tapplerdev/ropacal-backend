-- Fix move requests that have status 'assigned' but no actual assignment
-- These records are data inconsistencies that need to be corrected

-- First, let's see what we're dealing with
SELECT id, bin_id, status, assignment_type, assigned_shift_id, assigned_user_id
FROM bin_move_requests
WHERE status = 'assigned'
  AND (assignment_type IS NULL OR assignment_type = '')
  AND assigned_shift_id IS NULL
  AND assigned_user_id IS NULL;

-- Reset these records back to 'pending' since they're not actually assigned
UPDATE bin_move_requests
SET status = 'pending',
    assignment_type = 'shift',
    updated_at = EXTRACT(EPOCH FROM NOW())::BIGINT
WHERE status = 'assigned'
  AND (assignment_type IS NULL OR assignment_type = '')
  AND assigned_shift_id IS NULL
  AND assigned_user_id IS NULL;

-- Also fix any records that have assignment_type = '' (empty string) but DO have valid assignments
UPDATE bin_move_requests
SET assignment_type = 'shift'
WHERE (assignment_type IS NULL OR assignment_type = '')
  AND assigned_shift_id IS NOT NULL;

UPDATE bin_move_requests
SET assignment_type = 'manual'
WHERE (assignment_type IS NULL OR assignment_type = '')
  AND assigned_user_id IS NOT NULL;

-- Verify the fixes
SELECT
    status,
    assignment_type,
    COUNT(*) as count,
    COUNT(assigned_shift_id) as with_shift,
    COUNT(assigned_user_id) as with_user
FROM bin_move_requests
WHERE status IN ('assigned', 'in_progress')
GROUP BY status, assignment_type;
