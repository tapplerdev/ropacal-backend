package handlers

import "strings"

// Anchor-chain matching.
//
// Two failure modes this exists to prevent, both measured:
//
//  1. SUBSTRING COLLISIONS. Matching with strings.Contains gave a full 1.0
//     anchor score — 30% of the site score — to businesses that merely contain
//     a chain name: "rona" matched Corona Bakery and Verona Pizza, "ross"
//     matched Rossi's Pizza and Cross Street Cafe, "lucky" matched Lucky Nails.
//     The last two are US entries; the bug predates the Canadian additions.
//
//  2. REAL CHAINS THAT SHARE A COMMON WORD. Whole-word matching still can't
//     separate "Metro" the supermarket (1,612 stores across Ontario and Quebec)
//     from "Metro by T-Mobile". Dropping the keyword loses a major Canadian
//     grocer; keeping it blindly scores phone shops as anchors. So keywords in
//     ambiguousAnchors additionally require the POI's HERE category to be
//     retail-ish. The category comes free with the search response — HERE
//     categories are numeric and country-independent, which is why this works
//     in every market rather than needing a per-country patch.
//
// Only names on ambiguousAnchors pay the category cost, so a distinctive chain
// still matches when HERE returns no category at all.

// ambiguousAnchors are chain names that are also ordinary words or other
// businesses' names. They match ONLY alongside a retail-ish category.
var ambiguousAnchors = map[string]bool{
	"metro":   true, // Metro supermarket vs Metro by T-Mobile
	"winners": true, // Winners (TJX) vs "Winners Circle"
	"rona":    true, // Rona home improvement
	"lucky":   true, // Lucky supermarket vs "Lucky Nails"
	"ross":    true, // Ross Dress for Less vs "Ross Cafe"
	"target":  true, // Target vs "Target Nutrition"
	"staples": true, // Staples vs a grocery describing "staples"
}

// anchorCategoryPrefixes are HERE category IDs a real retail anchor sits in.
// Same taxonomy the density term already whitelists, minus the food-service
// entries — a restaurant is a fine density signal but is not an anchor tenant.
// These are taken verbatim from retailWhitelist, which is grounded in categories
// actually observed in HERE responses. An earlier draft of this list invented
// plausible-looking extra prefixes (600-6100, 600-6200, 600-6600, 600-6800) —
// one of them collided with a telecom store and let "Metro by T-Mobile" through,
// which is exactly the case the gate exists to stop. Only add a prefix here after
// seeing HERE actually return it.
var anchorCategoryPrefixes = []string{
	"600-6000", // convenience store
	"600-6300", // grocery store
	"600-6400", // pharmacy / drugstore
	"600-6500", // hardware / home improvement
	"600-6900", // retail / department store
}

// matchesChain reports whether name contains chain as a WHOLE WORD or phrase.
//
// category is the POI's HERE category ID and may be empty. It is consulted only
// for ambiguousAnchors: an empty category there means "unverified", and the
// match is refused rather than guessed — a false anchor is worth more score
// than a missed one costs.
func matchesChain(name, chain, category string) bool {
	if !containsWord(name, chain) {
		return false
	}
	if !ambiguousAnchors[chain] {
		return true
	}
	return isAnchorCategory(category)
}

func isAnchorCategory(category string) bool {
	if category == "" {
		return false
	}
	for _, p := range anchorCategoryPrefixes {
		if strings.HasPrefix(category, p) {
			return true
		}
	}
	return false
}

// containsWord finds needle in haystack at word boundaries. Both are expected
// lowercase and apostrophe-stripped (see nameNorm at the call site). A boundary
// is anything that is not a letter or digit, so "no frills" matches
// "No Frills Supermarket" but "rona" does not match "Corona".
//
// A single trailing "s" is allowed, because nameNorm strips apostrophes: the
// list entry "trader joe" has to match "trader joes market", and "longo" has to
// match "longos". Without this, switching from substring to word-boundary
// matching would silently break every possessive chain name in the existing US
// list — a regression, not a fix.
func containsWord(haystack, needle string) bool {
	if needle == "" || len(needle) > len(haystack) {
		return false
	}
	for i := 0; ; {
		j := strings.Index(haystack[i:], needle)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(needle)
		leftOK := start == 0 || !isWordByte(haystack[start-1])
		// Tolerate a possessive "s" immediately after the match.
		if end < len(haystack) && haystack[end] == 's' {
			end++
		}
		rightOK := end == len(haystack) || !isWordByte(haystack[end])
		if leftOK && rightOK {
			return true
		}
		i = start + 1
	}
}

func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
