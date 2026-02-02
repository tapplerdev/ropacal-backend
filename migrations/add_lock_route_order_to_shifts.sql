-- Add lock_route_order column to shifts table
-- This allows managers to prevent route optimization and keep exact task order

ALTER TABLE shifts ADD COLUMN IF NOT EXISTS lock_route_order BOOLEAN DEFAULT false;

COMMENT ON COLUMN shifts.lock_route_order IS 'If true, backend skips route optimization and keeps manager''s exact task order. If false (default), backend optimizes route with real-time traffic and dynamic warehouse insertion.';
