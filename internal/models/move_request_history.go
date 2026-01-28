package models

import "time"

// MoveRequestHistory represents an audit log entry for a move request
type MoveRequestHistory struct {
	ID            string `json:"id" db:"id"`
	MoveRequestID string `json:"move_request_id" db:"move_request_id"`

	// Action information
	ActionType string  `json:"action_type" db:"action_type"` // 'created', 'assigned', 'reassigned', 'unassigned', 'completed', 'cancelled', 'updated'
	ActorID    string  `json:"actor_id" db:"actor_id"`
	ActorName  string  `json:"actor_name" db:"actor_name"`
	ActorRole  *string `json:"actor_role,omitempty" db:"actor_role"` // 'manager', 'driver', 'system'

	// State snapshots (what changed)
	PreviousStatus             *string `json:"previous_status,omitempty" db:"previous_status"`
	NewStatus                  *string `json:"new_status,omitempty" db:"new_status"`
	PreviousAssignmentType     *string `json:"previous_assignment_type,omitempty" db:"previous_assignment_type"`
	NewAssignmentType          *string `json:"new_assignment_type,omitempty" db:"new_assignment_type"`
	PreviousAssignedUserID     *string `json:"previous_assigned_user_id,omitempty" db:"previous_assigned_user_id"`
	NewAssignedUserID          *string `json:"new_assigned_user_id,omitempty" db:"new_assigned_user_id"`
	PreviousAssignedUserName   *string `json:"previous_assigned_user_name,omitempty" db:"previous_assigned_user_name"`
	NewAssignedUserName        *string `json:"new_assigned_user_name,omitempty" db:"new_assigned_user_name"`
	PreviousAssignedShiftID    *string `json:"previous_assigned_shift_id,omitempty" db:"previous_assigned_shift_id"`
	NewAssignedShiftID         *string `json:"new_assigned_shift_id,omitempty" db:"new_assigned_shift_id"`

	// Additional context
	Notes    *string `json:"notes,omitempty" db:"notes"`
	Metadata *string `json:"metadata,omitempty" db:"metadata"` // JSON string

	// Timestamp
	CreatedAt int64 `json:"created_at" db:"created_at"`
}

// MoveRequestHistoryResponse includes formatted timestamp and user-friendly display
type MoveRequestHistoryResponse struct {
	ID            string `json:"id"`
	MoveRequestID string `json:"move_request_id"`

	// Action information
	ActionType      string  `json:"action_type"`
	ActionTypeLabel string  `json:"action_type_label"` // Human-readable label
	ActorID         string  `json:"actor_id"`
	ActorName       string  `json:"actor_name"`
	ActorRole       *string `json:"actor_role,omitempty"`

	// State snapshots
	PreviousStatus           *string `json:"previous_status,omitempty"`
	NewStatus                *string `json:"new_status,omitempty"`
	PreviousAssignmentType   *string `json:"previous_assignment_type,omitempty"`
	NewAssignmentType        *string `json:"new_assignment_type,omitempty"`
	PreviousAssignedUserID   *string `json:"previous_assigned_user_id,omitempty"`
	NewAssignedUserID        *string `json:"new_assigned_user_id,omitempty"`
	PreviousAssignedUserName *string `json:"previous_assigned_user_name,omitempty"`
	NewAssignedUserName      *string `json:"new_assigned_user_name,omitempty"`
	PreviousAssignedShiftID  *string `json:"previous_assigned_shift_id,omitempty"`
	NewAssignedShiftID       *string `json:"new_assigned_shift_id,omitempty"`

	// Display fields
	Description string `json:"description"` // Human-readable description of what happened

	// Additional context
	Notes    *string `json:"notes,omitempty"`
	Metadata *string `json:"metadata,omitempty"`

	// Formatted timestamp
	CreatedAtIso string `json:"created_at_iso"`
	CreatedAt    int64  `json:"created_at"` // Unix timestamp
}

