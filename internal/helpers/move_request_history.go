package helpers

import (
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"log"
	"ropacal-backend/internal/models"
	"time"
)

// LogMoveRequestCreated logs when a move request is created
func LogMoveRequestCreated(db *sqlx.DB, moveRequestID string, actorID string, actorName string) error {
	historyID := uuid.New().String()

	query := `
		INSERT INTO move_request_history (
			id, move_request_id, action_type, actor_id, actor_name, actor_role, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`

	_, err := db.Exec(query,
		historyID,
		moveRequestID,
		"created",
		actorID,
		actorName,
		"manager",
		time.Now().Unix(),
	)

	if err != nil {
		log.Printf("[HISTORY] Failed to log 'created' action for move request %s: %v", moveRequestID, err)
	}

	return err
}

// LogMoveRequestAssigned logs when a move request is assigned
func LogMoveRequestAssigned(db *sqlx.DB, moveRequestID string, actorID string, actorName string, assignmentType string, assignedUserID *string, assignedUserName *string, assignedShiftID *string) error {
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
		"manager",
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

// LogMoveRequestReassigned logs when a move request is reassigned
func LogMoveRequestReassigned(db *sqlx.DB, moveRequestID string, actorID string, actorName string,
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
		"manager",
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

// LogMoveRequestUnassigned logs when a move request is unassigned
func LogMoveRequestUnassigned(db *sqlx.DB, moveRequestID string, actorID string, actorName string,
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
		"manager",
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

// LogMoveRequestCompleted logs when a move request is completed
func LogMoveRequestCompleted(db *sqlx.DB, moveRequestID string, actorID string, actorName string) error {
	historyID := uuid.New().String()

	query := `
		INSERT INTO move_request_history (
			id, move_request_id, action_type, actor_id, actor_name, actor_role,
			previous_status, new_status, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`

	previousStatus := "in_progress"
	newStatus := "completed"

	_, err := db.Exec(query,
		historyID,
		moveRequestID,
		"completed",
		actorID,
		actorName,
		"driver",
		&previousStatus,
		&newStatus,
		time.Now().Unix(),
	)

	if err != nil {
		log.Printf("[HISTORY] Failed to log 'completed' action for move request %s: %v", moveRequestID, err)
	}

	return err
}

// LogMoveRequestCancelled logs when a move request is cancelled
func LogMoveRequestCancelled(db *sqlx.DB, moveRequestID string, actorID string, actorName string, reason *string) error {
	historyID := uuid.New().String()

	query := `
		INSERT INTO move_request_history (
			id, move_request_id, action_type, actor_id, actor_name, actor_role,
			previous_status, new_status, notes, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`

	previousStatus := "pending" // or could be "in_progress"
	newStatus := "cancelled"

	_, err := db.Exec(query,
		historyID,
		moveRequestID,
		"cancelled",
		actorID,
		actorName,
		"manager",
		&previousStatus,
		&newStatus,
		reason,
		time.Now().Unix(),
	)

	if err != nil {
		log.Printf("[HISTORY] Failed to log 'cancelled' action for move request %s: %v", moveRequestID, err)
	}

	return err
}

// LogMoveRequestUpdated logs when a move request details are updated
func LogMoveRequestUpdated(db *sqlx.DB, moveRequestID string, actorID string, actorName string, notes *string) error {
	historyID := uuid.New().String()

	query := `
		INSERT INTO move_request_history (
			id, move_request_id, action_type, actor_id, actor_name, actor_role, notes, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`

	_, err := db.Exec(query,
		historyID,
		moveRequestID,
		"updated",
		actorID,
		actorName,
		"manager",
		notes,
		time.Now().Unix(),
	)

	if err != nil {
		log.Printf("[HISTORY] Failed to log 'updated' action for move request %s: %v", moveRequestID, err)
	}

	return err
}

// GetMoveRequestHistory retrieves the full history for a move request
func GetMoveRequestHistory(db *sqlx.DB, moveRequestID string) ([]models.MoveRequestHistoryResponse, error) {
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
		ORDER BY created_at ASC
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
