package handlers

import "testing"

// The trap this guards: company:{orgID}:events and driver:location:{driverID}
// BOTH have three segments. Discriminating on len(parts) would misread one as
// the other, which is why parseChannel tries to parse parts[1] as a UUID
// instead. It is also why the org sits in the middle rather than at the end —
// company:events:{orgID} would have parts[1] == "events", matching the legacy
// case and being authorized for an admin of ANY org, silently.
func TestParseChannel(t *testing.T) {
	const org = "00000000-0000-0000-0000-000000000001"
	const drv = "15fadf19-5c7f-4553-badc-7845f867b08c"

	cases := []struct {
		channel           string
		ns, kind, id, org string
		legacy            bool
	}{
		{"driver:location:" + drv, "driver", "location", drv, "", false},
		{"driver:events:" + drv, "driver", "events", drv, "", false},
		{"shift:updates:" + drv, "shift", "updates", drv, "", false},
		{"manager:notifications:" + drv, "manager", "notifications", drv, "", false},
		{"company:" + org + ":events", "company", "events", "", org, false},
		{"company:events", "company", "events", "", "", true},
	}
	for _, c := range cases {
		got, err := parseChannel(c.channel)
		if err != nil {
			t.Fatalf("%s: %v", c.channel, err)
		}
		if got.namespace != c.ns || got.kind != c.kind || got.id != c.id ||
			got.orgID != c.org || got.legacy != c.legacy {
			t.Errorf("%s\n  got  ns=%q kind=%q id=%q org=%q legacy=%v\n  want ns=%q kind=%q id=%q org=%q legacy=%v",
				c.channel, got.namespace, got.kind, got.id, got.orgID, got.legacy,
				c.ns, c.kind, c.id, c.org, c.legacy)
		}
	}
}

// A three-segment driver channel must never be mistaken for the org-scoped
// form, and the scoped company form must never be mistaken for a legacy one.
func TestParseChannelDoesNotConfuseTheTwoThreeSegmentForms(t *testing.T) {
	const org = "00000000-0000-0000-0000-000000000001"
	drvCh, _ := parseChannel("driver:location:" + org) // driver id that IS a uuid
	if drvCh.orgID != "" {
		t.Fatalf("driver channel misread as org-scoped: orgID=%q", drvCh.orgID)
	}
	if drvCh.id != org {
		t.Fatalf("driver id lost: %q", drvCh.id)
	}

	coCh, _ := parseChannel("company:" + org + ":events")
	if coCh.legacy {
		t.Fatal("the scoped company channel must not be flagged legacy")
	}
	if coCh.orgID != org {
		t.Fatalf("org not extracted: %q", coCh.orgID)
	}
}

func TestParseChannelRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "company", "driver"} {
		if _, err := parseChannel(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
