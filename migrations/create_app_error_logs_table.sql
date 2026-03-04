-- Create app_error_logs table for tracking mobile app errors (navigation, GPS, sync, etc.)
CREATE TABLE IF NOT EXISTS app_error_logs (
    id                  TEXT PRIMARY KEY,
    driver_id           TEXT REFERENCES users(id) ON DELETE SET NULL,
    shift_id            TEXT REFERENCES shifts(id) ON DELETE SET NULL,
    task_id             TEXT,
    created_at          BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,
    log_timestamp       BIGINT NOT NULL,          -- Client-side timestamp (ms since epoch)
    context             TEXT NOT NULL,             -- "navigation", "map_load", "gps", "sync", "route_calculation", etc.
    error_type          TEXT NOT NULL,             -- Categorized error type (e.g., "invalid_waypoints", "gps_unavailable")
    error_message       TEXT NOT NULL,
    severity            TEXT NOT NULL CHECK(severity IN ('critical', 'error', 'warning', 'info')),
    platform            TEXT NOT NULL CHECK(platform IN ('ios', 'android')),
    app_version         TEXT,
    os_version          TEXT,
    device_info         TEXT,
    last_gps_latitude   DOUBLE PRECISION,
    last_gps_longitude  DOUBLE PRECISION,
    stack_trace         TEXT,
    metadata            JSONB,                     -- Flexible field for additional context (waypoints, route details, etc.)
    is_resolved         BOOLEAN DEFAULT FALSE,
    resolved_at         BIGINT,
    resolved_by_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    notes               TEXT
);

-- Indexes for efficient querying
CREATE INDEX IF NOT EXISTS idx_app_error_logs_driver_id ON app_error_logs(driver_id);
CREATE INDEX IF NOT EXISTS idx_app_error_logs_created_at ON app_error_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_app_error_logs_error_type ON app_error_logs(error_type);
CREATE INDEX IF NOT EXISTS idx_app_error_logs_severity ON app_error_logs(severity);
CREATE INDEX IF NOT EXISTS idx_app_error_logs_context ON app_error_logs(context);
CREATE INDEX IF NOT EXISTS idx_app_error_logs_shift_id ON app_error_logs(shift_id);
CREATE INDEX IF NOT EXISTS idx_app_error_logs_is_resolved ON app_error_logs(is_resolved);
