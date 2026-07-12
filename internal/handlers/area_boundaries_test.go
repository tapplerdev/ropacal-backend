package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ropacal-backend/internal/geo"
)

func TestGetAreaBoundary(t *testing.T) {
	store, err := geo.LoadBoundaries()
	if err != nil {
		t.Fatalf("LoadBoundaries: %v", err)
	}
	SetBoundaryStore(store)
	t.Cleanup(func() { SetBoundaryStore(nil) })

	handler := GetAreaBoundary()
	call := func(qs string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, "/api/areas/boundary?"+qs, nil)
		rr := httptest.NewRecorder()
		handler(rr, req)
		var body map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		return rr.Code, body
	}

	// The attack the review caught: a bare name with no coordinates must be
	// rejected, not silently resolved to a same-named city.
	if code, _ := call("name=Brentwood"); code != http.StatusBadRequest {
		t.Errorf("bare ?name=Brentwood: got %d, want 400", code)
	}
	if code, _ := call("name=Berkeley&lng=-122.27"); code != http.StatusBadRequest {
		t.Errorf("missing lat: got %d, want 400", code)
	}
	if code, _ := call("name=Berkeley&lat=abc&lng=-122.27"); code != http.StatusBadRequest {
		t.Errorf("unparseable lat: got %d, want 400", code)
	}
	if code, _ := call(""); code != http.StatusBadRequest {
		t.Errorf("missing name: got %d, want 400", code)
	}

	// Happy path: a real city at its own coordinates returns the polygon.
	code, body := call("name=Berkeley&type=city&lat=37.8715&lng=-122.2730")
	if code != http.StatusOK {
		t.Fatalf("Berkeley: got %d, want 200", code)
	}
	if body["found"] != true {
		t.Errorf("Berkeley: found=%v, want true", body["found"])
	}
	if body["geometry"] == nil {
		t.Errorf("Berkeley: geometry missing")
	}

	// A district (Brentwood in LA) with real LA coordinates now resolves to the
	// LA neighborhood polygon.
	if _, body := call("name=Brentwood&type=district&lat=34.0522&lng=-118.4740"); body["found"] != true {
		t.Errorf("LA district Brentwood: found=%v, want true", body["found"])
	}

	// Same name typed as a CITY but picked in LA: the geographic sanity check
	// still rejects the far-away Contra Costa Brentwood city.
	if _, body := call("name=Brentwood&type=city&lat=34.0522&lng=-118.4740"); body["found"] != false {
		t.Errorf("far same-name city: found=%v, want false", body["found"])
	}
}
