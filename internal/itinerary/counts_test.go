package itinerary

import (
	"testing"

	"ropacal-backend/internal/models"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func relTask(tt models.TaskType, done int, moveID string) models.RouteTask {
	t := models.RouteTask{TaskType: tt, IsCompleted: done}
	if moveID != "" {
		t.MoveRequestID = &moveID
	}
	return t
}

// A relocation (pickup+dropoff sharing a move_request_id) is ONE bin but TWO stops,
// and is only "done" when both its tasks are done.
func TestCountStops_RelocationIsOneBinTwoStops(t *testing.T) {
	tasks := []models.RouteTask{
		relTask(models.TaskTypeCollection, 1, ""),    // 1 bin, 1 stop, done
		relTask(models.TaskTypePickup, 1, "m1"),      // m1 pickup done
		relTask(models.TaskTypeDropoff, 0, "m1"),     // m1 dropoff NOT done → m1 incomplete
		relTask(models.TaskTypeWarehouseStop, 1, ""), // stop only, done
	}
	got := CountStops(tasks)
	want := StopCounts{
		TotalBins:      2, // collection + m1 (relocation counts once)
		CompletedBins:  1, // collection done; m1 incomplete (dropoff pending)
		TotalStops:     4, // every task
		CompletedStops: 3, // collection + pickup + warehouse
	}
	if got != want {
		t.Fatalf("CountStops = %+v, want %+v", got, want)
	}
}

// Once both legs of the relocation are done, the bin counts complete.
func TestCountStops_RelocationCompleteWhenBothLegsDone(t *testing.T) {
	tasks := []models.RouteTask{
		relTask(models.TaskTypePickup, 1, "m1"),
		relTask(models.TaskTypeDropoff, 1, "m1"),
	}
	got := CountStops(tasks)
	want := StopCounts{TotalBins: 1, CompletedBins: 1, TotalStops: 2, CompletedStops: 2}
	if got != want {
		t.Fatalf("CountStops = %+v, want %+v", got, want)
	}
}

// A skipped task is PROCESSED — it counts toward progress so the shift can complete.
func TestCountStops_SkippedCountsAsProcessed(t *testing.T) {
	tasks := []models.RouteTask{
		{TaskType: models.TaskTypeCollection, IsCompleted: 1},                // collected
		{TaskType: models.TaskTypeCollection, IsCompleted: 1, Skipped: true}, // skipped → processed, counts
		{TaskType: models.TaskTypeCollection, IsCompleted: 0},                // pending
	}
	got := CountStops(tasks)
	want := StopCounts{TotalBins: 3, CompletedBins: 2, TotalStops: 3, CompletedStops: 2}
	if got != want {
		t.Fatalf("CountStops(skipped) = %+v, want %+v", got, want)
	}
}

// warehouse_stop and service are stops but not bins.
func TestCountStops_WarehouseAndServiceNotBins(t *testing.T) {
	tasks := []models.RouteTask{
		{TaskType: models.TaskTypeCollection, IsCompleted: 1},
		{TaskType: models.TaskTypeService, IsCompleted: 1},
		{TaskType: models.TaskTypeWarehouseStop, IsCompleted: 0},
	}
	got := CountStops(tasks)
	want := StopCounts{TotalBins: 1, CompletedBins: 1, TotalStops: 3, CompletedStops: 2}
	if got != want {
		t.Fatalf("CountStops = %+v, want %+v", got, want)
	}
}

func TestCountStops_Empty(t *testing.T) {
	if got := CountStops(nil); got != (StopCounts{}) {
		t.Fatalf("CountStops(nil) = %+v, want zero", got)
	}
}

// RecomputeShiftCounts fetches the shift's route_tasks, derives counts via CountStops,
// and writes them back — one definition shared with the read path.
func TestRecomputeShiftCounts(t *testing.T) {
	db, mock := mockExt(t)
	defer db.Close()

	mock.ExpectQuery(`(?s)SELECT id, task_type, is_completed, skipped, move_request_id FROM route_tasks WHERE shift_id = `).
		WithArgs("s1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "task_type", "is_completed", "skipped", "move_request_id"}).
			AddRow("t1", "collection", 1, false, nil).
			AddRow("t2", "collection", 0, false, nil))
	mock.ExpectExec(`(?s)UPDATE shifts SET total_bins = .*completed_bins = .*WHERE id = `).
		WithArgs(2, 1, int64(100), "s1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := RecomputeShiftCounts(db, "s1", 100); err != nil {
		t.Fatalf("RecomputeShiftCounts: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
