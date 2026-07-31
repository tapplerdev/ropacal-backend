package handlers

import "testing"

const (
	catGrocery      = "600-6300-0066"
	catPharmacy     = "600-6400-0000"
	catRetail       = "600-6900-0247"
	catTelecom      = "600-6800-0093" // NOT in anchorCategoryPrefixes
	catRestaurant   = "100-1000-0000"
	catPersonalCare = "700-7300-0219" // nail salon — outside anchorCategoryPrefixes
)

// These substrings were scoring a full 1.0 anchor — 30% of the site score — for
// businesses that merely CONTAIN a chain name. Two of them ("ross", "lucky") are
// US entries, so this was live in production before Canada was ever added.
func TestMatchesChain_SubstringFalsePositives(t *testing.T) {
	cases := []struct {
		name, chain, category string
		want                  bool
		why                   string
	}{
		{"corona bakery", "rona", catGrocery, false, "Corona contains rona"},
		{"verona pizza", "rona", catRestaurant, false, "Verona contains rona"},
		{"rossis pizza", "ross", catRestaurant, false, "Rossi's contains ross"},
		{"cross street cafe", "ross", catRestaurant, false, "Cross contains ross"},
		{"lucky nails", "lucky", catPersonalCare, false, "a nail salon is not a supermarket"},
		{"lucky dragon restaurant", "lucky", catRestaurant, false, "not the Lucky grocery chain"},
		{"metropolitan bank", "metro", catRetail, false, "Metropolitan contains metro"},
		{"target nutrition", "target", catRestaurant, false, "not the Target chain"},

		// ...while the real chains must still match.
		{"rona home garden", "rona", catRetail, true, "the actual Rona"},
		{"ross dress for less", "ross", catRetail, true, "the actual Ross"},
		{"lucky supermarket", "lucky", catGrocery, true, "the actual Lucky"},
		{"target", "target", catRetail, true, "the actual Target"},
	}
	for _, c := range cases {
		if got := matchesChain(c.name, c.chain, c.category); got != c.want {
			t.Errorf("matchesChain(%q, %q, %q) = %v, want %v — %s",
				c.name, c.chain, c.category, got, c.want, c.why)
		}
	}
}

// The reason category gating exists at all: Metro is 1,612 stores across Ontario
// and Quebec, and dropping the keyword to avoid "Metro by T-Mobile" would cost
// every one of them. The category separates them.
func TestMatchesChain_MetroNeedsItsCategory(t *testing.T) {
	if !matchesChain("metro", "metro", catGrocery) {
		t.Error("Metro the supermarket must match — it is a major Canadian grocer")
	}
	if matchesChain("metro by t-mobile", "metro", catTelecom) {
		t.Error("Metro by T-Mobile must NOT score as a retail anchor")
	}
	// Unverifiable category: refuse rather than guess. A false anchor adds more
	// score than a missed one costs.
	if matchesChain("metro", "metro", "") {
		t.Error("an ambiguous name with no category must not match")
	}
}

// A distinctive chain name carries its own evidence, so it must not be held
// hostage to HERE returning a category.
func TestMatchesChain_UnambiguousNamesDoNotNeedACategory(t *testing.T) {
	for _, chain := range []string{"shoppers drug mart", "no frills", "loblaws", "walmart", "canadian tire", "dollarama"} {
		if !matchesChain(chain, chain, "") {
			t.Errorf("%q should match without a category — the name is unambiguous", chain)
		}
	}
}

