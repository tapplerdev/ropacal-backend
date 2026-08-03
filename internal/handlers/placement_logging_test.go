package handlers

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// The JSONB keys are a contract with ropacal_placement/features.py FEATURE_SPEC.
// A rename on either side does not fail anything at runtime — Python's to_matrix
// silently substitutes a neutral value for a missing column, so the model would
// train on a constant and nobody would notice. This test is the only thing
// standing between a typo and a quietly useless feature.
func TestPlacementFeatures_KeysMatchPythonContract(t *testing.T) {
	raw, err := json.Marshal(placementFeatures{
		RetailDensity: 12, AnchorStrength: 0.7, DaytimePop: 41000,
		DistNearestBinM: 1.8, MedianIncome: 96000,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Exactly FEATURE_SPEC, in ropacal_placement/features.py.
	want := []string{
		"retail_density", "anchor_strength", "daytime_pop",
		"dist_nearest_bin_mi", "median_income",
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing feature key %q — Python's to_matrix will substitute a neutral value and train on a constant", k)
		}
	}
	if len(got) != len(want) {
		t.Errorf("feature vector has %d keys, contract has %d: %v", len(got), len(want), got)
	}
}

// Values must survive the round-trip unrounded. dist_nearest_bin_mi in
// particular is in MILES and small — silently truncating it to an integer would
// collapse most candidates to the same value.
func TestPlacementFeatures_ValuesSurviveRoundTrip(t *testing.T) {
	in := placementFeatures{
		RetailDensity: 12, AnchorStrength: 0.7, DaytimePop: 41000,
		DistNearestBinM: 1.83, MedianIncome: 96000,
	}
	raw, _ := json.Marshal(in)
	var out placementFeatures
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip changed the vector:\n  in  %+v\n  out %+v", in, out)
	}
}

// A zero-valued feature must still be WRITTEN, not omitted. `omitempty` here
// would be silent poison: Python cannot distinguish "no anchor nearby" (a real,
// informative 0) from "Go never measured it", and would impute the column
// median in place of a true zero.
func TestPlacementFeatures_ZeroValuesAreNotOmitted(t *testing.T) {
	raw, _ := json.Marshal(placementFeatures{})
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if len(got) != 5 {
		t.Fatalf("an all-zero vector serialized %d keys, expected 5 — omitempty crept in: %s", len(got), raw)
	}
	for k, v := range got {
		if v == nil {
			t.Errorf("feature %q serialized as null", k)
		}
	}
}

// The placeholder builder generates the parameter list by hand; an off-by-one
// would shift every column in the batch by one position and mislabel the whole
// dataset.
func TestPlaceholderRow_Offsets(t *testing.T) {
	// Asserted against the column count rather than a literal. The previous
	// version of this test hardcoded 11 placeholders — matching a bug rather
	// than the INSERT — so it passed green while every write to
	// placement_decisions failed in production.
	n := placementDecisionCols

	first := placeholderRow(0)
	if got := strings.Count(first, "$"); got != n {
		t.Errorf("first row has %d placeholders, want %d: %s", got, n, first)
	}
	if !strings.HasPrefix(first, "($1,") {
		t.Errorf("first row must start at $1: %s", first)
	}

	// Rows must tile without gap or overlap: row 1 starts where row 0 ended.
	second := placeholderRow(n)
	if !strings.HasPrefix(second, "($"+strconv.Itoa(n+1)+",") {
		t.Errorf("second row must start at $%d: %s", n+1, second)
	}

	// Multi-digit offsets are where a hand-rolled integer formatter breaks.
	if got := placeholderRow(1089); !strings.HasPrefix(got, "($1090,") {
		t.Errorf("high offset: %s", got)
	}
}

// TestPlacementDecisionArity is the test that would have caught the silent
// failure: the INSERT column list, the placeholder count and the number of
// arguments appended per row must all agree. They are now all derived from
// placementDecisionColumns, so this pins that they stay derived.
func TestPlacementDecisionArity(t *testing.T) {
	if placementDecisionCols != len(placementDecisionColumns) {
		t.Fatalf("column count %d != column list length %d",
			placementDecisionCols, len(placementDecisionColumns))
	}
	if placementDecisionCols != 12 {
		t.Errorf("placement_decisions has 12 columns; got %d — if the schema "+
			"really changed, update the migration and this number together",
			placementDecisionCols)
	}
	seen := map[string]bool{}
	for _, c := range placementDecisionColumns {
		if seen[c] {
			t.Errorf("duplicate column %q in the INSERT list", c)
		}
		seen[c] = true
	}
}

func TestNullIfEmpty(t *testing.T) {
	if nullIfEmpty("") != nil {
		t.Error(`"" should become NULL, not an empty string in the training data`)
	}
	if nullIfEmpty("Hayward") != "Hayward" {
		t.Error("a real value must pass through")
	}
}

// A nil handle must be a no-op rather than a panic: logging is best-effort and
// must never cost a user their recommendations.
func TestLogPlacementDecisions_NilHandleIsSafe(t *testing.T) {
	logPlacementDecisions(nil, "Hayward", 0, []placementDecision{{Lat: 1, Lng: 2, Score: 5}})
	logPlacementDecisions(nil, "", 0, nil)
}
