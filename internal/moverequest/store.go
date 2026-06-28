package moverequest

import (
	"database/sql"
	"errors"

	"ropacal-backend/internal/models"

	"github.com/jmoiron/sqlx"
)

// ErrNotFound is returned by Store reads when no move-request matches.
var ErrNotFound = errors.New("move request not found")

// Store is the data-access seam for the move-request domain. It is
// consumer-defined: it lists only the operations the domain's handlers and
// services actually need, so consumers can be unit-tested against a fake without
// a database (see the ShiftStore precedent).
//
// Operations that must run inside a caller's transaction (e.g. ReleaseFromShift,
// which participates in the shift-end tx) deliberately stay as package functions
// taking sqlx.Ext rather than Store methods — the Store is for standalone reads
// and writes on the pool.
type Store interface {
	// ByID returns the move-request with the given id, or ErrNotFound.
	ByID(id string) (*models.BinMoveRequest, error)
	// EditByID returns a move plus the joined shift/bin context the editor needs
	// (shift status/driver, waypoint count, bin number), or ErrNotFound.
	EditByID(id string) (*EditView, error)
	// Create inserts a new move-request row (the lifecycle entry point). The
	// reasonCategory is stored separately (it is not a model field); any no-go
	// zone is linked by the caller after insert.
	Create(m *models.BinMoveRequest, reasonCategory *string) error
	// ActiveWithBin returns every not-yet-completed move (pending/assigned/
	// in_progress) joined with its bin number — the watcher's working set.
	ActiveWithBin() ([]ActionableMove, error)
	// ResponsibleDriver returns the driver id + name on the hook for a move:
	// assigned_user_id (manual) or the shift's driver (shift-assigned). Both
	// empty (nil error) when the move is in the pool. This is the single home for
	// the "who owns this move" rule — derived live, so no denormalization drift.
	ResponsibleDriver(m *models.BinMoveRequest) (driverID, driverName string, err error)
}

// EditView is a move plus the joined shift/bin context the multi-field editor
// (UpdateBinMoveRequest) needs in one read.
type EditView struct {
	models.BinMoveRequest
	ShiftStatus     *string `db:"shift_status"`
	ShiftDriverName *string `db:"shift_driver_name"`
	TotalWaypoints  *int    `db:"total_waypoints"`
	BinNumber       int     `db:"bin_number"`
}

// sqlStore is the production Store backed by a PostgreSQL pool.
type sqlStore struct{ db *sqlx.DB }

// NewSQLStore returns a Store backed by the given database pool.
func NewSQLStore(db *sqlx.DB) Store { return &sqlStore{db: db} }

func (s *sqlStore) ByID(id string) (*models.BinMoveRequest, error) {
	var mr models.BinMoveRequest
	if err := s.db.Get(&mr, `SELECT * FROM bin_move_requests WHERE id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &mr, nil
}

func (s *sqlStore) EditByID(id string) (*EditView, error) {
	var v EditView
	err := s.db.Get(&v, `
		SELECT
			mr.*,
			b.bin_number,
			s.status as shift_status,
			u.name as shift_driver_name,
			(SELECT COUNT(*) FROM route_tasks WHERE shift_id = mr.assigned_shift_id) as total_waypoints
		FROM bin_move_requests mr
		LEFT JOIN bins b ON mr.bin_id = b.id
		LEFT JOIN shifts s ON mr.assigned_shift_id = s.id
		LEFT JOIN users u ON s.driver_id = u.id
		WHERE mr.id = $1`, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &v, nil
}

func (s *sqlStore) Create(m *models.BinMoveRequest, reasonCategory *string) error {
	_, err := s.db.Exec(`
		INSERT INTO bin_move_requests (
			id, bin_id, scheduled_date, urgency, requested_by, status,
			original_latitude, original_longitude, original_address,
			new_latitude, new_longitude, new_address,
			move_type, disposal_action, reason, notes,
			source_potential_location_id,
			assignment_type, assigned_shift_id,
			created_at, updated_at,
			reason_category, no_go_zone_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)`,
		m.ID, m.BinID, m.ScheduledDate, m.Urgency, m.RequestedBy, m.Status,
		m.OriginalLatitude, m.OriginalLongitude, m.OriginalAddress,
		m.NewLatitude, m.NewLongitude, m.NewAddress,
		m.MoveType, m.DisposalAction, m.Reason, m.Notes,
		m.SourcePotentialLocationID,
		m.AssignmentType, m.AssignedShiftID,
		m.CreatedAt, m.UpdatedAt,
		reasonCategory, nil, // no_go_zone_id linked after insert
	)
	return err
}

func (s *sqlStore) ResponsibleDriver(m *models.BinMoveRequest) (driverID, driverName string, err error) {
	switch {
	case m.AssignedUserID != nil && *m.AssignedUserID != "":
		driverID = *m.AssignedUserID
		err = s.db.QueryRow(`SELECT name FROM users WHERE id = $1`, driverID).Scan(&driverName)
	case m.AssignedShiftID != nil && *m.AssignedShiftID != "":
		err = s.db.QueryRow(
			`SELECT u.id, u.name FROM shifts s JOIN users u ON s.driver_id = u.id WHERE s.id = $1`,
			*m.AssignedShiftID,
		).Scan(&driverID, &driverName)
	}
	return driverID, driverName, err
}

func (s *sqlStore) ActiveWithBin() ([]ActionableMove, error) {
	// 'assigned' (claimed by a driver/not-yet-active shift, but not in_progress)
	// must be included, or backlog moves get no overdue/due-soon alerts.
	var moves []ActionableMove
	err := s.db.Select(&moves, `
		SELECT bmr.id, bmr.bin_id, b.bin_number, bmr.scheduled_date, bmr.status
		FROM bin_move_requests bmr
		JOIN bins b ON bmr.bin_id = b.id
		WHERE bmr.status IN ('pending', 'assigned', 'in_progress')`)
	return moves, err
}
