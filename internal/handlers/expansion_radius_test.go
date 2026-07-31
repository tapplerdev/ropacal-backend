package handlers

import "testing"

// The radius arrives from a UI slider and from an LLM tool call. Neither is
// worth failing a whole recommendation over, so out-of-range values clamp
// rather than error — but they must land INSIDE the band, not pass through.
func TestClampExpansionRadius(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{25, 25},   // a comfortable day route — the case the slider exists for
		{75, 75},   // the historical default
		{150, 150}, // the far end
		{5, 5},     // the near end

		{0.5, minExpansionRadiusMiles}, // below the band
		{-40, minExpansionRadiusMiles}, // negative: geo.CitiesWithin rejects it outright
		{500, maxExpansionRadiusMiles}, // a whole province, and hundreds of paid calls
		{1e9, maxExpansionRadiusMiles}, // fat-fingered
	}
	for _, c := range cases {
		if got := clampExpansionRadius(c.in); got != c.want {
			t.Errorf("clampExpansionRadius(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// A clamped radius must always be something geo.CitiesWithin will accept — it
// returns an error on a non-positive radius, and the expansion path treats that
// as "no cities" and silently skips expanding.
func TestClampExpansionRadius_AlwaysUsable(t *testing.T) {
	for _, in := range []float64{-1e9, -1, 0, 0.0001, 75, 1e9} {
		if got := clampExpansionRadius(in); got <= 0 {
			t.Errorf("clampExpansionRadius(%v) = %v — geo.CitiesWithin rejects a non-positive radius", in, got)
		}
	}
}

// The default is what a caller gets when the slider is untouched and the LLM
// omits the parameter. It has to stay inside the band the slider offers, or the
// UI would open showing a value it cannot represent.
func TestDefaultRadius_IsWithinTheSliderRange(t *testing.T) {
	if expansionRadiusMiles < minExpansionRadiusMiles || expansionRadiusMiles > maxExpansionRadiusMiles {
		t.Errorf("default %.0f mi is outside the slider band %.0f-%.0f",
			expansionRadiusMiles, minExpansionRadiusMiles, maxExpansionRadiusMiles)
	}
	if clampExpansionRadius(expansionRadiusMiles) != expansionRadiusMiles {
		t.Error("the default does not survive its own clamp")
	}
}
