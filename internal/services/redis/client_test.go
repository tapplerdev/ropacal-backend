package redis

import (
	"strings"
	"testing"
)

// The bug this file exists to prevent: driverLocationKey (the writer) and
// driverIDFromKey (the reader) drifting apart. When they disagree the read path
// returns an EMPTY MAP rather than an error, which is indistinguishable from
// "no drivers are on shift" — so the break is silent for two hours, and then
// stale_shift_monitor auto-ends every live shift because
// driver_current_location stopped advancing. Round-trip every form here.

const orgA = "00000000-0000-0000-0000-000000000001"
const orgB = "11111111-1111-1111-1111-111111111111"

func TestDriverKeyRoundTrip(t *testing.T) {
	drivers := []string{
		"15fadf19-5c7f-4553-badc-7845f867b08c",
		"a", // minimal
	}
	for _, org := range []string{orgA, orgB, ""} { // "" = dark mode
		for _, driver := range drivers {
			key, err := driverLocationKey(org, driver)
			if err != nil {
				t.Fatalf("org=%q driver=%q: unexpected error: %v", org, driver, err)
			}
			got, ok := driverIDFromKey(orgDriverPrefix(org), key)
			if !ok {
				t.Fatalf("org=%q: reader rejected the writer's own key %q", org, key)
			}
			if got != driver {
				t.Fatalf("org=%q: round trip gave %q, want %q (key %q)", org, got, driver, key)
			}
		}
	}
}

func TestKeyShapes(t *testing.T) {
	live, _ := driverLocationKey(orgA, "d1")
	if want := "ropacal:org:" + orgA + ":driver:d1:location"; live != want {
		t.Fatalf("live key = %q, want %q", live, want)
	}
	dark, _ := driverLocationKey("", "d1")
	if want := "ropacal:driver:d1:location"; dark != want {
		t.Fatalf("dark key = %q, want %q", dark, want)
	}
	// Centrifugo's engine shares this Redis instance. Our prefix must not
	// collide with its keyspace.
	if strings.HasPrefix(live, "centrifugo") || strings.HasPrefix(dark, "centrifugo") {
		t.Fatal("driver keys must not live under the centrifugo prefix")
	}
}

// The partitioning proof, and it needs no second tenant: org B's reader must
// reject org A's key outright.
func TestOrgPartitioning(t *testing.T) {
	keyA, _ := driverLocationKey(orgA, "d1")

	if _, ok := driverIDFromKey(orgDriverPrefix(orgB), keyA); ok {
		t.Fatal("org B's reader accepted org A's key — cross-tenant GPS leak")
	}
	// And the legacy flat reader must not absorb org-scoped keys (this is the
	// "read-both" trap the plan warns about).
	if _, ok := driverIDFromKey(orgDriverPrefix(""), keyA); ok {
		t.Fatal("the dark-mode reader accepted an org-scoped key")
	}
	// Nor the reverse: an org reader must ignore leftover flat keys.
	keyFlat, _ := driverLocationKey("", "d1")
	if _, ok := driverIDFromKey(orgDriverPrefix(orgA), keyFlat); ok {
		t.Fatal("org A's reader absorbed a legacy un-prefixed key")
	}
}

func TestMalformedKeysRejected(t *testing.T) {
	p := orgDriverPrefix(orgA)
	for _, bad := range []string{
		"",
		"ropacal:driver:d1:location", // wrong family
		"ropacal:org:" + orgA + ":driver::location",    // empty id
		"ropacal:org:" + orgA + ":driver:d1",           // no suffix
		"ropacal:org:" + orgA + ":driver:d1:locations", // suffix typo
		"centrifugo:node:abc",                          // neighbour keyspace
	} {
		if id, ok := driverIDFromKey(p, bad); ok {
			t.Fatalf("accepted malformed key %q as driver %q", bad, id)
		}
	}
}

// orgdb.Migrated() is false in unit tests (Init never ran), so the empty-org
// path must stay legal here — that is dark mode, and it is what makes this
// change safe to roll back.
func TestEmptyOrgLegalWhileDark(t *testing.T) {
	if _, err := driverLocationKey("", "d1"); err != nil {
		t.Fatalf("dark mode must allow an empty org, got %v", err)
	}
}
