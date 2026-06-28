package moverequest

import "time"

// ActionableMove is the projection the overdue/due-soon watcher needs: a move
// plus its bin's number — enough to classify urgency and build an alert.
type ActionableMove struct {
	ID            string `db:"id"`
	BinID         string `db:"bin_id"`
	BinNumber     int    `db:"bin_number"`
	ScheduledDate int64  `db:"scheduled_date"`
	Status        string `db:"status"`
}

// ClassifyActionable partitions active moves into those that are overdue (past
// their scheduled date) and those due soon (within dueSoonHours). It is pure —
// `now` and `dueSoonHours` are passed in — so it is unit-tested without a
// database or a wall clock. Whether to *act* on each bucket (the enabled flags)
// is the caller's policy, not the domain's.
func ClassifyActionable(moves []ActionableMove, now int64, dueSoonHours float64) (overdue, dueSoon []ActionableMove) {
	for _, m := range moves {
		hoursUntil := float64(m.ScheduledDate-now) / 3600.0
		switch {
		case hoursUntil < 0:
			overdue = append(overdue, m)
		case hoursUntil < dueSoonHours:
			dueSoon = append(dueSoon, m)
		}
	}
	return overdue, dueSoon
}

// Service hosts move-request domain operations that orchestrate the Store.
type Service struct {
	store Store
	now   func() int64 // injectable clock; defaults to time.Now in NewService
}

// NewService builds a Service over the given Store.
func NewService(store Store) *Service {
	return &Service{store: store, now: func() int64 { return time.Now().Unix() }}
}

// FindActionable loads the active moves and classifies them into overdue and
// due-soon (within dueSoonHours). This is the data the move-request watcher acts
// on; the alert fan-out and dedup remain the watcher's job.
func (s *Service) FindActionable(dueSoonHours float64) (overdue, dueSoon []ActionableMove, err error) {
	moves, err := s.store.ActiveWithBin()
	if err != nil {
		return nil, nil, err
	}
	overdue, dueSoon = ClassifyActionable(moves, s.now(), dueSoonHours)
	return overdue, dueSoon, nil
}
