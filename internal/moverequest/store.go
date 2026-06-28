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
	// ActiveWithBin returns every not-yet-completed move (pending/assigned/
	// in_progress) joined with its bin number — the watcher's working set.
	ActiveWithBin() ([]ActionableMove, error)
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
