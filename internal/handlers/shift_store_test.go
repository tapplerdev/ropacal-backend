package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
)

// fakeShiftStore is an in-memory ShiftStore for tests — no database required.
// Its existence is the whole point of the ShiftStore seam.
type fakeShiftStore struct {
	shift    *models.Shift
	shiftErr error
	bins     []models.ShiftBinWithDetails
	binsErr  error
	tasks    []models.RouteTask
}

func (f fakeShiftStore) CurrentByDriver(_ context.Context, _, _ string) (*models.Shift, error) {
	return f.shift, f.shiftErr
}
func (f fakeShiftStore) TasksWithDetails(_ context.Context, _ string) ([]models.ShiftBinWithDetails, error) {
	return f.bins, f.binsErr
}
func (f fakeShiftStore) Tasks(_ context.Context, _ string) ([]models.RouteTask, error) {
	return f.tasks, nil
}

// withDriver returns a request carrying an authenticated driver in its context,
// matching what middleware.Auth sets.
func withDriver(id string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/driver/shift/current", nil)
	claims := middleware.UserClaims{UserID: id, Email: "driver@test", Role: "driver"}
	return req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, claims))
}

func TestGetCurrentShift_NoShift(t *testing.T) {
	h := GetCurrentShift(fakeShiftStore{shiftErr: sql.ErrNoRows})
	rec := httptest.NewRecorder()
	h(rec, withDriver("d1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp["success"] != true {
		t.Fatalf("want success=true, got %v", resp["success"])
	}
	if resp["data"] != nil {
		t.Fatalf("want data=null when no shift, got %v", resp["data"])
	}
}

func TestGetCurrentShift_WithShift(t *testing.T) {
	h := GetCurrentShift(fakeShiftStore{
		shift: &models.Shift{ID: "s1", DriverID: "d1", Status: "active", TotalBins: 3, CompletedBins: 1},
		tasks: []models.RouteTask{},
	})
	rec := httptest.NewRecorder()
	h(rec, withDriver("d1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var resp struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if !resp.Success || resp.Data == nil {
		t.Fatalf("want success+data, got %+v", resp)
	}
	if resp.Data["id"] != "s1" || resp.Data["status"] != "active" {
		t.Fatalf("unexpected shift fields: %+v", resp.Data)
	}
	// tasks must be present (the field consumers read)
	if _, ok := resp.Data["tasks"]; !ok {
		t.Fatalf("response missing 'tasks' field")
	}
}

func TestGetCurrentShift_BinsErrorIs500(t *testing.T) {
	// Preserves the original behavior: a failure fetching the legacy bin-detail
	// view returns 500, even though those bins aren't in the response body.
	h := GetCurrentShift(fakeShiftStore{
		shift:   &models.Shift{ID: "s1", DriverID: "d1", Status: "active"},
		binsErr: sql.ErrConnDone,
	})
	rec := httptest.NewRecorder()
	h(rec, withDriver("d1"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 when bins fetch fails, got %d", rec.Code)
	}
}

func TestGetCurrentShift_Unauthorized(t *testing.T) {
	// No user in context → 401, and the store is never touched.
	h := GetCurrentShift(fakeShiftStore{})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/driver/shift/current", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}
