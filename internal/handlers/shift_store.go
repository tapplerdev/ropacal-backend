package handlers

import (
	"context"

	"ropacal-backend/internal/database"
	"ropacal-backend/internal/models"

	"github.com/jmoiron/sqlx"
)

// ShiftStore is the data-access seam for the shift aggregate. Handlers depend on
// this interface rather than a concrete *sqlx.DB, so they can be unit-tested with
// a fake store (no live Postgres). This mirrors the consumer-defined interface
// pattern already used by Optimizer / RoadSnapper.
//
// It is intentionally small — methods are added as handlers are migrated onto it,
// not all at once.
type ShiftStore interface {
	// CurrentByDriver returns the driver's current shift: an active/paused shift
	// (any date) or a ready shift scheduled for today or earlier. Returns
	// sql.ErrNoRows when the driver has no current shift.
	CurrentByDriver(ctx context.Context, driverID, today string) (*models.Shift, error)
	// TasksWithDetails returns the legacy bin-detail view for a shift.
	TasksWithDetails(ctx context.Context, shiftID string) ([]models.ShiftBinWithDetails, error)
	// Tasks returns the route_tasks for a shift.
	Tasks(ctx context.Context, shiftID string) ([]models.RouteTask, error)
}

// sqlShiftStore is the Postgres-backed ShiftStore.
type sqlShiftStore struct {
	db *sqlx.DB
}

// NewSQLShiftStore returns a ShiftStore backed by the given database handle.
func NewSQLShiftStore(db *sqlx.DB) *sqlShiftStore {
	return &sqlShiftStore{db: db}
}

// currentShiftQuery is the exact query previously inlined in GetCurrentShift —
// preserved verbatim so the endpoint's behavior is unchanged.
const currentShiftQuery = `SELECT * FROM shifts
	WHERE driver_id = $1
	  AND status IN ('active', 'paused', 'ready')
	  AND (status IN ('active', 'paused') OR scheduled_date IS NULL OR scheduled_date <= $2)
	ORDER BY
	  CASE status
	    WHEN 'active' THEN 1
	    WHEN 'paused' THEN 2
	    WHEN 'ready' THEN 3
	  END ASC,
	  created_at DESC
	LIMIT 1`

func (s *sqlShiftStore) CurrentByDriver(ctx context.Context, driverID, today string) (*models.Shift, error) {
	var shift models.Shift
	if err := s.db.GetContext(ctx, &shift, currentShiftQuery, driverID, today); err != nil {
		return nil, err
	}
	return &shift, nil
}

func (s *sqlShiftStore) TasksWithDetails(ctx context.Context, shiftID string) ([]models.ShiftBinWithDetails, error) {
	return getShiftTasksWithDetails(s.db, shiftID)
}

func (s *sqlShiftStore) Tasks(ctx context.Context, shiftID string) ([]models.RouteTask, error) {
	return database.GetShiftTasks(s.db, shiftID)
}
