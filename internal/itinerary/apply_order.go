package itinerary

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/models"
)

// OrderedStop is one persisted stop in the optimizer's order. Exactly one mode applies:
//   - Insert == false: UPDATE the existing route_tasks row Task.ID in place (preserve its ID),
//     stamping the new sequence_order. On a first optimization the latitude/longitude/address
//     are refreshed from the optimizer too (the row pre-exists from shift creation); on a
//     mid-shift reopt only sequence_order changes.
//   - Insert == true: INSERT Task as a new row (a regenerated warehouse_stop, or — rarely — an
//     optimizer stop with no existing match). Task.ID must already be set to a fresh UUID.
//
// Stops are supplied in optimizer order and ApplyOrder stamps a dense sequence_order across
// them as given, so callers can interleave warehouse stops between bins (a first-optimize
// route may legitimately START with a warehouse pickup — that interleaving must survive).
type OrderedStop struct {
	Task   models.RouteTask
	Insert bool
}

// ApplyOrder is the order-DECIDING writer of sequence_order (Resequence is the order-PRESERVING
// one — two writers only). It persists the optimizer's decided order inside the caller's tx:
// stamping/inserting each stop in order, then normalizing to a dense 1..N.
//
//   - isFirst == false (mid-shift reopt): existing rows get a sequence-only UPDATE, new warehouse
//     stops are inserted, then Resequence normalizes (completed-first, otherwise preserving the
//     stamped optimizer order — including mid-route reload runs, whose position is load-bearing).
//   - isFirst == true (shift start): existing rows also have their coordinates refreshed, and the
//     renumber is the legacy first-opt rule — dense 1..N by (sequence_order ASC, created_at ASC),
//     WITHOUT completed-first / warehouse-last. That preserves the optimizer's interleaving (e.g.
//     a leading warehouse pickup) and keeps binsPreloaded auto-completed pickups in their slot.
//
// The caller owns everything before this: resolving optimizer stops → existing task IDs, building
// the warehouse rows, deleting old warehouse stops, the binsPreloaded auto-complete, and the
// first-opt optimization_metadata write. MUST NOT be called for a lock_route_order shift.
func ApplyOrder(ext sqlx.Ext, shiftID string, stops []OrderedStop, isFirst bool) error {
	now := time.Now().Unix()

	updSeqOnly := ext.Rebind(`UPDATE route_tasks SET sequence_order = ?, updated_at = ? WHERE id = ?`)
	updWithCoords := ext.Rebind(`UPDATE route_tasks SET sequence_order = ?, latitude = ?, longitude = ?, address = ?, updated_at = ? WHERE id = ?`)
	insQ := ext.Rebind(`
		INSERT INTO route_tasks (
			id, shift_id, task_type, sequence_order,
			bin_id, bin_number, fill_percentage,
			potential_location_id, new_bin_number, placement_source,
			move_request_id, destination_latitude, destination_longitude, destination_address,
			latitude, longitude, address,
			is_completed, is_deleted, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)

	seq := 1
	for _, s := range stops {
		t := s.Task
		switch {
		case s.Insert:
			if _, err := ext.Exec(insQ,
				t.ID, shiftID, t.TaskType, seq,
				t.BinID, t.BinNumber, t.FillPercentage,
				t.PotentialLocationID, t.NewBinNumber, t.PlacementSource,
				t.MoveRequestID, t.DestinationLatitude, t.DestinationLongitude, t.DestinationAddress,
				t.Latitude, t.Longitude, t.Address,
				0, false, now, now,
			); err != nil {
				return fmt.Errorf("ApplyOrder: insert task %s: %w", t.ID, err)
			}
		case isFirst:
			if _, err := ext.Exec(updWithCoords, seq, t.Latitude, t.Longitude, t.Address, now, t.ID); err != nil {
				return fmt.Errorf("ApplyOrder: update (first) task %s: %w", t.ID, err)
			}
		default:
			if _, err := ext.Exec(updSeqOnly, seq, now, t.ID); err != nil {
				return fmt.Errorf("ApplyOrder: update task %s: %w", t.ID, err)
			}
		}
		seq++
	}

	if isFirst {
		// First-opt renumber: dense 1..N preserving the optimizer's interleaved order
		// (sequence_order ASC, created_at ASC). Deliberately NOT Resequence — no completed-first
		// / warehouse-last — so a leading warehouse pickup and binsPreloaded auto-completed
		// pickups keep their positions. Single ROW_NUMBER statement = the old SELECT+loop.
		q := ext.Rebind(`
			UPDATE route_tasks AS rt
			SET sequence_order = sub.rn
			FROM (
				SELECT id, ROW_NUMBER() OVER (ORDER BY sequence_order ASC, created_at ASC, id ASC) AS rn
				FROM route_tasks
				WHERE shift_id = ? AND is_deleted = false
			) sub
			WHERE rt.id = sub.id`)
		if _, err := ext.Exec(q, shiftID); err != nil {
			return fmt.Errorf("ApplyOrder: first-opt renumber: %w", err)
		}
		return nil
	}

	if err := Resequence(ext, shiftID); err != nil {
		return fmt.Errorf("ApplyOrder: resequence: %w", err)
	}
	return nil
}
