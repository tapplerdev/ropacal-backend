package itinerary

import "testing"

func TestParseTaskType(t *testing.T) {
	for _, s := range []string{"collection", "placement", "pickup", "dropoff", "warehouse_stop", "service"} {
		if got, err := ParseTaskType(s); err != nil || string(got) != s {
			t.Errorf("ParseTaskType(%q) = (%q, %v), want (%q, nil)", s, got, err, s)
		}
	}
	for _, s := range []string{"", "COLLECTION", "pick up", "bogus", "move"} {
		if _, err := ParseTaskType(s); err == nil {
			t.Errorf("ParseTaskType(%q) = nil error, want invalid", s)
		}
	}
}

func TestTraits(t *testing.T) {
	if !Pickup.Traits().Paired || !Dropoff.Traits().Paired {
		t.Error("pickup/dropoff must be Paired")
	}
	if Collection.Traits().Paired {
		t.Error("collection must not be Paired")
	}
	if Placement.Traits().CapacityDelta != 1 || Collection.Traits().CapacityDelta != 0 {
		t.Error("placement occupies a bin slot; collection does not")
	}
	if !Service.Traits().HasTimeWindow || Collection.Traits().HasTimeWindow {
		t.Error("only service carries a time window today")
	}
}

func TestValid(t *testing.T) {
	if !Collection.Valid() || TaskType("nope").Valid() {
		t.Error("Valid() mismatch")
	}
}