// ToHistoryResponse converts MoveRequestHistory to MoveRequestHistoryResponse
func (h *MoveRequestHistory) ToHistoryResponse() MoveRequestHistoryResponse {
	resp := MoveRequestHistoryResponse{
		ID:                       h.ID,
		MoveRequestID:            h.MoveRequestID,
		ActionType:               h.ActionType,
		ActionTypeLabel:          getActionTypeLabel(h.ActionType),
		ActorID:                  h.ActorID,
		ActorName:                h.ActorName,
		ActorRole:                h.ActorRole,
		PreviousStatus:           h.PreviousStatus,
		NewStatus:                h.NewStatus,
		PreviousAssignmentType:   h.PreviousAssignmentType,
		NewAssignmentType:        h.NewAssignmentType,
		PreviousAssignedUserID:   h.PreviousAssignedUserID,
		NewAssignedUserID:        h.NewAssignedUserID,
		PreviousAssignedUserName: h.PreviousAssignedUserName,
		NewAssignedUserName:      h.NewAssignedUserName,
		PreviousAssignedShiftID:  h.PreviousAssignedShiftID,
		NewAssignedShiftID:       h.NewAssignedShiftID,
		Notes:                    h.Notes,
		Metadata:                 h.Metadata,
		CreatedAtIso:             time.Unix(h.CreatedAt, 0).Format(time.RFC3339),
		CreatedAt:                h.CreatedAt,
		Description:              h.BuildDescription(),
	}

	return resp
}

// BuildDescription creates a human-readable description of the history event
func (h *MoveRequestHistory) BuildDescription() string {
	switch h.ActionType {
	case "created":
		return "Created move request"

	case "assigned":
		if h.NewAssignmentType != nil && *h.NewAssignmentType == "shift" && h.NewAssignedUserName != nil {
			return "Assigned to " + *h.NewAssignedUserName + "'s shift (added to their route)"
		} else if h.NewAssignmentType != nil && *h.NewAssignmentType == "manual" && h.NewAssignedUserName != nil {
			return "Manually assigned to " + *h.NewAssignedUserName + " (one-off task, not part of shift route)"
		} else if h.NewAssignedUserName != nil {
			return "Assigned to " + *h.NewAssignedUserName
		}
		return "Assigned"

	case "reassigned":
		prevType := ""
		newType := ""

		if h.PreviousAssignmentType != nil && *h.PreviousAssignmentType == "shift" {
			prevType = " (shift route)"
		} else if h.PreviousAssignmentType != nil && *h.PreviousAssignmentType == "manual" {
			prevType = " (one-off task)"
		}

		if h.NewAssignmentType != nil && *h.NewAssignmentType == "shift" {
			newType = " (shift route)"
		} else if h.NewAssignmentType != nil && *h.NewAssignmentType == "manual" {
			newType = " (one-off task)"
		}

		if h.PreviousAssignedUserName != nil && h.NewAssignedUserName != nil {
			return "Reassigned from " + *h.PreviousAssignedUserName + prevType + " to " + *h.NewAssignedUserName + newType
		} else if h.NewAssignedUserName != nil {
			return "Reassigned to " + *h.NewAssignedUserName + newType
		}
		return "Reassigned"

	case "unassigned":
		if h.PreviousAssignmentType != nil && *h.PreviousAssignmentType == "shift" && h.PreviousAssignedUserName != nil {
			return "Unassigned from " + *h.PreviousAssignedUserName + "'s shift (removed from route)"
		} else if h.PreviousAssignmentType != nil && *h.PreviousAssignmentType == "manual" && h.PreviousAssignedUserName != nil {
			return "Unassigned from " + *h.PreviousAssignedUserName + " (one-off task removed)"
		} else if h.PreviousAssignedUserName != nil {
			return "Unassigned from " + *h.PreviousAssignedUserName
		}
		return "Unassigned (returned to unassigned pool)"

	case "completed":
		return "Move completed successfully"

	case "cancelled":
		if h.Notes != nil && *h.Notes != "" {
			return "Move cancelled - Reason: " + *h.Notes
		}
		return "Move cancelled"

	case "updated":
		if h.Notes != nil && *h.Notes != "" {
			return *h.Notes
		}
		return "Updated move request details (date/location/notes modified)"

	default:
		return "Modified"
	}
}

// getActionTypeLabel returns a human-readable label for the action type
func getActionTypeLabel(actionType string) string {
	labels := map[string]string{
		"created":    "Created",
		"assigned":   "Assigned",
		"reassigned": "Reassigned",
		"unassigned": "Unassigned",
		"completed":  "Completed",
		"cancelled":  "Cancelled",
		"updated":    "Updated",
	}

	if label, ok := labels[actionType]; ok {
		return label
	}
	return actionType
}
