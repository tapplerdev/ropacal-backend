package itinerary

import (
	"testing"

	"ropacal-backend/internal/models"
)

func TestCountStops(t *testing.T) {
	task := func(tt models.TaskType, done int) models.RouteTask {
		return models.RouteTask{TaskType: tt, IsCompleted: done}
	}
	tasks := []models.RouteTask{
		task(models.TaskTypeCollection, 1),    // bin, done
		task(models.TaskTypePickup, 0),        // bin, not done
		task(models.TaskTypeDropoff, 1),       // bin, done
		task(models.TaskTypeWarehouseStop, 1), // not a bin, done
		task(models.TaskTypeWarehouseStop, 0), // not a bin, not done
	}

	got := CountStops(tasks)
	want := StopCounts{
		TotalBins:      3, // collection + pickup + dropoff
		CompletedBins:  2, // collection + dropoff
		TotalStops:     5, // everything
		CompletedStops: 3, // collection + dropoff + one warehouse
	}
	if got != want {
		t.Fatalf("CountStops = %+v, want %+v", got, want)
	}
}

// A skipped task carries is_completed=1 (SkipTask sets it) but must NOT count as
// completed — it stays in the total, like the stored completed_bins column.
func TestCountStops_SkippedNotCompleted(t *testing.T) {
	tasks := []models.RouteTask{
		{TaskType: models.TaskTypeCollection, IsCompleted: 1},                // done
		{TaskType: models.TaskTypeCollection, IsCompleted: 1, Skipped: true}, // skipped → not done
		{TaskType: models.TaskTypeCollection, IsCompleted: 0},                // pending
	}
	got := CountStops(tasks)
	want := StopCounts{TotalBins: 3, CompletedBins: 1, TotalStops: 3, CompletedStops: 1}
	if got != want {
		t.Fatalf("CountStops(skipped) = %+v, want %+v", got, want)
	}
}

func TestCountStops_Empty(t *testing.T) {
	if got := CountStops(nil); got != (StopCounts{}) {
		t.Fatalf("CountStops(nil) = %+v, want zero", got)
	}
}
