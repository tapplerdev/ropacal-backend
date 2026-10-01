//go:build pgintegration

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/orgdb"
	"ropacal-backend/internal/pgtest"
)

// request builds what the router and middleware hand a handler: the org-bound
// database, the URL params, and (when set) the signed-in user.
func request(t *testing.T, app *sqlx.DB, org, method, body string, params map[string]string, user *middleware.UserClaims) *http.Request {
	t.Helper()
	d, err := orgdb.New(app, org)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, "/x", strings.NewReader(body))
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	ctx := context.WithValue(orgdb.NewContext(r.Context(), d), chi.RouteCtxKey, rc)
	if user != nil {
		ctx = context.WithValue(ctx, middleware.UserContextKey, *user)
	}
	return r.WithContext(ctx)
}

// GET /api/manager/shifts/{shiftId}/edit-history, org-bound like production:
// a shift's own events only, and another org gets an empty list — not null,
// not an error, not the other org's rows.
func TestIntegration_GetShiftEditHistory(t *testing.T) {
	app, admin := pgtest.Connect(t)
	orgA, orgB := uuid.NewString(), uuid.NewString()
	mgr, drv, shiftA, shiftA2 := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, o := range []string{orgA, orgB} {
		admin.MustExec(`INSERT INTO organizations (id, name, slug) VALUES ($1, $1, $1)`, o)
	}
	admin.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $1 || '@x', 'x', 'Manager Ann', 'admin', $2)`, mgr, orgA)
	admin.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $1 || '@x', 'x', 'Driver Dee', 'driver', $2)`, drv, orgA)
	for _, s := range []string{shiftA, shiftA2} {
		admin.MustExec(`INSERT INTO shifts (id, driver_id, status, organization_id) VALUES ($1, $2, 'ready', $3)`, s, drv, orgA)
	}
	admin.MustExec(`INSERT INTO shift_edit_history (organization_id, shift_id, event_type, actor_id, metadata, created_at)
		VALUES ($1, $2, 'created', $3, '{"bin_count": 5}', 1), ($1, $4, 'created', NULL, '{"bin_count": 4}', 1)`, orgA, shiftA, mgr, shiftA2)

	get := func(org string) (int, json.RawMessage) {
		w := httptest.NewRecorder()
		GetShiftEditHistory(nil).ServeHTTP(w, request(t, app, org, http.MethodGet, "", map[string]string{"shiftId": shiftA}, nil))
		var body struct {
			Data json.RawMessage `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body.Data
	}

	code, data := get(orgA)
	var events []map[string]any
	json.Unmarshal(data, &events)
	if code != http.StatusOK || len(events) != 1 || events[0]["bin_count"] != float64(5) || events[0]["actor_name"] != "Manager Ann" {
		t.Errorf("org A: %d %s — want shift A's one event (5 bins, by Manager Ann), not shift A2's", code, data)
	}
	if code, data := get(orgB); code != http.StatusOK || string(data) != "[]" {
		t.Errorf("org B: %d %s — want 200 with [] (RLS hides A's timeline; an empty list, not null)", code, data)
	}
}

// The real UpdateShift reassigning a live shift: the reassignment is logged
// FIRST, then the removal of the work the previous driver already finished —
// both by the manager, in one transaction with the edit.
func TestIntegration_UpdateShiftReassignLogsBeforeTheCleanup(t *testing.T) {
	app, admin := pgtest.Connect(t)
	org, mgr, d1, d2, shift := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	admin.MustExec(`INSERT INTO organizations (id, name, slug) VALUES ($1, $1, $1)`, org)
	for _, u := range []struct{ id, name, role string }{{mgr, "Manager Ann", "admin"}, {d1, "Driver Dee", "driver"}, {d2, "Driver Eve", "driver"}} {
		admin.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $1 || '@x', 'x', $2, $3, $4)`, u.id, u.name, u.role, org)
	}
	admin.MustExec(`INSERT INTO shifts (id, driver_id, status, start_time, organization_id) VALUES ($1, $2, 'active', 10, $3)`, shift, d1, org)
	var done string
	admin.Get(&done, `INSERT INTO route_tasks (shift_id, sequence_order, task_type, latitude, longitude, bin_number, is_completed, completed_at, organization_id)
		VALUES ($1, 1, 'collection', 37.3, -121.9, 1, 1, 20, $2) RETURNING id::text`, shift, org)
	admin.MustExec(`INSERT INTO route_tasks (shift_id, sequence_order, task_type, latitude, longitude, bin_number, organization_id)
		VALUES ($1, 2, 'collection', 37.3, -121.9, 2, $2)`, shift, org)

	w := httptest.NewRecorder()
	UpdateShift(nil, nil, nil, nil).ServeHTTP(w, request(t, app, org, http.MethodPatch, `{"driver_id":"`+d2+`"}`,
		map[string]string{"id": shift}, &middleware.UserClaims{UserID: mgr, Email: "m@x", Role: "admin", OrgID: org}))
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", w.Code, w.Body.String())
	}

	var rows []struct {
		Event  string  `db:"event_type"`
		TaskID *string `db:"task_id"`
		Actor  *string `db:"actor_id"`
		Reason *string `db:"reason"`
		Meta   *string `db:"metadata"`
	}
	if err := admin.Select(&rows, `SELECT event_type, task_id, actor_id, reason, metadata::text AS metadata
		FROM shift_edit_history WHERE shift_id = $1 ORDER BY created_at, seq`, shift); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Event != "driver_reassigned" || rows[1].Event != "task_removed" {
		t.Fatalf("history = %+v, want [driver_reassigned, task_removed]", rows)
	}
	var drivers struct {
		From string `json:"from_driver_id"`
		To   string `json:"to_driver_id"`
	}
	if err := json.Unmarshal([]byte(deref(rows[0].Meta)), &drivers); err != nil || drivers.From != d1 || drivers.To != d2 {
		t.Errorf("reassignment recorded %s, want from %s (Dee) to %s (Eve)", deref(rows[0].Meta), d1, d2)
	}
	if deref(rows[1].TaskID) != done || deref(rows[1].Reason) != "completed_before_reassign" {
		t.Errorf("cleanup logged %+v, want the completed task with reason completed_before_reassign", rows[1])
	}
	for _, r := range rows {
		if deref(r.Actor) != mgr {
			t.Errorf("%s logged by %s, want the manager", r.Event, deref(r.Actor))
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
