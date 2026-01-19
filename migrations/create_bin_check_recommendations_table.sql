-- Migration: Create bin_check_recommendations table
-- Purpose: Track bins that haven't been checked in over 7 days (simple time-based flagging)
-- Design: No priority field - anything over 7 days is already high priority

CREATE TABLE IF NOT EXISTS bin_check_recommendations (
    id TEXT PRIMARY KEY,
    bin_id TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT 'time_based',
    flagged_at BIGINT NOT NULL,
    days_since_check INT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending', 'resolved', 'dismissed')),
    resolved_at BIGINT,
    resolved_by_user_id TEXT,
    notes TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,

    CONSTRAINT fk_bin_check_recommendations_bin
        FOREIGN KEY (bin_id) REFERENCES bins(id) ON DELETE CASCADE,

    CONSTRAINT fk_bin_check_recommendations_resolved_by
        FOREIGN KEY (resolved_by_user_id) REFERENCES users(id) ON DELETE SET NULL
);

-- Index for efficient queries
CREATE INDEX IF NOT EXISTS idx_bin_check_recommendations_bin_id
    ON bin_check_recommendations(bin_id);

CREATE INDEX IF NOT EXISTS idx_bin_check_recommendations_status
    ON bin_check_recommendations(status);

CREATE INDEX IF NOT EXISTS idx_bin_check_recommendations_flagged_at
    ON bin_check_recommendations(flagged_at DESC);

-- Compound index for finding pending recommendations for specific bins
CREATE INDEX IF NOT EXISTS idx_bin_check_recommendations_bin_status
    ON bin_check_recommendations(bin_id, status);
