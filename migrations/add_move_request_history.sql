-- Migration: Add Move Request History Tracking
-- Purpose: Track all actions and changes made to move requests for audit trail
-- Date: 2026-01-27

-- Create move_request_history table
CREATE TABLE IF NOT EXISTS move_request_history (
    id TEXT PRIMARY KEY DEFAULT gen_random_uuid()::TEXT,
    move_request_id TEXT NOT NULL,

    -- Action information
    action_type VARCHAR(20) NOT NULL CHECK(action_type IN ('created', 'assigned', 'reassigned', 'unassigned', 'completed', 'cancelled', 'updated')),
    actor_id TEXT NOT NULL,
    actor_name TEXT NOT NULL,
    actor_role VARCHAR(20), -- 'manager', 'driver', 'system'

    -- State snapshots (what changed)
    previous_status VARCHAR(20),
    new_status VARCHAR(20),
    previous_assignment_type VARCHAR(20),
    new_assignment_type VARCHAR(20),
    previous_assigned_user_id TEXT,
    new_assigned_user_id TEXT,
    previous_assigned_user_name TEXT,
    new_assigned_user_name TEXT,
    previous_assigned_shift_id TEXT,
    new_assigned_shift_id TEXT,

    -- Additional context
    notes TEXT, -- Optional reason/description
    metadata JSONB, -- Flexible field for additional data

    -- Timestamp
    created_at BIGINT NOT NULL DEFAULT EXTRACT(EPOCH FROM NOW())::BIGINT,

    -- Foreign keys
    CONSTRAINT fk_move_request_history_move_request
        FOREIGN KEY (move_request_id)
        REFERENCES bin_move_requests(id)
        ON DELETE CASCADE,

    CONSTRAINT fk_move_request_history_actor
        FOREIGN KEY (actor_id)
        REFERENCES users(id)
        ON DELETE SET NULL
);

-- Indexes for performance
CREATE INDEX IF NOT EXISTS idx_move_request_history_move_id
    ON move_request_history(move_request_id);

CREATE INDEX IF NOT EXISTS idx_move_request_history_created_at
    ON move_request_history(created_at);

CREATE INDEX IF NOT EXISTS idx_move_request_history_actor
    ON move_request_history(actor_id);

CREATE INDEX IF NOT EXISTS idx_move_request_history_action_type
    ON move_request_history(action_type);

-- Composite index for querying move history chronologically
CREATE INDEX IF NOT EXISTS idx_move_request_history_move_time
    ON move_request_history(move_request_id, created_at DESC);

-- Comments for documentation
COMMENT ON TABLE move_request_history IS 'Audit trail of all actions performed on move requests';
COMMENT ON COLUMN move_request_history.action_type IS 'Type of action: created, assigned, reassigned, unassigned, completed, cancelled, updated';
COMMENT ON COLUMN move_request_history.actor_id IS 'User who performed the action';
COMMENT ON COLUMN move_request_history.metadata IS 'Flexible JSON field for additional context (e.g., old/new locations, reason codes)';

-- Verify migration
-- SELECT * FROM move_request_history LIMIT 1;
