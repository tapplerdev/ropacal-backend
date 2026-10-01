//go:build pgintegration

package database

import (
	"testing"

	"github.com/google/uuid"

	"ropacal-backend/internal/orgdb"
	"ropacal-backend/internal/pgtest"
)

// The real CreateShiftWithTasks, against real Postgres as the app role: a
// shift's birth is ONE `created` event, by the manager, whose bin count is the
// shift's recomputed logical count — a collection plus a relocation is 2 bins,
// though it is 3 tasks (and a warehouse stop). Logging before the recompute
// would record the wrong number; this is what catches it.
func TestIntegration_CreateShiftWithTasksLogsOneCreatedEvent(t *testing.T) {
	app, admin := pgtest.Connect(t)

	org, mgr, drv := uuid.NewString(), uuid.NewString(), uuid.NewString()
	bin1, bin2, move := uuid.NewString(), uuid.NewString(), uuid.NewString()
	admin.MustExec(`INSERT INTO organizations (id, name, slug) VALUES ($1, $1, $1)`, org)
	admin.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $1 || '@x', 'x', 'Manager Ann', 'admin', $2)`, mgr, org)
	admin.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $1 || '@x', 'x', 'Driver Dee', 'driver', $2)`, drv, org)
	for i, b := range []string{bin1, bin2} {
		admin.MustExec(`INSERT INTO bins (id, bin_number, current_street, city, zip, status, latitude, longitude, fill_percentage, organization_id)
			VALUES ($1, $2, '1 Main', 'San Jose', '95112', 'active', 37.3, -121.9, 40, $3)`, b, 900+i, org)
	}
	admin.MustExec(`INSERT INTO bin_move_requests (id, bin_id, scheduled_date, urgency, requested_by,
		original_latitude, original_longitude, original_address, move_type, created_at, updated_at, organization_id)
		VALUES ($1, $2, 1, 'scheduled', $3, 37.31, -121.91, '1 Main', 'relocation', 1, 1, $4)`, move, bin2, mgr, org)

	db, err := orgdb.New(app, org)
	if err != nil {
		t.Fatal(err)
	}
	wlat, wlng, waddr := 37.0, -121.0, "Warehouse"
	tasks := []map[string]interface{}{
		{"task_type": "collection", "latitude": 37.3, "longitude": -121.9, "bin_id": bin1, "bin_number": 900.0, "fill_percentage": 40.0, "address": "a"},
		{"task_type": "pickup", "latitude": 37.3, "longitude": -121.9, "bin_id": bin2, "bin_number": 901.0, "fill_percentage": 40.0, "address": "b",
			"move_request_id": move, "move_type": "relocation", "destination_latitude": 37.4, "destination_longitude": -121.8, "destination_address": "c"},
		{"task_type": "dropoff", "latitude": 37.4, "longitude": -121.8, "bin_id": bin2, "bin_number": 901.0, "fill_percentage": 40.0, "address": "c",
			"move_request_id": move, "move_type": "relocation"},
		{"task_type": "warehouse_stop", "latitude": 37.0, "longitude": -121.0},
	}
	shiftID, _, _, _, err := CreateShiftWithTasks(db, drv, mgr, 8, &wlat, &wlng, &waddr, tasks, false, nil,
		"standard", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateShiftWithTasks: %v", err)
	}

	var rows []struct {
		Event    string  `db:"event_type"`
		Actor    *string `db:"actor_id"`
		BinCount *int    `db:"bin_count"`
	}
	if err := admin.Select(&rows, `SELECT event_type, actor_id, (metadata->>'bin_count')::int AS bin_count
		FROM shift_edit_history WHERE shift_id = $1 ORDER BY seq`, shiftID); err != nil {
		t.Fatal(err)
	}
	var totalBins int
	admin.Get(&totalBins, `SELECT total_bins FROM shifts WHERE id = $1`, shiftID)

	if len(rows) != 1 || rows[0].Event != "created" {
		t.Fatalf("history = %+v, want exactly one created event (no per-task rows at birth)", rows)
	}
	if rows[0].BinCount == nil || *rows[0].BinCount != 2 || totalBins != 2 {
		t.Errorf("bin_count = %v, total_bins = %d; want both 2 (a collection + one relocation)", rows[0].BinCount, totalBins)
	}
	if rows[0].Actor == nil || *rows[0].Actor != mgr {
		t.Errorf("actor = %v, want the manager who created it", rows[0].Actor)
	}
}
