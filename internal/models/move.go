package models

import "time"

type Move struct {
	ID        int    `json:"id" db:"id"`
	BinID     string `json:"bin_id" db:"bin_id"`
	MovedFrom string `json:"moved_from" db:"moved_from"`
	MovedTo   string `json:"moved_to" db:"moved_to"`
	MovedOn   int64  `json:"moved_on" db:"moved_on"` // Unix timestamp
}

// MoveResponse is what we send to the client
type MoveResponse struct {
	ID         int    `json:"id"`
	BinID      string `json:"binId"`
	MovedFrom  string `json:"movedFrom"`
	MovedTo    string `json:"movedTo"`
	MovedOnIso string `json:"movedOnIso"`
	MovedOn    string `json:"movedOn"` // formatted date
}

// CreateMoveRequest is the request body for POST /api/bins/:id/moves
type CreateMoveRequest struct {
	ToStreet   string  `json:"toStreet"`
	ToCity     string  `json:"toCity"`
	ToZip      string  `json:"toZip"`
	MovedOnIso *string `json:"movedOnIso,omitempty"`
}

// ToMoveResponse converts a Move to MoveResponse
func (m *Move) ToMoveResponse() MoveResponse {
	t := time.Unix(m.MovedOn, 0)
	return MoveResponse{
		ID:         m.ID,
		BinID:      m.BinID,
		MovedFrom:  m.MovedFrom,
		MovedTo:    m.MovedTo,
		MovedOnIso: t.Format(time.RFC3339),
		MovedOn:    t.Format("Jan 02, 2006"),
	}
}
