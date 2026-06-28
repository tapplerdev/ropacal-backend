package moverequest

import "testing"

const hour = int64(3600)

func TestUrgency(t *testing.T) {
	now := int64(1_000_000)
	cases := []struct {
		name          string
		status        string
		scheduledDate int64
		want          string
	}{
		{"completed is resolved", "completed", now + 100*hour, "resolved"},
		{"cancelled is resolved", "cancelled", now - 100*hour, "resolved"},
		{"past due is overdue", "assigned", now - 1*hour, "overdue"},
		{"exactly now is urgent", "pending", now, "urgent"},
		{"under 24h is urgent", "pending", now + 23*hour, "urgent"},
		{"24h boundary is soon", "pending", now + 24*hour, "soon"},
		{"under 72h is soon", "pending", now + 71*hour, "soon"},
		{"72h boundary is scheduled", "pending", now + 72*hour, "scheduled"},
		{"far future is scheduled", "in_progress", now + 200*hour, "scheduled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Urgency(tc.status, tc.scheduledDate, now); got != tc.want {
				t.Errorf("Urgency(%q, +%dh) = %q, want %q", tc.status, (tc.scheduledDate-now)/hour, got, tc.want)
			}
		})
	}
}

func TestScheduledUrgency(t *testing.T) {
	now := int64(1_000_000)
	cases := []struct {
		name          string
		scheduledDate int64
		want          string
	}{
		{"past is urgent", now - 5*hour, "urgent"},
		{"under 24h is urgent", now + 23*hour, "urgent"},
		{"24h boundary is scheduled", now + 24*hour, "scheduled"},
		{"far future is scheduled", now + 100*hour, "scheduled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScheduledUrgency(tc.scheduledDate, now); got != tc.want {
				t.Errorf("ScheduledUrgency(+%dh) = %q, want %q", (tc.scheduledDate-now)/hour, got, tc.want)
			}
		})
	}
}