// Canadian chains against real-world signage, including the multi-word ones.
func TestMatchesChain_CanadianChains(t *testing.T) {
	cases := []struct {
		poi, chain string
		want       bool
	}{
		{"shoppers drug mart 1234", "shoppers drug mart", true},
		{"no frills", "no frills", true},
		{"real canadian superstore", "real canadian superstore", true},
		{"fortinos supermarket", "fortinos", true},
		{"canadian tire", "canadian tire", true},
		{"dollarama", "dollarama", true},
		{"giant tiger store", "giant tiger", true},
		// A word-boundary miss: "frills" alone is not the chain.
		{"no frills grill and bar", "no frills", true}, // genuinely ambiguous; accepted
		{"frills boutique", "no frills", false},
	}
	for _, c := range cases {
		if got := matchesChain(c.poi, c.chain, catGrocery); got != c.want {
			t.Errorf("matchesChain(%q, %q) = %v, want %v", c.poi, c.chain, got, c.want)
		}
	}
}

// Apostrophes are stripped upstream (nameNorm), so the list must be written
// stripped too — "longos", never "longo's".
func TestMatchesChain_ApostropheStrippedForm(t *testing.T) {
	if !matchesChain("longos", "longos", catGrocery) {
		t.Error("longos should match the stripped form produced by nameNorm")
	}
	if !containsWord("trader joes market", "trader joe") {
		t.Error("multi-word prefixes must still match at a word boundary")
	}
}

func TestContainsWord_Boundaries(t *testing.T) {
	cases := []struct {
		haystack, needle string
		want             bool
	}{
		{"metro grocery", "metro", true},
		{"metropolitan", "metro", false},
		{"the metro", "metro", true},
		{"submetro", "metro", false},
		{"metro-north", "metro", true}, // hyphen is a boundary
		{"", "metro", false},
		{"metro", "", false},
		{"a", "metro", false}, // needle longer than haystack
	}
	for _, c := range cases {
		if got := containsWord(c.haystack, c.needle); got != c.want {
			t.Errorf("containsWord(%q, %q) = %v, want %v", c.haystack, c.needle, got, c.want)
		}
	}
}

// contains reports whether a chain list holds an exact entry.
func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// Dollarama and Bulk Barn are TIER 1 for Canada (anchor score 1.0, not 0.7).
// Promoted 2026-07-31: both are errand-frequency retail, and errand density
// scored higher in the 2026-07 calibration (rho=+0.39) than anchor presence
// itself (+0.35). US instincts would file them as tier 2.
func TestCanadianTier1_IncludesErrandAnchors(t *testing.T) {
	ca := chainsFor("CA")

	for _, want := range []string{"dollarama", "bulk barn"} {
		if !contains(ca.Tier1, want) {
			t.Errorf("%q is not tier 1 for CA — it would score 0.7 instead of 1.0", want)
		}
		if contains(ca.Tier2, want) {
			t.Errorf("%q is in BOTH tiers; tier 1 wins the loop, so the tier 2 entry is dead weight", want)
		}
	}
	// The search terms decide what HERE is even asked for. A chain that is
	// tier 1 for SCORING but missing from the search list can only ever be
	// found by accident — that exact split is what hid Loblaws originally.
	for _, want := range []string{"Dollarama", "Bulk Barn"} {
		if !contains(ca.SearchTier1, want) {
			t.Errorf("%q is not in SearchTier1 — the recommender never goes looking for it", want)
		}
	}
}

// Real HERE result names must actually match the new entries.
func TestMatchesChain_NewCanadianAnchors(t *testing.T) {
	cases := []struct {
		name, chain string
		want        bool
	}{
		{"dollarama", "dollarama", true},
		{"dollarama #1234", "dollarama", true},
		{"bulk barn", "bulk barn", true},
		{"bulk barn foods ltd", "bulk barn", true},
		// Must not swallow unrelated businesses that merely share a word.
		{"the barn restaurant", "bulk barn", false},
		{"barn owl bakery", "bulk barn", false},
		{"dollar tree", "dollarama", false},
	}
	for _, c := range cases {
		if got := matchesChain(c.name, c.chain, ""); got != c.want {
			t.Errorf("matchesChain(%q, %q) = %v, want %v", c.name, c.chain, got, c.want)
		}
	}
}
