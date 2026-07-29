package moverequest

import (
	"encoding/json"
	"log"
	"ropacal-backend/internal/models"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// Durability policy: every Log* takes sqlx.Ext, so a caller MAY pass its
// transaction (*sqlx.Tx) to record the audit row atomically with the action it
// describes — preferred, so the trail can't disagree with reality. Passing the
// plain *sqlx.DB logs post-commit best-effort instead (the action still succeeds
// if the audit insert fails). actorRole and previousStatus are passed in by the
// caller (which knows who acted and the move's status before the change) rather
// than hardcoded/guessed here.

// LogCreated logs when a move request is created
func LogCreated(db sqlx.Ext, moveRequestID string, actorID string, actorName string, actorRole string, moveType string, destinationAddress *string) error {
	historyID := uuid.New().String()

	// Build metadata JSON via json.Marshal so a quote/backslash in an address can't
	// produce invalid JSON (the old string-concat did). map[string]string always
	// marshals, so the error is genuinely impossible here.
	meta := map[string]string{"move_type": moveType}
	if destinationAddress != nil {
		meta["destination_address"] = *destinationAddress
	}
	metadataBytes, _ := json.Marshal(meta)
	metadata := string(metadataBytes)

	query := `
		INSERT INTO move_request_history (
			id, move_request_id, action_type, actor_id, actor_name, actor_role, metadata, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`

	_, err := db.Exec(query,
		historyID,
		moveRequestID,
		"created",
		actorID,
		actorName,
		actorRole,
		metadata,
		time.Now().Unix(),
	)

	if err != nil {
		log.Printf("[HISTORY] Failed to log 'created' action for move request %s: %v", moveRequestID, err)
	}

	return err
}

// LogAssigned logs when a move request is assigned
func LogAssigned(db sqlx.Ext, moveRequestID string, actorID string, actorName string, actorRole string, assignmentType string, assignedUserID *string, assignedUserName *string, assignedShiftID *string) error {
	historyID := uuid.New().String()

	query := `
		INSERT INTO move_request_history (
			id, move_request_id, action_type, actor_id, actor_name, actor_role,
			new_assignment_type, new_assigned_user_id, new_assigned_user_name, new_assigned_shift_id,
			created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`

	_, err := db.Exec(query,
		historyID,
		moveRequestID,
		"assigned",
		actorID,
		actorName,
		actorRole,
		assignmentType,
		assignedUserID,
		assignedUserName,
		assignedShiftID,
		time.Now().Unix(),
	)

	if err != nil {
		log.Printf("[HISTORY] Failed to log 'assigned' action for move request %s: %v", moveRequestID, err)
	}

	return err
}

// LogReassigned logs when a move request is reassigned
func LogReassigned(db sqlx.Ext, moveRequestID string, actorID string, actorName string, actorRole string,
	previousAssignmentType *string, newAssignmentType *string,
	previousAssignedUserID *string, newAssignedUserID *string,
	previousAssignedUserName *string, newAssignedUserName *string,
	previousAssignedShiftID *string, newAssignedShiftID *string) error {

	historyID := uuid.New().String()

	query := `
		INSERT INTO move_request_history (
			id, move_request_id, action_type, actor_id, actor_name, actor_role,
			previous_assignment_type, new_assignment_type,
			previous_assigned_user_id, new_assigned_user_id,
			previous_assigned_user_name, new_assigned_user_name,
			previous_assigned_shift_id, new_assigned_shift_id,
			created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`

	_, err := db.Exec(query,
		historyID,
		moveRequestID,
		"reassigned",
		actorID,
		actorName,
		actorRole,
		previousAssignmentType,
		newAssignmentType,
		previousAssignedUserID,
		newAssignedUserID,
		previousAssignedUserName,
		newAssignedUserName,
		previousAssignedShiftID,
		newAssignedShiftID,
		time.Now().Unix(),
	)

	if err != nil {
		log.Printf("[HISTORY] Failed to log 'reassigned' action for move request %s: %v", moveRequestID, err)
	}

	return err
}

// LogUnassigned logs when a move request is unassigned
func LogUnassigned(db sqlx.Ext, moveRequestID string, actorID string, actorName string, actorRole string,
	previousAssignmentType *string, previousAssignedUserID *string, previousAssignedUserName *string, previousAssignedShiftID *string) error {

	historyID := uuid.New().String()

	query := `
		INSERT INTO move_request_history (
			id, move_request_id, action_type, actor_id, actor_name, actor_role,
			previous_assignment_type, previous_assigned_user_id, previous_assigned_user_name, previous_assigned_shift_id,
			created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`

	_, err := db.Exec(query,
		historyID,
		moveRequestID,
		"unassigned",
		actorID,
		actorName,
		actorRole,
		previousAssignmentType,
		previousAssignedUserID,
		previousAssignedUserName,
		previousAssignedShiftID,
		time.Now().Unix(),
	)

	if err != nil {
		log.Printf("[HISTORY] Failed to log 'unassigned' action for move request %s: %v", moveRequestID, err)
	}

	return err
}

// LogCompleted logs when a move request is completed. previousStatus is the move's
// status immediately before completion (read by the caller, not assumed).
func LogCompleted(db sqlx.Ext, moveRequestID string, actorID string, actorName string, actorRole string, previousStatus string) error {
	historyID := uuid.New().String()

	query := `
		INSERT INTO move_request_history (
			id, move_request_id, action_type, actor_id, actor_name, actor_role,
			previous_status, new_status, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`

	newStatus := "completed"

	_, err := db.Exec(query,
		historyID,
		moveRequestID,
		"completed",
		actorID,
		actorName,
		actorRole,
		previousStatus,
		newStatus,
		time.Now().Unix(),
	)

	if err != nil {
		log.Printf("[HISTORY] Failed to log 'completed' action for move request %s: %v", moveRequestID, err)
	}

	return err
}

// LogCancelled logs when a move request is cancelled. previousStatus is the move's
// status immediately before cancellation (read by the caller, not assumed).
func LogCancelled(db sqlx.Ext, moveRequestID string, actorID string, actorName string, actorRole string, previousStatus string, reason *string) error {
	historyID := uuid.New().String()

	query := `
		INSERT INTO move_request_history (
			id, move_request_id, action_type, actor_id, actor_name, actor_role,
			previous_status, new_status, notes, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`

	newStatus := "cancelled"

	_, err := db.Exec(query,
		historyID,
		moveRequestID,
		"cancelled",
		actorID,
		actorName,
		actorRole,
		previousStatus,
		newStatus,
		reason,
		time.Now().Unix(),
	)

	if err != nil {
		log.Printf("[HISTORY] Failed to log 'cancelled' action for move request %s: %v", moveRequestID, err)
	}

	return err
}

// LogUpdated logs when a move request details are updated
func LogUpdated(db sqlx.Ext, moveRequestID string, actorID string, actorName string, actorRole string, notes *string, metadata *string) error {
	historyID := uuid.New().String()

	query := `
		INSERT INTO move_request_history (
			id, move_request_id, action_type, actor_id, actor_name, actor_role, notes, metadata, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`

	_, err := db.Exec(query,
		historyID,
		moveRequestID,
		"updated",
		actorID,
		actorName,
		actorRole,
		notes,
		metadata,
		time.Now().Unix(),
	)

	if err != nil {
		log.Printf("[HISTORY] Failed to log 'updated' action for move request %s: %v", moveRequestID, err)
	}

	return err
}

// GetHistory retrieves the full history for a move request
func GetHistory(db DB, moveRequestID string) ([]models.MoveRequestHistoryResponse, error) {
	query := `
		SELECT
			id, move_request_id, action_type, actor_id, actor_name, actor_role,
			previous_status, new_status,
			previous_assignment_type, new_assignment_type,
			previous_assigned_user_id, new_assigned_user_id,
			previous_assigned_user_name, new_assigned_user_name,
			previous_assigned_shift_id, new_assigned_shift_id,
			notes, metadata, created_at
		FROM move_request_history
		WHERE move_request_id = $1
		ORDER BY created_at ASC, seq ASC
	`

	var history []models.MoveRequestHistory
	err := db.Select(&history, query, moveRequestID)
	if err != nil {
		log.Printf("[HISTORY] Failed to fetch history for move request %s: %v", moveRequestID, err)
		return nil, err
	}

	// Convert to response format
	responses := make([]models.MoveRequestHistoryResponse, len(history))
	for i, h := range history {
		responses[i] = h.ToHistoryResponse()
	}

	return responses, nil
}
