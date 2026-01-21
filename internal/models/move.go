package models

import "time"

type Move struct {
	ID        int    `json:"id" db:"id"`
	BinID     string `json:"bin_id" db:"bin_id"`
	MovedFrom string `json:"moved_from" db:"moved_from"`
	MovedTo   string `json:"moved_to" db:"moved_to"`
	MovedOn   int64  `json:"moved_on" db:"moved_on"` // Unix timestamp

	// Enhanced context fields
	MoveType          string  `json:"move_type" db:"move_type"` // 'shift' or 'manual'
	FromStreet        *string `json:"from_street" db:"from_street"`
	FromCity          *string `json:"from_city" db:"from_city"`
	FromZip           *string `json:"from_zip" db:"from_zip"`
	ToStreet          *string `json:"to_street" db:"to_street"`
	ToCity            *string `json:"to_city" db:"to_city"`
	ToZip             *string `json:"to_zip" db:"to_zip"`
	MoveRequestID     *string `json:"move_request_id" db:"move_request_id"`
	CompletedByUserID *string `json:"completed_by_user_id" db:"completed_by_user_id"`
	ShiftID           *string `json:"shift_id" db:"shift_id"`
}

// MoveResponse is what we send to the client
type MoveResponse struct {
	ID         int    `json:"id"`
	BinID      string `json:"binId"`
	MovedFrom  string `json:"movedFrom"`
	MovedTo    string `json:"movedTo"`
	MovedOnIso string `json:"movedOnIso"`
	MovedOn    string `json:"movedOn"` // formatted date

	// Enhanced context fields
	MoveType          string  `json:"moveType"` // 'shift' or 'manual'
	FromStreet        *string `json:"fromStreet,omitempty"`
	FromCity          *string `json:"fromCity,omitempty"`
	FromZip           *string `json:"fromZip,omitempty"`
	ToStreet          *string `json:"toStreet,omitempty"`
	ToCity            *string `json:"toCity,omitempty"`
	ToZip             *string `json:"toZip,omitempty"`
	MoveRequestID     *string `json:"moveRequestId,omitempty"`
	CompletedByUserID *string `json:"completedByUserId,omitempty"`
	CompletedByName   *string `json:"completedByName,omitempty"` // Populated via JOIN
	ShiftID           *string `json:"shiftId,omitempty"`
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
		ID:                m.ID,
		BinID:             m.BinID,
		MovedFrom:         m.MovedFrom,
		MovedTo:           m.MovedTo,
		MovedOnIso:        t.Format(time.RFC3339),
		MovedOn:           t.Format("Jan 02, 2006"),
		MoveType:          m.MoveType,
		FromStreet:        m.FromStreet,
		FromCity:          m.FromCity,
		FromZip:           m.FromZip,
		ToStreet:          m.ToStreet,
		ToCity:            m.ToCity,
		ToZip:             m.ToZip,
		MoveRequestID:     m.MoveRequestID,
		CompletedByUserID: m.CompletedByUserID,
		ShiftID:           m.ShiftID,
	}
}
