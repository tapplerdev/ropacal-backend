package moverequest

import (
	"database/sql"
	"errors"

	"ropacal-backend/internal/models"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// ErrNotFound is returned by Store reads when no move-request matches.
var ErrNotFound = errors.New("move request not found")

// ErrOpenMoveExists is returned by Create when the bin already has an open
// (pending/assigned/in_progress) move request. The invariant is one open move
// per bin — a bin is a single physical object, and two concurrent open moves
// would hand two drivers contradictory instructions and freeze stale origin
// coordinates into the second move. Enforced at the DB by the
// uidx_bin_move_requests_one_open partial unique index (database.go); handlers
// map this to HTTP 409.
var ErrOpenMoveExists = errors.New("bin already has an open move request")

// oneOpenMoveIndex is the partial unique index backing ErrOpenMoveExists.
const oneOpenMoveIndex = "uidx_bin_move_requests_one_open"

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
	// zone is linked by the caller after insert. Returns ErrOpenMoveExists when
	// the one-open-move-per-bin index rejects the insert (concurrent create race).
	Create(m *models.BinMoveRequest, reasonCategory *string) error
	// ActiveWithBin returns every not-yet-completed move (pending/assigned/
	// in_progress) joined with its bin number — the watcher's working set.
	ActiveWithBin() ([]ActionableMove, error)
	// ActiveForBin returns one bin's open (non-terminal) moves with the display
	// context clients need (type, destination, responsible driver). Single home
	// for the "does this bin already have an open move?" question — used by the
	// schedule-move 409 guard and the active-move-requests endpoint.
	ActiveForBin(binID string) ([]ActiveMove, error)
	// ResponsibleDriver returns the driver id + name on the hook for a move:
	// assigned_user_id (manual) or the shift's driver (shift-assigned). Both
	// empty (nil error) when the move is in the pool. This is the single home for
	// the "who owns this move" rule — derived live, so no denormalization drift.
	ResponsibleDriver(m *models.BinMoveRequest) (driverID, driverName string, err error)
}

// ActiveMove is one of a bin's open (pending/assigned/in_progress) move
// requests, shaped for client display: what kind of move, where it's headed,
// and who is on the hook for it. JSON tags match the active-move-requests
// endpoint contract (the dashboard's supersede banner and the schedule 409).
type ActiveMove struct {
	ID                 string  `db:"id" json:"id"`
	MoveType           string  `db:"move_type" json:"move_type"`
	Status             string  `db:"status" json:"status"`
	NewAddress         string  `db:"new_address" json:"new_address"`
	DisposalAction     *string `db:"disposal_action" json:"disposal_action,omitempty"`
	AssignedShiftID    *string `db:"assigned_shift_id" json:"assigned_shift_id,omitempty"`
	AssignedDriverName string  `db:"assigned_driver_name" json:"assigned_driver_name"`
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

// DB is the database handle the move-request domain needs. It is satisfied by
// both *sqlx.DB (single-tenant / tests) and *orgdb.DB (org-scoped requests and
// per-org worker iterations) — handlers rebuild the store per request around
// their org-bound handle (`moverequest.NewSQLStore(db)` after the orgdb
// shadow), so every query the store issues carries app.org_id under RLS.
type DB interface {
	sqlx.Ext
	Get(dest interface{}, query string, args ...interface{}) error
	Select(dest interface{}, query string, args ...interface{}) error
}

// sqlStore is the production Store backed by a PostgreSQL handle.
type sqlStore struct{ db DB }

// NewSQLStore returns a Store backed by the given database handle.
func NewSQLStore(db DB) Store { return &sqlStore{db: db} }

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
	return Insert(s.db, m, reasonCategory)
}

// Insert writes a new move-request row on the caller's executor — the shared
// core of Store.Create (pool) and in-transaction minting (e.g. the shift
// builder turning warehouse deployments into redeployment moves).
func Insert(ext sqlx.Ext, m *models.BinMoveRequest, reasonCategory *string) error {
	_, err := ext.Exec(`
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
	// A violation of the one-open-move partial unique index means a concurrent
	// create slipped past the handler's pre-check — surface it as the typed
	// domain error so the handler can 409 instead of 500.
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" && pqErr.Constraint == oneOpenMoveIndex {
		return ErrOpenMoveExists
	}
	return err
}

func (s *sqlStore) ActiveForBin(binID string) ([]ActiveMove, error) {
	var moves []ActiveMove
	err := s.db.Select(&moves, `
		SELECT mr.id, mr.move_type, mr.status,
		       COALESCE(mr.new_address, '') AS new_address,
		       mr.disposal_action,
		       mr.assigned_shift_id,
		       COALESCE(u.name, du.name, '') AS assigned_driver_name
		FROM bin_move_requests mr
		LEFT JOIN users u ON u.id = mr.assigned_user_id
		LEFT JOIN shifts s ON s.id = mr.assigned_shift_id
		LEFT JOIN users du ON du.id = s.driver_id
		WHERE mr.bin_id = $1 AND mr.status NOT IN ('completed', 'cancelled')
		ORDER BY mr.created_at DESC`, binID)
	return moves, err
}

func (s *sqlStore) ResponsibleDriver(m *models.BinMoveRequest) (driverID, driverName string, err error) {
	// Get (not QueryRow) so the DB seam stays satisfiable by both *sqlx.DB and
	// *orgdb.DB — their QueryRow return types differ, their Get signatures do not.
	switch {
	case m.AssignedUserID != nil && *m.AssignedUserID != "":
		driverID = *m.AssignedUserID
		err = s.db.Get(&driverName, `SELECT name FROM users WHERE id = $1`, driverID)
	case m.AssignedShiftID != nil && *m.AssignedShiftID != "":
		var row struct {
			ID   string `db:"id"`
			Name string `db:"name"`
		}
		err = s.db.Get(&row,
			`SELECT u.id, u.name FROM shifts s JOIN users u ON s.driver_id = u.id WHERE s.id = $1`,
			*m.AssignedShiftID)
		driverID, driverName = row.ID, row.Name
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
