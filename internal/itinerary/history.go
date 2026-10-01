package itinerary

import (
	"encoding/json"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// The shift edit timeline: who changed a shift, what changed, and why.
//
// Shift edits used to be audited only row by row on route_tasks (added_by /
// deleted_by / their reasons), so there was no single ordered answer to "what
// happened to this shift?". Because itinerary is the ONLY writer of
// route_tasks, logging here means every path that adds or removes a task —
// manager edits, move cancels and reassignments, bin edits superseding a move,
// reassign cleanup — records itself without any caller having to remember to.
//
// What is deliberately NOT an edit, and never logged:
//   - optimizer mechanics: a mid-shift re-optimize soft-deletes every live task
//     and re-inserts it in the new order (RemoveAllLive + ApplyOrder inserts),
//     and warehouse stops are regenerated on every optimize. Logging those
//     would bury each real edit under N removals and N re-additions.
//   - tasks created WITH the shift. The shift's birth is one `created` event,
//     not one event per task.
//
// All writes run in the caller's transaction, so an edit and its history row
// commit or roll back together.

// EditEvent is a shift_edit_history.event_type.
//
// Typed, and kept in step with the table's CHECK constraint, so an unknown
// value cannot reach the database: a CHECK violation inside a transaction
// aborts the whole EDIT, not just its log line.
type EditEvent string

const (
	EditCreated          EditEvent = "created"
	EditTaskAdded        EditEvent = "task_added"
	EditTaskRemoved      EditEvent = "task_removed"
	EditDriverReassigned EditEvent = "driver_reassigned"
)

// editEvents is every EditEvent, in one place, so a test can hold it against
// the migration's CHECK list in BOTH directions. Add a constant, add it here.
var editEvents = []EditEvent{EditCreated, EditTaskAdded, EditTaskRemoved, EditDriverReassigned}

// SystemActor is the actor recorded by system-initiated writes. Stored as NULL.
const SystemActor = "system"

// actorOrNull maps "" and SystemActor to SQL NULL, so actor_id only ever holds
// a real users.id and the read side can left-join users without special cases.
func actorOrNull(actorID string) any {
	if actorID == "" || actorID == SystemActor {
		return nil
	}
	return actorID
}

// logTasksAdded records tasks added to an EXISTING shift. It reads each task's
// details back from route_tasks rather than taking them as arguments, so the
// history can never disagree with the row it describes. Warehouse stops are
// skipped — they are optimizer artifacts, not edits.
//
// organization_id is copied from the task row, not taken from the session, so
// the write does not depend on session state; the table's WITH CHECK still
// rejects it if the two ever disagree.
//
// Matched on the uuid column itself (the ids are the writers' own uuids), so
// the primary key serves the lookup; comparing id::text would scan the table.
func logTasksAdded(ext sqlx.Ext, taskIDs []string, actorID string, reason *string, now int64) error {
	if len(taskIDs) == 0 {
		return nil
	}
	q, args, err := sqlx.In(`
		INSERT INTO shift_edit_history
			(organization_id, shift_id, event_type, task_id, task_type, bin_number,
			 move_request_id, actor_id, reason, created_at)
		SELECT organization_id, shift_id, ?, id::text, task_type, bin_number,
			   move_request_id, ?, ?, ?
		  FROM route_tasks
		 WHERE id IN (?) AND task_type <> 'warehouse_stop'`,
		string(EditTaskAdded), actorOrNull(actorID), reason, now, taskIDs)
	if err != nil {
		return fmt.Errorf("log tasks added: %w", err)
	}
	if _, err := ext.Exec(ext.Rebind(q), args...); err != nil {
		return fmt.Errorf("log tasks added: %w", err)
	}
	return nil
}

// LogShiftCreated records a shift's birth as ONE event, rather than one
// task_added per task it was created with.
//
// Call it after the shift's counts are computed. The bin count is read from
// the shift row itself — the same logical-bin count (a relocation's two legs
// count once) the dashboard shows for the shift everywhere else — so the
// timeline cannot disagree with the shift it describes.
func LogShiftCreated(ext sqlx.Ext, shiftID, actorID string, now int64) error {
	if _, err := ext.Exec(ext.Rebind(`
		INSERT INTO shift_edit_history
			(organization_id, shift_id, event_type, actor_id, metadata, created_at)
		SELECT organization_id, id, ?, ?, jsonb_build_object('bin_count', total_bins), ?
		  FROM shifts WHERE id = ?`),
		string(EditCreated), actorOrNull(actorID), now, shiftID); err != nil {
		return fmt.Errorf("log shift created: %w", err)
	}
	return nil
}

// LogDriverReassigned records a shift handed from one driver to another.
// Both driver ids go in metadata; the read side resolves them to names.
func LogDriverReassigned(ext sqlx.Ext, shiftID, actorID, fromDriverID, toDriverID string, now int64) error {
	meta, _ := json.Marshal(map[string]string{"from_driver_id": fromDriverID, "to_driver_id": toDriverID})
	if _, err := ext.Exec(ext.Rebind(`
		INSERT INTO shift_edit_history
			(organization_id, shift_id, event_type, actor_id, metadata, created_at)
		SELECT organization_id, id, ?, ?, ?, ?
		  FROM shifts WHERE id = ?`),
		string(EditDriverReassigned), actorOrNull(actorID), string(meta), now, shiftID); err != nil {
		return fmt.Errorf("log driver reassigned: %w", err)
	}
	return nil
}

// Edit is one row of a shift's timeline, as served to the dashboard.
type Edit struct {
	ID             string  `db:"id" json:"id"`
	EventType      string  `db:"event_type" json:"event_type"`
	TaskID         *string `db:"task_id" json:"task_id,omitempty"`
	TaskType       *string `db:"task_type" json:"task_type,omitempty"`
	BinNumber      *int    `db:"bin_number" json:"bin_number,omitempty"`
	MoveRequestID  *string `db:"move_request_id" json:"move_request_id,omitempty"`
	ActorID        *string `db:"actor_id" json:"actor_id,omitempty"`
	ActorName      *string `db:"actor_name" json:"actor_name,omitempty"` // NULL = system
	Reason         *string `db:"reason" json:"reason,omitempty"`
	FromDriverName *string `db:"from_driver_name" json:"from_driver_name,omitempty"`
	ToDriverName   *string `db:"to_driver_name" json:"to_driver_name,omitempty"`
	BinCount       *int    `db:"bin_count" json:"bin_count,omitempty"`
	CreatedAt      int64   `db:"created_at" json:"created_at"`
}

// Reader is what ListEdits reads through. A handler passes its org-bound
// *orgdb.DB, whose Select runs in its own short transaction carrying the
// tenant; a *sqlx.Tx works too.
type Reader interface {
	Select(dest interface{}, query string, args ...interface{}) error
	Rebind(query string) string
}

// ListEdits returns a shift's timeline, oldest first. seq breaks ties within a
// second — a single request routinely writes several events at once.
//
// Names are resolved at read time against users, so a renamed user shows their
// current name. A deleted user's events survive (actor_id is not a foreign key)
// and simply read back with no name.
func ListEdits(db Reader, shiftID string) ([]Edit, error) {
	var out []Edit
	err := db.Select(&out, db.Rebind(`
		SELECT h.id, h.event_type, h.task_id, h.task_type, h.bin_number, h.move_request_id,
		       h.actor_id, actor.name AS actor_name, h.reason,
		       fd.name AS from_driver_name, td.name AS to_driver_name,
		       (h.metadata->>'bin_count')::int AS bin_count,
		       h.created_at
		  FROM shift_edit_history h
		  LEFT JOIN users actor ON actor.id = h.actor_id
		  LEFT JOIN users fd    ON fd.id    = h.metadata->>'from_driver_id'
		  LEFT JOIN users td    ON td.id    = h.metadata->>'to_driver_id'
		 WHERE h.shift_id = ?
		 ORDER BY h.created_at, h.seq`), shiftID)
	if err != nil {
		return nil, fmt.Errorf("list shift edits: %w", err)
	}
	if out == nil {
		out = []Edit{} // an empty timeline is [], not null, on the wire
	}
	return out, nil
}
