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
	// ByIDForDriver returns a specific shift owned by the given driver (the
	// driver_id scope enforces ownership). Returns an error if not found.
	ByIDForDriver(ctx context.Context, shiftID, driverID string) (*models.Shift, error)
	// PauseByDriver flips the driver's ACTIVE shift to paused and returns the
	// updated shift. Returns sql.ErrNoRows if the driver has no active shift.
	PauseByDriver(ctx context.Context, driverID string, now int64) (*models.Shift, error)
	// PausedByDriver returns the driver's paused shift, or sql.ErrNoRows if none.
	PausedByDriver(ctx context.Context, driverID string) (*models.Shift, error)
	// ResumeByID flips a shift back to active, setting total_pause_seconds, and
	// returns the updated shift.
	ResumeByID(ctx context.Context, shiftID string, totalPauseSeconds, now int64) (*models.Shift, error)
	// TasksWithDetails returns the legacy bin-detail view for a shift.
	TasksWithDetails(ctx context.Context, shiftID string) ([]models.ShiftBinWithDetails, error)
	// Tasks returns the route_tasks for a shift.
	Tasks(ctx context.Context, shiftID string) ([]models.RouteTask, error)
}

// currentShiftResponse is the typed `data` payload of GET /api/driver/shift/current.
// It replaces an ad-hoc map[string]interface{} so the response shape is
// compile-checked and documented in one place. Fields are declared in the same
// (alphabetical) order the map serialized them, and use NO omitempty, so the JSON
// is byte-identical to the previous output — nullable fields still emit as null.
type currentShiftResponse struct {
	CompletedBins     int                `json:"completed_bins"`
	CreatedAt         int64              `json:"created_at"`
	DriverID          string             `json:"driver_id"`
	EndTime           *int64             `json:"end_time"`
	ID                string             `json:"id"`
	PauseStartTime    *int64             `json:"pause_start_time"`
	RouteID           *string            `json:"route_id"`
	StartTime         *int64             `json:"start_time"`
	Status            models.ShiftStatus `json:"status"`
	Tasks             []models.RouteTask `json:"tasks"`
	TotalBins         int                `json:"total_bins"`
	TotalPauseSeconds int                `json:"total_pause_seconds"`
	UpdatedAt         int64              `json:"updated_at"`
}

// shiftDetailsResponse is the typed `data` payload of GET /api/driver/shift-details.
// NOTE: deliberately distinct from currentShiftResponse — this endpoint omits
// pause_start_time (12 keys, not 13). Alphabetical order + no omitempty keep the
// JSON byte-identical to the previous map output.
type shiftDetailsResponse struct {
	CompletedBins     int                `json:"completed_bins"`
	CreatedAt         int64              `json:"created_at"`
	DriverID          string             `json:"driver_id"`
	EndTime           *int64             `json:"end_time"`
	ID                string             `json:"id"`
	RouteID           *string            `json:"route_id"`
	StartTime         *int64             `json:"start_time"`
	Status            models.ShiftStatus `json:"status"`
	Tasks             []models.RouteTask `json:"tasks"`
	TotalBins         int                `json:"total_bins"`
	TotalPauseSeconds int                `json:"total_pause_seconds"`
	UpdatedAt         int64              `json:"updated_at"`
}

// pauseResponse / resumeResponse are the typed `data` payloads for the pause and
// resume endpoints (replacing ad-hoc maps).
type pauseResponse struct {
	PauseStartTime *int64             `json:"pause_start_time"`
	Status         models.ShiftStatus `json:"status"`
}

type resumeResponse struct {
	Status            models.ShiftStatus `json:"status"`
	TotalPauseSeconds int                `json:"total_pause_seconds"`
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

func (s *sqlShiftStore) ByIDForDriver(ctx context.Context, shiftID, driverID string) (*models.Shift, error) {
	var shift models.Shift
	if err := s.db.GetContext(ctx, &shift, `SELECT * FROM shifts WHERE id = $1 AND driver_id = $2`, shiftID, driverID); err != nil {
		return nil, err
	}
	return &shift, nil
}

// PauseByDriver fixes the long-standing bug where the UPDATE reused $1 for both
// pause_start_time and driver_id (so it always errored). Correct params: $1/$2
// timestamps, $3 driver_id. RETURNING * gives the updated row in one round-trip.
func (s *sqlShiftStore) PauseByDriver(ctx context.Context, driverID string, now int64) (*models.Shift, error) {
	var shift models.Shift
	err := s.db.GetContext(ctx, &shift, `UPDATE shifts
		SET status = 'paused', pause_start_time = $1, updated_at = $2
		WHERE driver_id = $3 AND status = 'active'
		RETURNING *`, now, now, driverID)
	if err != nil {
		return nil, err
	}
	return &shift, nil
}

func (s *sqlShiftStore) PausedByDriver(ctx context.Context, driverID string) (*models.Shift, error) {
	var shift models.Shift
	if err := s.db.GetContext(ctx, &shift, `SELECT * FROM shifts WHERE driver_id = $1 AND status = 'paused'`, driverID); err != nil {
		return nil, err
	}
	return &shift, nil
}

func (s *sqlShiftStore) ResumeByID(ctx context.Context, shiftID string, totalPauseSeconds, now int64) (*models.Shift, error) {
	var shift models.Shift
	err := s.db.GetContext(ctx, &shift, `UPDATE shifts
		SET status = 'active', total_pause_seconds = $1, pause_start_time = NULL, updated_at = $2
		WHERE id = $3
		RETURNING *`, totalPauseSeconds, now, shiftID)
	if err != nil {
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
