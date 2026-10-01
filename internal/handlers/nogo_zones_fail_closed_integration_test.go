//go:build pgintegration

package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"ropacal-backend/internal/orgdb"
	"ropacal-backend/internal/pgtest"
)

// Both placement recommenders treat no-go zones as a hard rule, and both used
// to carry on with an empty list when the zones couldn't be read. An empty
// list filters nothing, so a failed read meant recommending spots inside
// no-go zones. Now they fail instead.

// unreadableNoGoZones returns a pool on which only the no_go_zones read fails:
// its stand-in table has no SELECT grant. Every other table is the real one.
func unreadableNoGoZones(t *testing.T, admin *sqlx.DB) *sqlx.DB {
	t.Helper()
	pool := shadowPool(t, admin, `CREATE TABLE {s}.no_go_zones (center_latitude float8, center_longitude float8,
		radius_meters int, status text, merged_into_zone_id text)`)

	// Positive controls: the zones read fails with permission denied, and
	// nothing else does. Without these the tests below would prove nothing.
	var n int
	var pqErr *pq.Error
	if err := pool.Get(&n, `SELECT COUNT(*) FROM no_go_zones`); !errors.As(err, &pqErr) || pqErr.Code != "42501" {
		t.Fatalf("no_go_zones read: %v, want permission denied (42501)", err)
	}
	if err := pool.Get(&n, `SELECT COUNT(*) FROM bins`); err != nil {
		t.Fatalf("the stand-in broke more than no_go_zones: %v", err)
	}
	return pool
}

func seedNoGoOrg(t *testing.T, admin *sqlx.DB) string {
	t.Helper()
	org := uuid.NewString()
	admin.MustExec(`INSERT INTO organizations (id, name, slug) VALUES ($1, $1, $1)`, org)
	admin.MustExec(`INSERT INTO no_go_zones (id, name, center_latitude, center_longitude, radius_meters, organization_id)
		VALUES ($1, 'closed lot', 37.6, -122.1, 800, $2)`, uuid.NewString(), org)
	return org
}

// The AI chat recommender (recommend_bin_locations). The error naming the
// zones, not the bins, proves the run got past the bins read and stopped at
// the right step.
func TestIntegration_ChatRecommenderFailsWhenNoGoZonesCannotLoad(t *testing.T) {
	app, admin := pgtest.Connect(t)
	org := seedNoGoOrg(t, admin)

	// Zones readable: the same run gets through (no bins, so nothing to
	// search). This is what proves the zones query itself works on the real
	// schema; without it, failing on an error would fail every run.
	d, err := orgdb.New(app, org)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&ChatHandler{db: d}).toolRecommendLocations(map[string]any{}); err != nil {
		t.Fatalf("recommender failed with its no-go zones readable: %v", err)
	}

	d, err = orgdb.New(unreadableNoGoZones(t, admin), org)
	if err != nil {
		t.Fatal(err)
	}
	out, err := (&ChatHandler{db: d}).toolRecommendLocations(map[string]any{})
	if err == nil {
		t.Fatalf("recommender carried on without its no-go zones and returned: %.300s", out)
	}
	if !strings.Contains(err.Error(), "no-go zones") {
		t.Fatalf("failed at the wrong step: %v", err)
	}
}

// GET /analytics/growth/candidates, behind the Growth tab and the relocate and
// redeploy pickers. The relocate modal hides candidates by in_no_go_zone, so
// scoring without zones would put spots inside them in front of a manager.
func TestIntegration_GrowthCandidatesFailWhenNoGoZonesCannotLoad(t *testing.T) {
	app, admin := pgtest.Connect(t)
	org := seedNoGoOrg(t, admin)
	// A zone made from an incident report carries radius 0; seedNoGoOrg's
	// zone is sized at 800 m. The rule is GREATEST(radius, 500).
	admin.MustExec(`INSERT INTO no_go_zones (id, name, center_latitude, center_longitude, radius_meters, organization_id)
		VALUES ($1, 'reported vandalism', 37.8, -122.3, 0, $2)`, uuid.NewString(), org)
	user := uuid.NewString()
	admin.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $1 || '@x', 'x', 'Manager Ann', 'admin', $2)`, user, org)
	admin.MustExec(`INSERT INTO bins (id, bin_number, current_street, city, zip, status, latitude, longitude, organization_id)
		VALUES ($1, 1, '1 Main St', 'Hayward', '94541', 'active', 37.7, -122.2, $2)`, uuid.NewString(), org)
	// Each candidate sits due north of a zone center (1 degree of latitude is
	// about 111,195 m), 50 m or more from any edge of the rule.
	cands := []struct {
		name     string
		lat, lng float64
		flagged  bool
	}{
		{"center of the 800 m zone", 37.6, -122.1, true},
		{"650 m from the 800 m zone: its own radius beats the floor", 37.605846, -122.1, true},
		{"200 m from the radius-0 zone", 37.801799, -122.3, true},
		{"450 m from the radius-0 zone: inside the floor", 37.804047, -122.3, true},
		{"550 m from the radius-0 zone: outside the floor", 37.804946, -122.3, false},
		{"far from both", 37.9, -122.5, false},
	}
	ids := make([]string, len(cands))
	for i, c := range cands {
		ids[i] = uuid.NewString()
		admin.MustExec(`INSERT INTO potential_locations (id, address, street, city, zip, latitude, longitude,
				requested_by_user_id, requested_by_name, created_at, updated_at, organization_id)
			VALUES ($1, 'a', 's', 'c', 'z', $2, $3, $4, 'Manager Ann', 1, 1, $5)`, ids[i], c.lat, c.lng, user, org)
	}

	// Zones readable: each candidate is flagged (and zeroed) exactly per the rule.
	w := httptest.NewRecorder()
	GetGrowthCandidates(nil).ServeHTTP(w, request(t, app, org, http.MethodGet, "", nil, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Data struct {
			Candidates []struct {
				ID         string  `json:"id"`
				Score      float64 `json:"score"`
				InNoGoZone bool    `json:"in_no_go_zone"`
			} `json:"candidates"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	flagged := map[string]bool{}
	for _, c := range body.Data.Candidates {
		flagged[c.ID] = c.InNoGoZone
		if c.InNoGoZone && c.Score != 0 {
			t.Errorf("candidate %s is in a no-go zone but scored %v, want 0", c.ID, c.Score)
		}
	}
	if len(body.Data.Candidates) != len(cands) {
		t.Fatalf("got %d candidates, want %d", len(body.Data.Candidates), len(cands))
	}
	for i, c := range cands {
		if flagged[ids[i]] != c.flagged {
			t.Errorf("%s: in_no_go_zone = %v, want %v", c.name, flagged[ids[i]], c.flagged)
		}
	}

	// Zones unreadable: no scores at all, rather than scores that ignore them.
	w = httptest.NewRecorder()
	GetGrowthCandidates(nil).ServeHTTP(w, request(t, unreadableNoGoZones(t, admin), org, http.MethodGet, "", nil, nil))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "no-go zones") {
		t.Fatalf("with no-go zones unreadable: %d %.300s, want 500 naming the zones", w.Code, w.Body.String())
	}
}
