package handlers

import "testing"

// THE REGRESSION THIS EXISTS FOR. On the first live Canadian run, "lucky" —
// present for the California supermarket — matched six unrelated Toronto corner
// shops as whole words, each scoring a tier-2 anchor: Lucky Convenience, Lucky
// Lotto Centre, Lucky Dollar Food Centre, Lucky Pooja Convenience and Grocery,
// Lucky Convenience and Bargain, Lucky Lotto Verity. Country scoping means a
// Canadian organization never evaluates that name at all.
func TestChainsFor_CanadaNeverSeesUSOnlyNames(t *testing.T) {
	ca := chainsFor("CA")
	all := append(append([]string{}, ca.Tier1...), ca.Tier2...)

	// Ordinary English words that US chains happen to use, and that Canadian
	// independents use freely. Each one caused or would cause false anchors.
	for _, banned := range []string{"lucky", "ross", "target", "safeway", "cvs", "walgreens"} {
		for _, c := range all {
			if c == banned {
				t.Errorf("%q is in the Canadian list — it is a US chain name and collides with Canadian independents", banned)
			}
		}
	}
}

func TestChainsFor_USNeverSeesCanadaOnlyNames(t *testing.T) {
	us := chainsFor("US")
	all := append(append([]string{}, us.Tier1...), us.Tier2...)
	for _, banned := range []string{"metro", "winners", "staples", "loblaws", "shoppers drug mart"} {
		for _, c := range all {
			if c == banned {
				t.Errorf("%q is in the US list — it belongs to the Canadian scope", banned)
			}
		}
	}
}

func TestChainsFor_BothCountriesHaveTheirRealAnchors(t *testing.T) {
	has := func(list []string, want string) bool {
		for _, c := range list {
			if c == want {
				return true
			}
		}
		return false
	}
	ca := chainsFor("CA")
	for _, want := range []string{"loblaws", "no frills", "metro", "sobeys", "canadian tire"} {
		if !has(ca.Tier1, want) {
			t.Errorf("Canadian tier-1 is missing %q", want)
		}
	}
	if !has(ca.Tier2, "shoppers drug mart") {
		t.Error("Canadian tier-2 is missing Shoppers Drug Mart — 1,300+ stores, the single biggest gap")
	}
	us := chainsFor("US")
	for _, want := range []string{"target", "walmart", "safeway"} {
		if !has(us.Tier1, want) {
			t.Errorf("US tier-1 is missing %q", want)
		}
	}
	// Chains that genuinely operate in both must appear in both.
	for _, both := range []string{"walmart", "costco", "home depot"} {
		if !has(ca.Tier1, both) {
			t.Errorf("%q operates in Canada and must be in the Canadian list", both)
		}
		if !has(us.Tier1, both) {
			t.Errorf("%q operates in the US and must be in the US list", both)
		}
	}
}

// An unknown or missing country must fall back, never return nothing: an empty
// list silently zeroes 30% of every site score, which is far worse than a few
// chain names that simply never match.
func TestChainsFor_UnknownCountryFallsBackNotEmpty(t *testing.T) {
	for _, c := range []string{"", "  ", "ZZ", "GB", "xx"} {
		got := chainsFor(c)
		if len(got.Tier1) == 0 {
			t.Errorf("chainsFor(%q) returned an empty tier-1 — that silently zeroes the anchor term", c)
		}
	}
	if len(chainsFor("ca").Tier1) != len(chainsFor("CA").Tier1) {
		t.Error("country lookup must be case-insensitive")
	}
	if len(chainsFor(" CA ").Tier1) != len(chainsFor("CA").Tier1) {
		t.Error("country lookup must tolerate surrounding whitespace")
	}
}

// A corner shop is not a destination anchor in any country. 600-6000 was in the
// category whitelist at first and certified all six "Lucky" convenience stores.
func TestAnchorCategories_ConvenienceStoresAreNotAnchors(t *testing.T) {
	if isAnchorCategory("600-6000-0000") {
		t.Error("convenience store must NOT count as an anchor category — this is what let the Lucky corner shops through")
	}
	for _, ok := range []string{"600-6300-0066", "600-6400-0000", "600-6900-0247", "600-6500-0000"} {
		if !isAnchorCategory(ok) {
			t.Errorf("%s should be an anchor category", ok)
		}
	}
}
