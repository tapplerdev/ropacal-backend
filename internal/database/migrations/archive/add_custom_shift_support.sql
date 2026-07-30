-- Add support for custom one-off shifts with service tasks and Mapbox v2 time constraints
-- Custom shifts allow drivers to visit locations without bin-specific logic

-- ============================================================
-- SHIFTS TABLE: Add shift type, label, and custom start/end locations
-- ============================================================

-- Shift type discriminator (standard = bin shifts, custom = one-off service shifts)
ALTER TABLE shifts ADD COLUMN IF NOT EXISTS shift_type TEXT NOT NULL DEFAULT 'standard';
ALTER TABLE shifts DROP CONSTRAINT IF EXISTS shifts_shift_type_check;
ALTER TABLE shifts ADD CONSTRAINT shifts_shift_type_check CHECK (shift_type IN ('standard', 'custom'));

-- Display label for custom shifts (e.g., "Morning Inspections")
ALTER TABLE shifts ADD COLUMN IF NOT EXISTS shift_label TEXT;

-- Custom vehicle start location (overrides warehouse for Mapbox optimization)
ALTER TABLE shifts ADD COLUMN IF NOT EXISTS start_latitude DOUBLE PRECISION;
ALTER TABLE shifts ADD COLUMN IF NOT EXISTS start_longitude DOUBLE PRECISION;
ALTER TABLE shifts ADD COLUMN IF NOT EXISTS start_address TEXT;

-- Custom vehicle end location (overrides warehouse for Mapbox optimization)
ALTER TABLE shifts ADD COLUMN IF NOT EXISTS end_latitude DOUBLE PRECISION;
ALTER TABLE shifts ADD COLUMN IF NOT EXISTS end_longitude DOUBLE PRECISION;
ALTER TABLE shifts ADD COLUMN IF NOT EXISTS end_address TEXT;

-- Index for filtering by shift type
CREATE INDEX IF NOT EXISTS idx_shifts_shift_type ON shifts(shift_type);

-- ============================================================
-- ROUTE_TASKS TABLE: Add service task type and time window fields
-- ============================================================

-- Add 'service' to the task_type CHECK constraint
ALTER TABLE route_tasks DROP CONSTRAINT IF EXISTS route_tasks_task_type_check;
ALTER TABLE route_tasks ADD CONSTRAINT route_tasks_task_type_check
    CHECK (task_type IN ('collection', 'placement', 'pickup', 'dropoff', 'warehouse_stop', 'service'));

-- Time window fields for Mapbox v2 service_times
ALTER TABLE route_tasks ADD COLUMN IF NOT EXISTS earliest_arrival TIMESTAMPTZ;
ALTER TABLE route_tasks ADD COLUMN IF NOT EXISTS latest_arrival TIMESTAMPTZ;
ALTER TABLE route_tasks ADD COLUMN IF NOT EXISTS time_window_type TEXT DEFAULT 'soft';
ALTER TABLE route_tasks DROP CONSTRAINT IF EXISTS route_tasks_time_window_type_check;
ALTER TABLE route_tasks ADD CONSTRAINT route_tasks_time_window_type_check
    CHECK (time_window_type IN ('strict', 'soft', 'soft_start', 'soft_end'));

-- Service duration in seconds (how long the driver spends at a stop)
ALTER TABLE route_tasks ADD COLUMN IF NOT EXISTS service_duration_seconds INT DEFAULT 300;

-- Label and description shown to driver for service tasks
ALTER TABLE route_tasks ADD COLUMN IF NOT EXISTS task_label TEXT;
ALTER TABLE route_tasks ADD COLUMN IF NOT EXISTS task_description TEXT;

-- Whether photo is required for task completion
ALTER TABLE route_tasks ADD COLUMN IF NOT EXISTS photo_required BOOLEAN DEFAULT FALSE;

-- Driver notes upon task completion
ALTER TABLE route_tasks ADD COLUMN IF NOT EXISTS completion_notes TEXT;
