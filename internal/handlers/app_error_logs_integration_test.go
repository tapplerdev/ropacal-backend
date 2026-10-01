//go:build pgintegration

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"ropacal-backend/internal/models"
	"ropacal-backend/internal/pgtest"
)

// GET /api/manager/logs/app-errors against the real table. This query used to be
// `SELECT el.*` read by a positional Scan, so the tenancy migration's extra
// organization_id column failed every row and the crash-log page showed
// nothing, with a 200. Every column below holds a distinct value, so a column
// listed out of order lands in the wrong field and fails here too.
func TestIntegration_GetAppErrorLogsReturnsEveryColumn(t *testing.T) {
	app, admin := pgtest.Connect(t)
	org, other := uuid.NewString(), uuid.NewString()
	drv, mgr := uuid.NewString(), uuid.NewString()
	for _, o := range []string{org, other} {
		admin.MustExec(`INSERT INTO organizations (id, name, slug) VALUES ($1, $1, $1)`, o)
	}
	admin.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $1 || '@x', 'x', 'Driver Dee', 'driver', $2)`, drv, org)
	admin.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $1 || '@x', 'x', 'Manager Ann', 'admin', $2)`, mgr, org)

	shift := uuid.NewString()
	admin.MustExec(`INSERT INTO shifts (id, driver_id, status, organization_id) VALUES ($1, $2, 'ended', $3)`, shift, drv, org)

	full, sparse, foreign := uuid.NewString(), uuid.NewString(), uuid.NewString()
	admin.MustExec(`INSERT INTO app_error_logs (id, driver_id, shift_id, task_id, created_at, log_timestamp,
			context, error_type, error_message, severity, platform, app_version, os_version, device_info,
			last_gps_latitude, last_gps_longitude, stack_trace, metadata,
			is_resolved, resolved_at, resolved_by_user_id, notes, organization_id)
		VALUES ($1, $2, $5, 'task-1', 1700000000, 1700000000123,
			'navigation', 'invalid_waypoints', 'boom', 'error', 'android', '1.0.0+39', 'Android 14', 'Pixel 8',
			37.5, -122.25, 'trace', '{"k": "v"}',
			true, 1700000500, $3, 'fixed', $4)`, full, drv, mgr, org, shift)
	// is_resolved is nullable. Scan failures now fail the request, so a NULL
	// there must read as false rather than take the whole page down.
	admin.MustExec(`INSERT INTO app_error_logs (id, log_timestamp, context, error_type, error_message, severity, platform, is_resolved, organization_id)
		VALUES ($1, 1700000000000, 'gps', 'gps_unavailable', 'no fix', 'warning', 'ios', NULL, $2)`, sparse, org)
	admin.MustExec(`INSERT INTO app_error_logs (id, log_timestamp, context, error_type, error_message, severity, platform, organization_id)
		VALUES ($1, 1700000000000, 'sync', 'x', 'other org', 'info', 'ios', $2)`, foreign, other)

	w := httptest.NewRecorder()
	GetAppErrorLogs(nil).ServeHTTP(w, request(t, app, org, http.MethodGet, "", nil, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got []models.AppErrorLogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byID := map[string]models.AppErrorLogResponse{}
	for _, l := range got {
		byID[l.ID] = l
	}
	if len(got) != 2 || byID[full].ID == "" || byID[sparse].ID == "" {
		t.Fatalf("want this org's two logs (and not the other org's), got %d: %s", len(got), w.Body.String())
	}

	l := byID[full]
	str := map[string][2]string{
		"driver_id":             {deref(l.DriverID), drv},
		"driver_name":           {deref(l.DriverName), "Driver Dee"},
		"shift_id":              {deref(l.ShiftID), shift},
		"task_id":               {deref(l.TaskID), "task-1"},
		"context":               {l.Context, "navigation"},
		"error_type":            {l.ErrorType, "invalid_waypoints"},
		"error_message":         {l.ErrorMessage, "boom"},
		"severity":              {l.Severity, "error"},
		"platform":              {l.Platform, "android"},
		"app_version":           {deref(l.AppVersion), "1.0.0+39"},
		"os_version":            {deref(l.OSVersion), "Android 14"},
		"device_info":           {deref(l.DeviceInfo), "Pixel 8"},
		"stack_trace":           {deref(l.StackTrace), "trace"},
		"metadata":              {deref(l.Metadata), `{"k": "v"}`},
		"resolved_by_user_id":   {deref(l.ResolvedByUserID), mgr},
		"resolved_by_user_name": {deref(l.ResolvedByUserName), "Manager Ann"},
		"notes":                 {deref(l.Notes), "fixed"},
	}
	for field, gw := range str {
		if gw[0] != gw[1] {
			t.Errorf("%s = %q, want %q", field, gw[0], gw[1])
		}
	}
	if l.LastGPSLatitude == nil || *l.LastGPSLatitude != 37.5 || l.LastGPSLongitude == nil || *l.LastGPSLongitude != -122.25 {
		t.Errorf("last GPS = %v, %v; want 37.5, -122.25", l.LastGPSLatitude, l.LastGPSLongitude)
	}
	// The handler formats in the server's local zone, so the expectation must too.
	if want := time.Unix(1700000500, 0).Format(time.RFC3339); !l.IsResolved || deref(l.ResolvedAtISO) != want {
		t.Errorf("is_resolved = %v, resolved_at_iso = %q; want true, %q", l.IsResolved, deref(l.ResolvedAtISO), want)
	}
	// Both timestamps encode the same instant (seconds vs milliseconds), so
	// swapping the two columns puts one in 1970 and the other in year 55,000.
	if l.CreatedAtISO != l.LogTimestampISO {
		t.Errorf("created_at_iso %s != log_timestamp_iso %s; both encode 1700000000", l.CreatedAtISO, l.LogTimestampISO)
	}
	if s := byID[sparse]; s.IsResolved || s.Context != "gps" {
		t.Errorf("sparse row: is_resolved = %v, context = %q; want false, gps", s.IsResolved, s.Context)
	}
}

// The two ends meet: what the driver app posts to /api/logs/app-error is what
// the manager list shows. app_error_logs has never had a row in production, so
// nothing else proves it.
func TestIntegration_AppErrorLogPostedByTheAppShowsInTheList(t *testing.T) {
	app, admin := pgtest.Connect(t)
	org, drv := uuid.NewString(), uuid.NewString()
	admin.MustExec(`INSERT INTO organizations (id, name, slug) VALUES ($1, $1, $1)`, org)
	admin.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $1 || '@x', 'x', 'Driver Dee', 'driver', $2)`, drv, org)

	// The shape AppErrorLoggingService.logCriticalNavigationError sends.
	body := `{"driver_id": "` + drv + `", "log_timestamp": 1700000000123, "context": "navigation",
		"error_type": "invalid_waypoints", "error_message": "route has no waypoints", "severity": "critical",
		"platform": "android", "app_version": "1.0.0", "metadata": {"waypoints": [], "route_status": "empty"}}`
	w := httptest.NewRecorder()
	LogAppError(nil).ServeHTTP(w, request(t, app, org, http.MethodPost, body, nil, nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("POST app-error: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("POST app-error response %s: %v", w.Body.String(), err)
	}

	// As the issues page asks: unresolved, newest first, limit 100.
	r := request(t, app, org, http.MethodGet, "", nil, nil)
	r.URL.RawQuery = "is_resolved=false&limit=100"
	w = httptest.NewRecorder()
	GetAppErrorLogs(nil).ServeHTTP(w, r)
	var got []models.AppErrorLogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("GET app-errors: %d %s", w.Code, w.Body.String())
	}
	if len(got) != 1 || got[0].ID != created.ID {
		t.Fatalf("want exactly the posted log %s, got %s", created.ID, w.Body.String())
	}
	l := got[0]
	if deref(l.DriverName) != "Driver Dee" || l.Severity != "critical" || l.IsResolved ||
		!strings.Contains(deref(l.Metadata), `"route_status": "empty"`) {
		t.Errorf("listed log = driver %q, severity %q, resolved %v, metadata %s",
			deref(l.DriverName), l.Severity, l.IsResolved, deref(l.Metadata))
	}
}

// A row that can't be read is a 500 now, not a silently shorter list: the
// Scan failure that hid every row from July on must never be quiet again.
// A two-row view stands in for app_error_logs (on this test's connections
// only); one row can't be scanned. A read failing partway through is
// covered by TestListsFailWhenReadingRowsFails.
func TestIntegration_GetAppErrorLogsFailsLoudly(t *testing.T) {
	_, admin := pgtest.Connect(t)
	org := uuid.NewString()
	admin.MustExec(`INSERT INTO organizations (id, name, slug) VALUES ($1, $1, $1)`, org)

	view := func(createdAt string) string {
		return `CREATE VIEW {s}.app_error_logs AS SELECT
			n::text AS id, NULL::text AS driver_id, NULL::text AS shift_id, NULL::text AS task_id,
			` + createdAt + ` AS created_at, 1::bigint AS log_timestamp,
			'gps'::text AS context, 't'::text AS error_type, 'm'::text AS error_message,
			'error'::text AS severity, 'ios'::text AS platform,
			NULL::text AS app_version, NULL::text AS os_version, NULL::text AS device_info,
			NULL::float8 AS last_gps_latitude, NULL::float8 AS last_gps_longitude,
			NULL::text AS stack_trace, NULL::jsonb AS metadata,
			false AS is_resolved, NULL::bigint AS resolved_at, NULL::text AS resolved_by_user_id, NULL::text AS notes
			FROM generate_series(1, 2) AS n`
	}
	// The control's rows all read fine, so it must answer 200 with both: that
	// proves the view has every column the query asks for. The broken view
	// differs in one row's value only, so its 500 can only come from the Scan.
	// That row sorts first (text, DESC); skipping it would answer 200 with one.
	for name, tc := range map[string]struct {
		createdAt string
		want      int
	}{
		"control, every row readable": {`CASE WHEN n = 1 THEN '1700000001' ELSE '1700000000' END`, http.StatusOK},
		"one row unreadable":          {`CASE WHEN n = 1 THEN 'not-a-number' ELSE '1700000000' END`, http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			pool := shadowPool(t, admin, view(tc.createdAt), `GRANT SELECT ON {s}.app_error_logs TO {role}`)
			w := httptest.NewRecorder()
			GetAppErrorLogs(nil).ServeHTTP(w, request(t, pool, org, http.MethodGet, "", nil, nil))
			if w.Code != tc.want {
				t.Fatalf("%d %.300s, want %d", w.Code, w.Body.String(), tc.want)
			}
			var got []models.AppErrorLogResponse
			if tc.want == http.StatusOK && (json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got) != 2) {
				t.Fatalf("control answered %s, want both rows", w.Body.String())
			}
		})
	}
}
