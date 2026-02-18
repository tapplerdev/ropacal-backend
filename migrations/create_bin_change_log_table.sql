-- Migration: create bin_change_log table
-- Documents the schema that was manually created in Railway.
-- If applying to a fresh DB, run this. If upgrading existing DB that has
-- the old changed_at column, run the ALTER below instead.

-- Fresh DB:
CREATE TABLE IF NOT EXISTS bin_change_log (
    id                  TEXT PRIMARY KEY,
    bin_id              TEXT NOT NULL REFERENCES bins(id),
    changed_by_user_id  TEXT REFERENCES users(id),
    created_at          BIGINT NOT NULL,   -- Unix epoch seconds
    change_type         TEXT NOT NULL,     -- e.g. 'status_change', 'address_change', 'fill_update'
    old_values          JSONB,
    new_values          JSONB,
    reason_category     TEXT,              -- landlord_complaint, theft, vandalism, missing, relocation_request, pulled_from_service, other
    reason_notes        TEXT,
    no_go_zone_created  BOOLEAN NOT NULL DEFAULT FALSE,
    no_go_zone_id       TEXT REFERENCES no_go_zones(id)
);

CREATE INDEX IF NOT EXISTS idx_bin_change_log_bin_id ON bin_change_log(bin_id);
CREATE INDEX IF NOT EXISTS idx_bin_change_log_created_at ON bin_change_log(created_at DESC);

-- Upgrade existing DB (had changed_at instead of created_at):
-- ALTER TABLE bin_change_log RENAME COLUMN changed_at TO created_at;
