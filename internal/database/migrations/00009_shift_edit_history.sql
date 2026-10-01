-- Shift edit timeline: who changed a shift, what changed, and why.
--
-- Until now a shift's edits were only audited ROW BY ROW on route_tasks
-- (added_by / addition_reason / deleted_by / deletion_reason), plus the move
-- side in move_request_history. There was no way to ask "what happened to this
-- shift?" and get one ordered answer — shift_history is the completed-shift
-- archive, and `shift_edited` is an ephemeral Centrifugo event that nobody can
-- read after the fact.
--
-- Written by the itinerary domain, which is the single writer of route_tasks,
-- so every path that adds or removes a task goes through one choke point and
-- cannot skip the log: manager edits, move cancels and reassignments, bin
-- edits superseding a move, reassign cleanup.
--
-- WHAT IS DELIBERATELY NOT LOGGED. A mid-shift re-optimize soft-deletes every
-- live task and re-inserts it in the new order; warehouse stops are regenerated
-- on every optimize. Both are optimizer mechanics, not edits, and logging them
-- would bury the real events under N removals and N re-additions per reroute.
--
-- APPEND-ONLY. Rows are never updated; a correction is a new event.

-- +goose Up
-- +goose StatementBegin

-- The composite foreign key below locks `shifts` (SHARE ROW EXCLUSIVE) for the
-- rest of this transaction. If a writer holds `shifts` open, waiting for that
-- lock would queue EVERY later shift write behind it — shift starts, ends,
-- edits — for as long as the blocker lasts. Fail fast instead: the boot fails,
-- the deploy restarts, and the previous instance keeps serving meanwhile.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS shift_edit_history (
    id               TEXT PRIMARY KEY DEFAULT (gen_random_uuid())::text,
    -- Defaults to the session's tenant, like every other tenant table, but the
    -- writers set it explicitly from the route_tasks / shifts row they read, so
    -- a write never depends on session state that might be missing.
    organization_id  TEXT NOT NULL DEFAULT NULLIF(current_setting('app.org_id', true), ''),
    shift_id         TEXT NOT NULL,

    -- Orders events that land in the same second, which a single request
    -- routinely produces (a move's pickup and dropoff are added together).
    seq              BIGINT GENERATED ALWAYS AS IDENTITY,

    event_type       TEXT NOT NULL,

    -- Task-level events carry the task they concern. NULL for shift-level
    -- events (created, driver_reassigned).
    task_id          TEXT,
    task_type        TEXT,
    bin_number       INTEGER,
    move_request_id  TEXT,

    -- users.id of whoever made the change; NULL when the system did. Not a
    -- foreign key: a deleted user must not erase what they did.
    actor_id         TEXT,
    -- Machine reason codes (move_request_cancelled, superseded_by_manual_bin_edit)
    -- or a manager's free text. Stored raw; the dashboard humanizes the codes.
    reason           TEXT,
    metadata         JSONB,
    created_at       BIGINT NOT NULL,

    -- Kept in step with itinerary.EditEvent. The Go side is typed precisely so
    -- this can never fire at runtime: a CHECK violation inside a transaction
    -- aborts the EDIT, not just the log line.
    CONSTRAINT shift_edit_history_event_type_check
        CHECK (event_type IN ('created', 'task_added', 'task_removed', 'driver_reassigned')),

    -- COMPOSITE, like every other foreign key to shifts in this schema
    -- (uq_shifts_org_id). A plain shift_id -> shifts(id) would let a row in one
    -- organization reference another organization's shift; keying on
    -- (organization_id, id) makes that structurally impossible rather than
    -- something RLS has to catch. CASCADE, not the SET NULL the other tables
    -- use: an audit row with no shift has nothing left to describe.
    CONSTRAINT shift_edit_history_shift_fkey
        FOREIGN KEY (organization_id, shift_id)
        REFERENCES shifts (organization_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_shift_edit_history_shift
    ON shift_edit_history (shift_id, created_at, seq);

ALTER TABLE shift_edit_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE shift_edit_history FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS shift_edit_history_tenant ON shift_edit_history;
CREATE POLICY shift_edit_history_tenant ON shift_edit_history
    USING (organization_id = NULLIF(current_setting('app.org_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('app.org_id', true), ''));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS shift_edit_history;
-- +goose StatementEnd
