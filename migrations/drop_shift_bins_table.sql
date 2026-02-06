-- Migration: Drop legacy shift_bins table
-- Date: 2026-02-06
-- Reason: Replaced by route_tasks table for task-based shift management
--
-- IMPORTANT: Before running this migration:
-- 1. Ensure all shifts are using route_tasks (not shift_bins)
-- 2. Verify bin_move_requests.go has been updated to use route_tasks
-- 3. Backup shift_bins data if needed for historical records
--
-- To verify all shifts have route_tasks:
--   SELECT s.id, s.driver_id,
--          (SELECT COUNT(*) FROM route_tasks WHERE shift_id = s.id) as task_count
--   FROM shifts s
--   WHERE (SELECT COUNT(*) FROM route_tasks WHERE shift_id = s.id) = 0;
--
-- Expected result: 0 rows (all shifts should have tasks)

-- Drop the legacy shift_bins table
DROP TABLE IF EXISTS shift_bins CASCADE;

-- Note: Any foreign key constraints referencing shift_bins have been removed
-- Note: All code references to shift_bins have been updated to use route_tasks
