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
	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/orgdb"
	"ropacal-backend/internal/pgtest"
)

// Real-Postgres tests for per-organization AirTag tracking (migration 00010).

func seedAirtagOrg(t *testing.T, admin *sqlx.DB, airtagTracking bool) string {
	t.Helper()
	id := uuid.NewString()
	admin.MustExec(`INSERT INTO organizations (id, name, slug, airtag_tracking) VALUES ($1, $1, $1, $2)`, id, airtagTracking)
	return id
}

func TestIntegration_AirtagEndpointsAre404WithoutTracking(t *testing.T) {
	app, admin := pgtest.Connect(t)
	on, off := seedAirtagOrg(t, admin, true), seedAirtagOrg(t, admin, false)

	for org, want := range map[string]int{on: http.StatusOK, off: http.StatusNotFound} {
		w := httptest.NewRecorder()
		GetAirtagLocations(nil).ServeHTTP(w, request(t, app, org, http.MethodGet, "", nil, nil))
		if w.Code != want {
			t.Errorf("GET airtag-locations for an org with tracking=%v: %d, want %d", org == on, w.Code, want)
		}
	}
	// Nobody without tracking may kick the bridge's sync either.
	w := httptest.NewRecorder()
	SyncAirtagLocations().ServeHTTP(w, request(t, app, off, http.MethodPost, "", nil, nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("POST airtag-sync without tracking: %d, want 404", w.Code)
	}
}

// A bridge location is resolved ONLY among orgs with tracking. Before, another
// org merely owning a bin with the same number made the tag "claimed by two
// organizations", and its location was dropped.
func TestIntegration_BridgeLocationsResolveOnlyToOrgsWithTracking(t *testing.T) {
	app, admin := pgtest.Connect(t)
	if err := orgdb.Init(app); err != nil { // tenancy live, as in production
		t.Fatal(err)
	}
	on, off := seedAirtagOrg(t, admin, true), seedAirtagOrg(t, admin, false)
	// Both own a bin with the same number. Unique to this run, so no org from
	// another test (or package running in parallel) shares it.
	n := 1_000_000 + int(time.Now().UnixNano()%1_000_000)
	for _, org := range []string{on, off} {
		admin.MustExec(`INSERT INTO bins (id, bin_number, current_street, city, zip, status, organization_id)
			VALUES ($1, $2, '1 Main', 'San Jose', '95112', 'active', $3)`, uuid.NewString(), n, org)
	}
	key := "airtag_loc:test-" + uuid.NewString()
	probe := airtagLocationProbe("tag-"+uuid.NewString(), "tag-"+uuid.NewString(), &n)

	d, found, err := resolveAirtagOrg(app, key, probe)
	if err != nil || !found || d.OrgID() != on {
		t.Fatalf("resolved (%v, found=%v, %v), want the org with tracking", orgOf(d), found, err)
	}

	// The answer is now cached; switching the flag off must not leave the
	// cache routing locations into that org.
	admin.MustExec(`UPDATE organizations SET airtag_tracking = false WHERE id = $1`, on)
	if d, found, err := resolveAirtagOrg(app, key, probe); err != nil || found {
		t.Errorf("after tracking was switched off, resolved (%v, found=%v, %v), want nothing", orgOf(d), found, err)
	}
}

func orgOf(d *orgdb.DB) string {
	if d == nil {
		return "<none>"
	}
	return d.OrgID()
}

// The organization in the login response carries the flag, on every path that
// builds it: an explicit slug, and the org inferred from the email.
func TestIntegration_LoginOrganizationCarriesAirtagTracking(t *testing.T) {
	app, admin := pgtest.Connect(t)
	if err := orgdb.Init(app); err != nil {
		t.Fatal(err)
	}
	for _, want := range []bool{true, false} {
		org := seedAirtagOrg(t, admin, want)
		email := uuid.NewString() + "@x.test"
		admin.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $2, 'x', 'U', 'admin', $3)`,
			uuid.NewString(), email, org)

		bySlug, status, _ := resolveLoginOrg(app, org, email)
		if status != 0 || bySlug == nil || bySlug.ID != org || bySlug.AirtagTracking != want {
			t.Errorf("by slug: %+v (status %d), want airtag_tracking=%v", bySlug, status, want)
		}
		byEmail, status, _ := resolveLoginOrg(app, "", email)
		if status != 0 || byEmail == nil || byEmail.ID != org || byEmail.AirtagTracking != want {
			t.Errorf("by email: %+v (status %d), want airtag_tracking=%v", byEmail, status, want)
		}
	}
}

// The two other places the flag reaches a client: auth status (the dashboard's
// per-load refresh and the app's launch restore) and the operators' org list.
func TestIntegration_AuthStatusAndOperatorListCarryAirtagTracking(t *testing.T) {
	app, admin := pgtest.Connect(t)
	want := map[string]bool{}
	for _, tracking := range []bool{true, false} {
		org := seedAirtagOrg(t, admin, tracking)
		want[org] = tracking
		user := uuid.NewString()
		admin.MustExec(`INSERT INTO users (id, email, password, name, role, organization_id) VALUES ($1, $1 || '@x.test', 'x', 'U', 'admin', $2)`, user, org)

		w := httptest.NewRecorder()
		GetAuthStatus(nil).ServeHTTP(w, request(t, app, org, http.MethodGet, "", nil,
			&middleware.UserClaims{UserID: user, Email: user + "@x.test", Role: "admin", OrgID: org}))
		var body struct {
			Organization struct {
				ID             string `json:"id"`
				AirtagTracking *bool  `json:"airtag_tracking"`
			} `json:"organization"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK {
			t.Fatalf("auth status: %d %s", w.Code, w.Body.String())
		}
		if body.Organization.ID != org || body.Organization.AirtagTracking == nil || *body.Organization.AirtagTracking != tracking {
			t.Errorf("auth status for an org with tracking=%v: %s", tracking, w.Body.String())
		}
	}

	orgs, err := platformOrgList(app)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, o := range orgs {
		if tracking, ok := want[o.ID]; ok {
			seen++
			if o.AirtagTracking != tracking {
				t.Errorf("operator list: %s airtag_tracking=%v, want %v", o.ID, o.AirtagTracking, tracking)
			}
		}
	}
	if seen != len(want) {
		t.Errorf("operator list carried %d of the %d seeded orgs", seen, len(want))
	}
}

