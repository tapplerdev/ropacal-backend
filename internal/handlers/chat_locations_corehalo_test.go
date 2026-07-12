package handlers

import "testing"

// A tight core bbox around downtown San Jose for the tests.
func sjArea() *areaTarget {
	return &areaTarget{
		Label: "San Jose", Type: "city", Lat: 37.3382, Lng: -121.8863,
		BBox: &[4]float64{-121.95, 37.28, -121.82, 37.40},
	}
}

func inRec(lat, lng float64) scoredRec {
	return scoredRec{
		rec:  LocationRecommendation{Latitude: lat, Longitude: lng, Score: 7, Locality: "in_area"},
		feat: featureVec{0.8, 0.7, 0.5, 1.0},
	}
}

func nearRec(lat, lng float64, feat featureVec) scoredRec {
	return scoredRec{
		rec:  LocationRecommendation{Latitude: lat, Longitude: lng, Score: 7, Locality: "near_area"},
		feat: feat,
	}
}

func TestRefine_NoArea_Passthrough(t *testing.T) {
	scored := []scoredRec{inRec(37.33, -121.88), nearRec(37.9, -122.0, featureVec{0.1, 0.1, 0.1, 0.1})}
	dropped := map[string]int{}
	out, in, near := refineByAreaProfile(scored, nil, true, dropped)
	if len(out) != 2 || in != 0 || near != 0 {
		t.Fatalf("no-area passthrough: got %d recs (in=%d near=%d), want 2/0/0", len(out), in, near)
	}
}

func TestRefine_KeepsMatchingNear_DropsDissimilar(t *testing.T) {
	// 3 in-area anchors + one nearby profile-match + one nearby dissimilar.
	scored := []scoredRec{
		inRec(37.33, -121.88), inRec(37.34, -121.89), inRec(37.32, -121.87),
		nearRec(37.345, -121.905, featureVec{0.78, 0.72, 0.5, 1.0}), // matches profile → keep
		nearRec(37.345, -121.905, featureVec{0.05, 0.05, 0.1, 0.3}), // dead zone → drop
	}
	dropped := map[string]int{}
	out, in, near := refineByAreaProfile(scored, sjArea(), true, dropped)
	if in != 3 {
		t.Errorf("in_area count = %d, want 3", in)
	}
	if near != 1 {
		t.Errorf("nearby kept = %d, want 1", near)
	}
	if dropped["dissimilar_neighbor"] != 1 {
		t.Errorf("dissimilar_neighbor = %d, want 1", dropped["dissimilar_neighbor"])
	}
	if len(out) != 4 {
		t.Errorf("total = %d, want 4", len(out))
	}
	// The kept near pick must carry a match strength.
	for _, r := range out {
		if r.Locality == "near_area" && r.AreaMatch <= 0 {
			t.Errorf("kept near pick has no AreaMatch")
		}
	}
}

func TestRefine_DropsSpatialOutlier(t *testing.T) {
	// A nearby pick that matches the profile but sits far from the cluster is
	// the "lonely" placement — must be dropped by the outlier guard.
	scored := []scoredRec{
		inRec(37.33, -121.88), inRec(37.34, -121.89), inRec(37.32, -121.87),
		nearRec(37.55, -121.70, featureVec{0.8, 0.7, 0.5, 1.0}), // ~25 km away, profile-matching
	}
	dropped := map[string]int{}
	out, _, near := refineByAreaProfile(scored, sjArea(), true, dropped)
	if near != 0 {
		t.Errorf("nearby kept = %d, want 0 (outlier dropped)", near)
	}
	if dropped["spatial_outlier"] != 1 {
		t.Errorf("spatial_outlier = %d, want 1", dropped["spatial_outlier"])
	}
	if len(out) != 3 {
		t.Errorf("total = %d, want 3 (only in-area)", len(out))
	}
}

func TestRefine_NoProfile_DropsNearby(t *testing.T) {
	// Fewer than 3 in-area picks → can't build a profile → nearby dropped.
	scored := []scoredRec{
		inRec(37.33, -121.88),
		nearRec(37.35, -121.90, featureVec{0.8, 0.7, 0.5, 1.0}),
	}
	dropped := map[string]int{}
	out, in, near := refineByAreaProfile(scored, sjArea(), true, dropped)
	if in != 1 || near != 0 {
		t.Errorf("got in=%d near=%d, want in=1 near=0", in, near)
	}
	if dropped["nearby_no_profile"] != 1 {
		t.Errorf("nearby_no_profile = %d, want 1", dropped["nearby_no_profile"])
	}
	if len(out) != 1 {
		t.Errorf("total = %d, want 1", len(out))
	}
}
