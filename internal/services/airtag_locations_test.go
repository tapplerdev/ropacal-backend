package services

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The one read path for an organization's AirTags. The database's answer is
// final — empty included — and the org-blind FindMy bridge is asked only when
// the read FAILS and tenancy is dark. Every other fallback has leaked one
// company's tags, bin numbers and addresses to another.
func TestReadAirtagLocations_TheDatabaseIsTheAnswer(t *testing.T) {
	var hits atomic.Int32
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "bridge-tag", "bin_number": 46}}})
	}))
	defer bridge.Close()

	own := []AirtagEntry{{ID: "own-tag"}}
	dbErr := errors.New("db down")
	cases := []struct {
		name      string
		rows      []AirtagEntry
		err       error
		live      bool
		bridgeURL string
		wantID    string // "" = expect no entries
		wantErr   bool
		wantHits  int32
	}{
		{"live: rows", own, nil, true, bridge.URL, "own-tag", false, 0},
		{"live: empty is the answer", nil, nil, true, bridge.URL, "", false, 0},
		{"live: a DB error is NOT a reason to ask the bridge", nil, dbErr, true, bridge.URL, "", true, 0},
		{"dark: empty is the answer", nil, nil, false, bridge.URL, "", false, 0},
		{"dark: a DB error falls back to the bridge", nil, dbErr, false, bridge.URL, "bridge-tag", false, 1},
		{"dark: a DB error with no bridge configured", nil, dbErr, false, "", "", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hits.Store(0)
			got, err := readAirtagLocations(func() ([]AirtagEntry, error) { return c.rows, c.err }, c.live, c.bridgeURL, "test")
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, want error=%v", err, c.wantErr)
			}
			if c.wantID == "" && len(got) != 0 {
				t.Errorf("got %+v, want no entries", got)
			}
			if c.wantID != "" && (len(got) != 1 || got[0].ID != c.wantID) {
				t.Errorf("got %+v, want just %s", got, c.wantID)
			}
			if n := hits.Load(); n != c.wantHits {
				t.Errorf("bridge asked %d time(s), want %d", n, c.wantHits)
			}
		})
	}
}
