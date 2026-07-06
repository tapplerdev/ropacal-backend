package database

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/jmoiron/sqlx"
)

// addrEvent is one address change in a bin's life: before → after at time t.
// Sources: bin_change_log 'address_change' entries (manual dashboard edits)
// and moves rows (completed move requests).
type addrEvent struct {
	t      int64
	before string
	after  string
}

// coordEvent mirrors addrEvent for coordinates ('coordinates_change' entries).
type coordEvent struct {
	t                    int64
	beforeLat, beforeLng *float64
	afterLat, afterLng   *float64
}

// BackfillCheckAddressSnapshots fills bin_address_snapshot (+lat/lng) on
// checks that predate the snapshot columns, by reconstructing each bin's
// address at checked_on from its bin_change_log + moves timelines.
//
// Resolution per check at time t:
//   - the "after" side of the latest change at or before t, else
//   - the "before" side of the earliest change after t (anchor: a check older
//     than the first recorded change happened at the original address), else
//   - the bin's current address (bins with no recorded changes never moved).
//
// Idempotent: only rows with a NULL snapshot are touched, and every touched
// row receives a value, so this is a no-op on subsequent boots. Checks whose
// bin was deleted cannot exist (FK CASCADE).
func BackfillCheckAddressSnapshots(db *sqlx.DB) error {
	var pending []struct {
		ID        int    `db:"id"`
		BinID     string `db:"bin_id"`
		CheckedOn int64  `db:"checked_on"`
	}
	if err := db.Select(&pending, `
		SELECT id, bin_id, checked_on FROM checks
		WHERE bin_address_snapshot IS NULL
		ORDER BY bin_id, checked_on`); err != nil {
		return fmt.Errorf("select pending checks: %w", err)
	}
	if len(pending) == 0 {
		return nil
	}
	log.Printf("🕰️  [SNAPSHOT-BACKFILL] Backfilling address snapshots for %d check(s)...", len(pending))

	// Group pending checks by bin so each bin's timeline is built once.
	byBin := make(map[string][]int)
	for i, p := range pending {
		byBin[p.BinID] = append(byBin[p.BinID], i)
	}

	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin backfill tx: %w", err)
	}
	defer tx.Rollback()

	updated := 0
	for binID, idxs := range byBin {
		addrEvents, coordEvents, err := loadBinTimelines(db, binID)
		if err != nil {
			return fmt.Errorf("timeline for bin %s: %w", binID, err)
		}

		// Current state = final fallback for bins with no recorded changes.
		var cur struct {
			Addr string   `db:"addr"`
			Lat  *float64 `db:"latitude"`
			Lng  *float64 `db:"longitude"`
		}
		if err := db.Get(&cur, `
			SELECT CONCAT_WS(', ', NULLIF(current_street,''), NULLIF(city,''), NULLIF(zip,'')) AS addr,
			       latitude, longitude
			FROM bins WHERE id = $1`, binID); err != nil {
			return fmt.Errorf("current state for bin %s: %w", binID, err)
		}

		for _, i := range idxs {
			p := pending[i]
			addr := resolveAddr(addrEvents, p.CheckedOn, cur.Addr)
			lat, lng := resolveCoords(coordEvents, p.CheckedOn, cur.Lat, cur.Lng)
			if _, err := tx.Exec(`
				UPDATE checks SET bin_address_snapshot = $1, bin_latitude_snapshot = $2, bin_longitude_snapshot = $3
				WHERE id = $4`, addr, lat, lng, p.ID); err != nil {
				return fmt.Errorf("update check %d: %w", p.ID, err)
			}
			updated++
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit backfill: %w", err)
	}
	log.Printf("✅ [SNAPSHOT-BACKFILL] Backfilled %d check(s) across %d bin(s)", updated, len(byBin))
	return nil
}

