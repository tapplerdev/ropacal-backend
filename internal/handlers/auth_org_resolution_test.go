package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Login resolves the organization from the email when no Organization ID is
// typed. These cover the decision, not the probing — the database work is a
// loop of ordinary org-bound lookups; the part worth pinning is what each
// outcome is allowed to REVEAL.

func TestOrgResolution_UnambiguousEmailResolves(t *testing.T) {
	org, status, msg := decideOrgFromMatches(
		[]activeOrg{{ID: "org-1", Name: "Ropacal", Slug: "ropacal"}}, "driver@ropacal.com")

	if status != 0 {
		t.Fatalf("a single match must resolve; got status %d (%q)", status, msg)
	}
	if org == nil || org.Slug != "ropacal" {
		t.Fatalf("wrong organization resolved: %#v", org)
	}
}

// THE IMPORTANT ONE. An email that exists in no organization must be
// indistinguishable from a wrong password. Anything else makes an
// unauthenticated endpoint an oracle for "is this address a Binly user
// anywhere" — across every tenant at once, which is worse than the per-tenant
// enumeration login already allows.
func TestOrgResolution_UnknownEmailIsIndistinguishableFromWrongPassword(t *testing.T) {
	_, status, msg := decideOrgFromMatches(nil, "nobody@example.com")

	if status != http.StatusUnauthorized {
		t.Errorf("unknown email must return 401, got %d", status)
	}
	if msg != "" {
		t.Errorf("unknown email must carry NO message, got %q — that string is the oracle", msg)
	}

	// Compare the actual wire bytes against what a wrong password sends.
	unknownEmail, err := json.Marshal(LoginResponse{OK: false, Error: msg})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wrongPassword, err := json.Marshal(LoginResponse{OK: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(unknownEmail) != string(wrongPassword) {
		t.Errorf("responses differ and can be told apart:\n  unknown email:  %s\n  wrong password: %s",
			unknownEmail, wrongPassword)
	}
}

// The genuinely ambiguous case — one person working for two of our clients
// under the same address — is the only one that may ask for an Organization ID.
func TestOrgResolution_AmbiguousEmailAsksForOrganizationID(t *testing.T) {
	_, status, msg := decideOrgFromMatches([]activeOrg{
		{ID: "org-1", Name: "Ropacal", Slug: "ropacal"},
		{ID: "org-2", Name: "Acme", Slug: "acme"},
	}, "consultant@example.com")

	if status != http.StatusBadRequest {
		t.Fatalf("an ambiguous email must return 400, got %d", status)
	}
	if !strings.Contains(msg, "Organization ID") {
		t.Errorf("the prompt must name the field as the user sees it, got %q", msg)
	}
	// "slug" is our word, not the customer's. It must not reach a user.
	if strings.Contains(strings.ToLower(msg), "slug") {
		t.Errorf("user-facing text must not say 'slug', got %q", msg)
	}
	// It must not name the organizations the address belongs to — that would
	// tell an outsider who our customers are, and which of them share a person.
	for _, leaked := range []string{"ropacal", "Ropacal", "acme", "Acme"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("the prompt leaks a tenant name (%q): %q", leaked, msg)
		}
	}
}
