//go:build pgintegration

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ropacal-backend/internal/models"
	"ropacal-backend/internal/pgtest"
)

// GET /api/manager/bins/check-recommendations against the real table. Same bug as
// the crash log: `SELECT bcr.*` into a positional Scan silently dropped every
// row once organization_id existed. Distinct values per column catch a column
// listed out of order.
func TestIntegration_GetBinCheckRecommendationsReturnsEveryColumn(t *testing.T) {
	app, admin := pgtest.Connect(t)
	org, other, empty := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, o := range []string{org, other, empty} {
		admin.MustExec(`INSERT INTO organizations (id, name, slug) VALUES ($1, $1, $1)`, o)
	}
	bin, otherBin := uuid.NewString(), uuid.NewString()
	admin.MustExec(`INSERT INTO bins (id, bin_number, current_street, city, zip, status, fill_percentage, latitude, longitude, last_checked_at, organization_id)
		VALUES ($1, 4242, '1 Main St', 'Hayward', '94541', 'active', 55, 37.6, -122.1, 1699990000, $2)`, bin, org)
	admin.MustExec(`INSERT INTO bins (id, bin_number, current_street, city, zip, status, organization_id)
		VALUES ($1, 4242, '2 Elm St', 'Toronto', 'M5V', 'active', $2)`, otherBin, other)

	mgr, rec := uuid.NewString(), uuid.NewString()
	admin.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $1 || '@x', 'x', 'Manager Ann', 'admin', $2)`, mgr, org)
	admin.MustExec(`INSERT INTO bin_check_recommendations (id, bin_id, reason, flagged_at, days_since_check, status,
			resolved_at, resolved_by_user_id, notes, created_at, updated_at, organization_id)
		VALUES ($1, $2, 'manual_flag', 1700000100, 9, 'pending', 1700000200, $4, 'look at the lid', 1700000300, 1700000400, $3)`,
		rec, bin, org, mgr)
	admin.MustExec(`INSERT INTO bin_check_recommendations (id, bin_id, flagged_at, days_since_check, created_at, updated_at, organization_id)
		VALUES ($1, $2, 1, 1, 1, 1, $3)`, uuid.NewString(), otherBin, other)

	w := httptest.NewRecorder()
	GetBinCheckRecommendations(nil).ServeHTTP(w, request(t, app, org, http.MethodGet, "", nil, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got []models.BinCheckRecommendationWithBin
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != rec || got[0].Bin == nil {
		t.Fatalf("want exactly this org's recommendation with its bin, got: %s", w.Body.String())
	}
	r, b := got[0], got[0].Bin
	ints := map[string][2]int64{
		"flagged_at":       {r.FlaggedAt, 1700000100},
		"days_since_check": {int64(r.DaysSinceCheck), 9},
		"created_at":       {r.CreatedAt, 1700000300},
		"updated_at":       {r.UpdatedAt, 1700000400},
		"bin.bin_number":   {int64(b.BinNumber), 4242},
	}
	for field, gw := range ints {
		if gw[0] != gw[1] {
			t.Errorf("%s = %d, want %d", field, gw[0], gw[1])
		}
	}
	strs := map[string][2]string{
		"bin_id":              {r.BinID, bin},
		"reason":              {r.Reason, "manual_flag"},
		"status":              {r.Status, "pending"},
		"resolved_by_user_id": {deref(r.ResolvedByUserID), mgr},
		"notes":               {deref(r.Notes), "look at the lid"},
		"bin.id":              {b.ID, bin},
		"bin.current_street":  {b.CurrentStreet, "1 Main St"},
		"bin.city":            {b.City, "Hayward"},
		"bin.zip":             {b.Zip, "94541"},
		"bin.status":          {b.Status, "active"},
	}
	for field, gw := range strs {
		if gw[0] != gw[1] {
			t.Errorf("%s = %q, want %q", field, gw[0], gw[1])
		}
	}
	if r.ResolvedAt == nil || *r.ResolvedAt != 1700000200 {
		t.Errorf("resolved_at = %v, want 1700000200", r.ResolvedAt)
	}
	if b.FillPercentage == nil || *b.FillPercentage != 55 || b.LastCheckedAt == nil || *b.LastCheckedAt != 1699990000 {
		t.Errorf("bin fill/last check = %v, %v; want 55, 1699990000", b.FillPercentage, b.LastCheckedAt)
	}
	if b.Latitude == nil || *b.Latitude != 37.6 || b.Longitude == nil || *b.Longitude != -122.1 {
		t.Errorf("bin coords = %v, %v; want 37.6, -122.1", b.Latitude, b.Longitude)
	}

	// No recommendations is an empty list, not null.
	w = httptest.NewRecorder()
	GetBinCheckRecommendations(nil).ServeHTTP(w, request(t, app, empty, http.MethodGet, "", nil, nil))
	if body := strings.TrimSpace(w.Body.String()); w.Code != http.StatusOK || body != "[]" {
		t.Errorf("org with no recommendations: %d %s, want 200 []", w.Code, body)
	}
}

// A row that can't be read is a 500, not a silently shorter list. A two-row
// view stands in for bin_check_recommendations (on this test's connections
// only), joined to a real bin; one row can't be scanned. A read failing
// partway through is covered by TestListsFailWhenReadingRowsFails.
func TestIntegration_GetBinCheckRecommendationsFailsLoudly(t *testing.T) {
	_, admin := pgtest.Connect(t)
	org, bin := uuid.NewString(), uuid.NewString()
	admin.MustExec(`INSERT INTO organizations (id, name, slug) VALUES ($1, $1, $1)`, org)
	admin.MustExec(`INSERT INTO bins (id, bin_number, current_street, city, zip, status, organization_id)
		VALUES ($1, 7, '1 Main St', 'Hayward', '94541', 'active', $2)`, bin, org)

	view := func(flaggedAt string) string {
		return `CREATE VIEW {s}.bin_check_recommendations AS SELECT
			n::text AS id, '` + bin + `'::text AS bin_id, 'time_based'::text AS reason,
			` + flaggedAt + ` AS flagged_at, 5 AS days_since_check,
			'pending'::text AS status, NULL::bigint AS resolved_at, NULL::text AS resolved_by_user_id,
			NULL::text AS notes, 1::bigint AS created_at, 1::bigint AS updated_at
			FROM generate_series(1, 2) AS n`
	}
	// The control's rows all read fine, so it must answer 200 with both: that
	// proves the view has every column the query asks for. The broken view
	// differs in one row's value only, so its 500 can only come from the Scan.
	// That row sorts first (text, DESC); skipping it would answer 200 with one.
	for name, tc := range map[string]struct {
		flaggedAt string
		want      int
	}{
		"control, every row readable": {`CASE WHEN n = 1 THEN '2' ELSE '1' END`, http.StatusOK},
		"one row unreadable":          {`CASE WHEN n = 1 THEN 'x' ELSE '1' END`, http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			pool := shadowPool(t, admin, view(tc.flaggedAt), `GRANT SELECT ON {s}.bin_check_recommendations TO {role}`)
			w := httptest.NewRecorder()
			GetBinCheckRecommendations(nil).ServeHTTP(w, request(t, pool, org, http.MethodGet, "", nil, nil))
			if w.Code != tc.want {
				t.Fatalf("%d %.300s, want %d", w.Code, w.Body.String(), tc.want)
			}
			var got []models.BinCheckRecommendationWithBin
			if tc.want == http.StatusOK && (json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got) != 2) {
				t.Fatalf("control answered %s, want both rows", w.Body.String())
			}
		})
	}
}