// The bridge is handed Apple credentials ONLY from orgs with tracking.
func TestIntegration_BridgeAccountsComeOnlyFromOrgsWithTracking(t *testing.T) {
	app, admin := pgtest.Connect(t)
	if err := orgdb.Init(app); err != nil {
		t.Fatal(err)
	}
	emails := map[bool]string{}
	for _, tracking := range []bool{true, false} {
		org := seedAirtagOrg(t, admin, tracking)
		emails[tracking] = uuid.NewString() + "@icloud.test"
		admin.MustExec(`INSERT INTO airtag_accounts (id, email, password, created_at, updated_at, organization_id)
			VALUES ($1, $2, 'apple-secret', 1, 1, $3)`, uuid.NewString(), emails[tracking], org)
	}
	w := httptest.NewRecorder()
	GetAirtagAccounts(app).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("accounts: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, emails[true]) {
		t.Errorf("the account of the org WITH tracking is missing")
	}
	if strings.Contains(body, emails[false]) {
		t.Errorf("an account from an org WITHOUT tracking was handed to the bridge")
	}
}

// A cached bridge owner is re-checked against the scan's own predicate, so it
// cannot outlive the org being suspended.
func TestIntegration_CachedBridgeOwnerIsDroppedWhenSuspended(t *testing.T) {
	app, admin := pgtest.Connect(t)
	if err := orgdb.Init(app); err != nil {
		t.Fatal(err)
	}
	org := seedAirtagOrg(t, admin, true)
	n := 3_000_000 + int(time.Now().UnixNano()%1_000_000)
	admin.MustExec(`INSERT INTO bins (id, bin_number, current_street, city, zip, status, organization_id)
		VALUES ($1, $2, '1 Main', 'San Jose', '95112', 'active', $3)`, uuid.NewString(), n, org)
	key := "airtag_loc:test-" + uuid.NewString()
	probe := airtagLocationProbe("tag-"+uuid.NewString(), "tag-"+uuid.NewString(), &n)

	if d, found, err := resolveAirtagOrg(app, key, probe); err != nil || !found || d.OrgID() != org {
		t.Fatalf("first resolve: (%v, %v, %v)", orgOf(d), found, err)
	}
	admin.MustExec(`UPDATE organizations SET status = 'suspended' WHERE id = $1`, org)
	if d, found, err := resolveAirtagOrg(app, key, probe); err != nil || found {
		t.Errorf("after suspension, resolved (%v, found=%v, %v), want nothing", orgOf(d), found, err)
	}
}
