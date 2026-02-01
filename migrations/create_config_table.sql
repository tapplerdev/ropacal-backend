-- Create config table for application settings
-- This table stores key-value configuration data
CREATE TABLE IF NOT EXISTS config (
    id SERIAL PRIMARY KEY,
    key VARCHAR(255) UNIQUE NOT NULL,
    value JSONB NOT NULL,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_by VARCHAR(255),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Create index on key for fast lookups
CREATE INDEX IF NOT EXISTS idx_config_key ON config(key);

-- Insert default warehouse location (San Jose, CA)
INSERT INTO config (key, value, updated_by)
VALUES (
    'warehouse_location',
    '{
        "latitude": 37.3009357,
        "longitude": -121.9493848,
        "address": "1185 Campbell Ave, San Jose, CA 95126, United States"
    }'::jsonb,
    'system'
)
ON CONFLICT (key) DO NOTHING;

-- Add comment for documentation
COMMENT ON TABLE config IS 'Application configuration settings stored as key-value pairs';
COMMENT ON COLUMN config.key IS 'Unique configuration key (e.g., warehouse_location, app_settings)';
COMMENT ON COLUMN config.value IS 'JSONB value for flexible configuration storage';