// loadBinTimelines merges a bin's address changes from bin_change_log and
// moves into time-ordered event lists.
func loadBinTimelines(db *sqlx.DB, binID string) ([]addrEvent, []coordEvent, error) {
	var addrEvents []addrEvent
	var coordEvents []coordEvent

	var logRows []struct {
		CreatedAt  int64  `db:"created_at"`
		ChangeType string `db:"change_type"`
		OldValues  []byte `db:"old_values"`
		NewValues  []byte `db:"new_values"`
	}
	if err := db.Select(&logRows, `
		SELECT created_at, change_type, old_values, new_values
		FROM bin_change_log
		WHERE bin_id = $1 AND change_type IN ('address_change', 'coordinates_change')
		ORDER BY created_at ASC`, binID); err != nil {
		return nil, nil, fmt.Errorf("change log: %w", err)
	}
	for _, r := range logRows {
		var oldV, newV map[string]interface{}
		_ = json.Unmarshal(r.OldValues, &oldV)
		_ = json.Unmarshal(r.NewValues, &newV)
		switch r.ChangeType {
		case "address_change":
			addrEvents = append(addrEvents, addrEvent{
				t:      r.CreatedAt,
				before: joinAddr(oldV),
				after:  joinAddr(newV),
			})
		case "coordinates_change":
			coordEvents = append(coordEvents, coordEvent{
				t:         r.CreatedAt,
				beforeLat: asFloat(oldV["latitude"]), beforeLng: asFloat(oldV["longitude"]),
				afterLat: asFloat(newV["latitude"]), afterLng: asFloat(newV["longitude"]),
			})
		}
	}

	var moveRows []struct {
		MovedOn   int64  `db:"moved_on"`
		MovedFrom string `db:"moved_from"`
		MovedTo   string `db:"moved_to"`
	}
	if err := db.Select(&moveRows, `
		SELECT moved_on, moved_from, moved_to FROM moves
		WHERE bin_id = $1 ORDER BY moved_on ASC`, binID); err != nil {
		return nil, nil, fmt.Errorf("moves: %w", err)
	}
	for _, m := range moveRows {
		addrEvents = append(addrEvents, addrEvent{t: m.MovedOn, before: m.MovedFrom, after: m.MovedTo})
	}

	// Merge-sort the two address sources by time (each is already sorted).
	for i := 1; i < len(addrEvents); i++ {
		for j := i; j > 0 && addrEvents[j].t < addrEvents[j-1].t; j-- {
			addrEvents[j], addrEvents[j-1] = addrEvents[j-1], addrEvents[j]
		}
	}
	return addrEvents, coordEvents, nil
}

// resolveAddr returns the address in effect at time t given a bin's
// time-ordered change events, falling back to the current address.
func resolveAddr(events []addrEvent, t int64, current string) string {
	lastBefore := -1
	for i, e := range events {
		if e.t <= t {
			lastBefore = i
		}
	}
	if lastBefore >= 0 && strings.TrimSpace(events[lastBefore].after) != "" {
		return events[lastBefore].after
	}
	if lastBefore < 0 && len(events) > 0 && strings.TrimSpace(events[0].before) != "" {
		return events[0].before
	}
	return current
}

// resolveCoords mirrors resolveAddr for the coordinate timeline.
func resolveCoords(events []coordEvent, t int64, curLat, curLng *float64) (*float64, *float64) {
	lastBefore := -1
	for i, e := range events {
		if e.t <= t {
			lastBefore = i
		}
	}
	if lastBefore >= 0 && events[lastBefore].afterLat != nil {
		return events[lastBefore].afterLat, events[lastBefore].afterLng
	}
	if lastBefore < 0 && len(events) > 0 && events[0].beforeLat != nil {
		return events[0].beforeLat, events[0].beforeLng
	}
	return curLat, curLng
}

// joinAddr builds "street, city, zip" from a change-log values map, skipping
// blank parts (mirrors the CONCAT_WS used by the check writers).
func joinAddr(v map[string]interface{}) string {
	if v == nil {
		return ""
	}
	var parts []string
	for _, k := range []string{"street", "city", "zip"} {
		if s, ok := v[k].(string); ok && strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

// asFloat coerces a JSON-decoded numeric value to *float64 (nil otherwise).
func asFloat(v interface{}) *float64 {
	if f, ok := v.(float64); ok {
		return &f
	}
	return nil
}
