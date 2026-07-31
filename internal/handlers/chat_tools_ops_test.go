package handlers

import "testing"

// The window is what makes "were there any incidents today" answerable at all.
// Before these tools the only incident data was a LIFETIME count per zone.
func TestOpsWindowDays(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		def    int
		want   int
	}{
		{"absent falls back to the caller's default", map[string]any{}, 7, 7},
		{"move requests default to no window", map[string]any{}, 0, 0},
		{"explicit 1 day = today", map[string]any{"days": float64(1)}, 7, 1},
		{"explicit overrides the default", map[string]any{"days": float64(30)}, 7, 30},
		// A year is already more than anyone means by "recently", and an
		// unbounded scan on a growing table is not worth the one-line risk.
		{"clamped at a year", map[string]any{"days": float64(99999)}, 7, 365},
		// JSON numbers arrive as float64. An int would silently miss and fall
		// back to the default, which is the quiet kind of wrong.
		{"non-float days is ignored", map[string]any{"days": 5}, 7, 7},
		{"zero is not a window", map[string]any{"days": float64(0)}, 7, 7},
		{"negative is not a window", map[string]any{"days": float64(-3)}, 7, 7},
	}
	for _, c := range cases {
		if got := opsWindowDays(c.params, c.def); got != c.want {
			t.Errorf("%s: opsWindowDays(%v, %d) = %d, want %d", c.name, c.params, c.def, got, c.want)
		}
	}
}
