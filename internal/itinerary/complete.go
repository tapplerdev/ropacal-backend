package itinerary

import (
	"fmt"

	"github.com/jmoiron/sqlx"
)

// Completion is the driver's evidence for a completed task: fill reading,
// notes, before/after photos and their EXIF GPS.
type Completion struct {
	FillPercentage *int
	Notes          *string
	PhotoURL       *string
	AfterPhotoURL  *string
	PhotoLat       *float64
	PhotoLng       *float64
	AfterPhotoLat  *float64
	AfterPhotoLng  *float64
}

// Complete marks a task done with the driver's evidence — THE completion
// write (CompleteTask's heart). Returns rows affected so the caller can 400
// a vanished task.
//
// NOTE (Phase 6, deliberate): no is_completed=0 guard yet — a double-submit
// silently re-completes, exactly as the historical write did. Adding the
// guard (0 rows → 409) is a flagged behavior change requiring mobile-app
// retry tolerance; it lands with the CompleteTask tx-wrap slice.
func Complete(ext sqlx.Ext, taskID string, now int64, c Completion) (int64, error) {
	res, err := ext.Exec(ext.Rebind(`UPDATE route_tasks
		SET is_completed = 1,
			completed_at = ?,
			updated_fill_percentage = ?,
			completion_notes = ?,
			updated_at = ?,
			photo_url = ?,
			after_photo_url = ?,
			photo_latitude = ?,
			photo_longitude = ?,
			after_photo_latitude = ?,
			after_photo_longitude = ?
		WHERE id = ?`),
		now, c.FillPercentage, c.Notes, now,
		c.PhotoURL, c.AfterPhotoURL, c.PhotoLat, c.PhotoLng, c.AfterPhotoLat, c.AfterPhotoLng,
		taskID)
	if err != nil {
		return 0, fmt.Errorf("complete task: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Uncomplete reverts a completion — the manual compensation CompleteTask's
// pool-write flow needs when a follow-up write fails. It exists ONLY because
// that flow is not yet tx-wrapped; the CompleteTask tx-wrap slice deletes
// every caller (a failure becomes tx.Rollback) and then this function.
func Uncomplete(ext sqlx.Ext, taskID string, now int64) error {
	_, err := ext.Exec(ext.Rebind(
		`UPDATE route_tasks SET is_completed = 0, completed_at = NULL, updated_at = ? WHERE id = ?`),
		now, taskID)
	if err != nil {
		return fmt.Errorf("uncomplete task: %w", err)
	}
	return nil
}
