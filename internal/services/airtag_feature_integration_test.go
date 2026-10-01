//go:build pgintegration

package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/orgdb"
	"ropacal-backend/internal/pgtest"
)

// Real-Postgres tests for per-organization AirTag tracking (migration 00010).

func seedOrg(t *testing.T, admin *sqlx.DB, airtagTracking bool) string {
	t.Helper()
	id := uuid.NewString()
	admin.MustExec(`INSERT INTO organizations (id, name, slug, airtag_tracking) VALUES ($1, $1, $1, $2)`, id, airtagTracking)
	return id
}

// fakeBridge stands in for the FindMy bridge: it serves ANOTHER company's tag,
// exactly what the real bridge would hand any org that asked, and counts asks.
func fakeBridge(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "other-co-tag", "bin_number": 46, "name": "Bin 46", "address": "1 Other Company St", "battery_status": 3},
		}})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestIntegration_AirtagTrackingFollowsTheOrgFlag(t *testing.T) {
	app, admin := pgtest.Connect(t)
	for want, org := range map[bool]string{true: seedOrg(t, admin, true), false: seedOrg(t, admin, false)} {
		d, err := orgdb.New(app, org)
		if err != nil {
			t.Fatal(err)
		}
		got, err := AirtagTrackingEnabled(d)
		if err != nil || got != want {
			t.Errorf("org with airtag_tracking=%v read back (%v, %v)", want, got, err)
		}
	}
}

// The leak this release closes: an org whose airtag_locations came back EMPTY
// used to fall back to the bridge, which serves another company's fleet, and
// put those tags — bin numbers, street addresses — in its daily push. Under
// live tenancy the bridge must never be asked; empty is the answer.
func TestIntegration_BatteryReportNeverAsksTheBridgeUnderTenancy(t *testing.T) {
	app, admin := pgtest.Connect(t)
	if err := orgdb.Init(app); err != nil { // tenancy live, as in production
		t.Fatal(err)
	}
	bridge, hits := fakeBridge(t)

	org := seedOrg(t, admin, true) // tracking on, but no AirTags of its own yet
	d, err := orgdb.New(app, org)
	if err != nil {
		t.Fatal(err)
	}
	s := &DigestScheduler{db: d, bridgeURL: bridge.URL}

	entries, err := s.airtagLocations()
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty org read (%d entries, %v), want none", len(entries), err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("asked the org-blind bridge %d time(s) on an empty result", n)
	}

	// With a tag of its own it reads exactly that, still without the bridge.
	admin.MustExec(`INSERT INTO airtag_locations (id, bin_number, name, latitude, longitude, last_seen, battery_status, organization_id)
		VALUES ($1, 7, 'Bin 7', 37.3, -121.9, now(), 2, $2)`, "own-"+org, org)
	entries, err = s.airtagLocations()
	if err != nil || len(entries) != 1 || entries[0].ID != "own-"+org {
		t.Errorf("org read %+v (%v), want just its own tag", entries, err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("asked the bridge %d time(s)", n)
	}
}

func TestIntegration_BatteryReportIsSkippedWithoutAirtagTracking(t *testing.T) {
	app, admin := pgtest.Connect(t)
	if err := orgdb.Init(app); err != nil {
		t.Fatal(err)
	}
	bridge, hits := fakeBridge(t)
	org := seedOrg(t, admin, false)
	d, err := orgdb.New(app, org)
	if err != nil {
		t.Fatal(err)
	}

	res, err := (&DigestScheduler{db: d, bridgeURL: bridge.URL}).RunDailyBatteryReport(true)
	if err != nil || res == nil || res.Skipped == "" {
		t.Fatalf("got (%+v, %v), want a skipped result", res, err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("asked the bridge %d time(s) for an org without AirTag tracking", n)
	}
	var notified int
	if err := admin.Get(&notified, `SELECT count(*) FROM notification_log WHERE organization_id = $1`, org); err != nil {
		t.Fatal(err)
	}
	if notified != 0 {
		t.Errorf("%d notification(s) logged for an org without AirTag tracking", notified)
	}
}

// Drift checks run only for orgs with AirTag tracking. The control matters: the
// same fixture in an org WITH tracking must raise an alert, or "no alert"
// proves nothing.
func TestIntegration_DriftChecksOnlyOrgsWithAirtagTracking(t *testing.T) {
	app, admin := pgtest.Connect(t)
	if err := orgdb.Init(app); err != nil {
		t.Fatal(err)
	}
	bridge, hits := fakeBridge(t)
	n := 2_000_000 + int(time.Now().UnixNano()%1_000_000)

	alerts := map[bool]int{}
	for _, tracking := range []bool{true, false} {
		org := seedOrg(t, admin, tracking)
		admin.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $1 || '@x.test', 'x', 'Admin', 'admin', $2)`, uuid.NewString(), org)
		admin.MustExec(`INSERT INTO bins (id, bin_number, current_street, city, zip, status, latitude, longitude, organization_id)
			VALUES ($1, $2, '1 Main', 'San Jose', '95112', 'active', 37.30, -121.90, $3)`, uuid.NewString(), n, org)
		// Its own tag for that bin, ~11 km from where the bin should be.
		admin.MustExec(`INSERT INTO airtag_locations (id, bin_number, name, latitude, longitude, last_seen, organization_id)
			VALUES ($1, $2, 'drifted', 37.40, -121.90, now(), $3)`, "drift-"+org, n, org)

		d, err := orgdb.New(app, org)
		if err != nil {
			t.Fatal(err)
		}
		(&AirtagMonitor{root: app, db: d, bridgeURL: bridge.URL}).checkDriftOrg()

		var got int
		if err := admin.Get(&got, `SELECT count(*) FROM notification_log WHERE organization_id = $1 AND type = 'bin_drift_alert'`, org); err != nil {
			t.Fatal(err)
		}
		alerts[tracking] = got
	}
	if alerts[true] == 0 {
		t.Fatal("control: the drifted tag raised no alert in the org WITH tracking — the fixture does not exercise drift")
	}
	if alerts[false] != 0 {
		t.Errorf("an org without AirTag tracking got %d drift alert(s)", alerts[false])
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("asked the bridge %d time(s)", n)
	}
}
