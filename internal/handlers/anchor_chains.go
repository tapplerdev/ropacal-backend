package handlers

import (
	"log"
	"strings"

	"ropacal-backend/internal/orgdb"
)

// Anchor-chain lists, SCOPED BY COUNTRY.
//
// A single merged list was tried first and failed in production on the first
// Canadian run. "lucky" — present for the California supermarket — matched SIX
// unrelated Toronto corner shops as whole words (Lucky Convenience, Lucky Lotto
// Centre, Lucky Dollar Food Centre, Lucky Pooja Convenience and Grocery...),
// each scoring a tier-2 anchor worth 30% of the site score.
//
// The assumption behind merging was that chain names are geographically
// distinctive enough not to collide. They are not: winners, staples, target,
// ross, metro and lucky are all ordinary English words that small independent
// shops use freely. Country scoping removes the whole class rather than patching
// names one at a time — a Canadian org never evaluates "lucky", a US org never
// evaluates "metro".
//
// Tier 1 = destination anchors (score 1.0): big-box and major grocery, the kind
// of store someone drives to deliberately.
// Tier 2 = strong co-tenants (score 0.7): pharmacy, discount, specialty.

type anchorChains struct {
	Tier1 []string
	Tier2 []string
}

// chainsByCountry is keyed on ISO 3166-1 alpha-2, matching organizations.country.
// Names are written apostrophe-stripped and lowercase to match nameNorm at the
// call site ("longos", never "Longo's").
var chainsByCountry = map[string]anchorChains{
	"US": {
		Tier1: []string{
			"target", "walmart", "costco", "home depot", "lowes", "safeway",
			"trader joe", "whole foods", "dicks sporting", "kohls", "best buy", "sprouts",
		},
		Tier2: []string{
			"cvs", "walgreens", "grocery outlet", "food maxx", "99 ranch", "lucky",
			"dollar tree", "petco", "petsmart", "ross", "marshalls",
		},
	},
	"CA": {
		Tier1: []string{
			// Big-box with real Canadian presence
			"walmart", "costco", "home depot", "canadian tire", "rona", "ikea",
			// The five major grocers and their banners
			"real canadian superstore", "loblaws", "no frills", "metro", "sobeys",
			"food basics", "freshco", "fortinos", "zehrs", "longos",
			"your independent grocer",
		},
		Tier2: []string{
			"shoppers drug mart", "rexall", "dollarama", "giant tiger", "winners",
			"homesense", "marshalls", "sport chek", "pet valu", "farm boy",
			"valu-mart", "staples", "best buy",
		},
	},
}

// defaultAnchorCountry is used when an organization has no country recorded.
// US because every organization predating the country column is US-based.
const defaultAnchorCountry = "US"

// chainsFor returns the anchor lists for a country code, falling back to the
// default. An unknown country returns the default rather than an empty list:
// scoring nothing as an anchor would silently zero 30% of every site score,
// which is a worse failure than a few irrelevant chain names that simply never
// match.
func chainsFor(country string) anchorChains {
	c := strings.ToUpper(strings.TrimSpace(country))
	if ch, ok := chainsByCountry[c]; ok {
		return ch
	}
	return chainsByCountry[defaultAnchorCountry]
}

// organizationCountry reads this organization's ISO country code.
//
// Returns the default on any failure — a missing row, an older database without
// the column, a query error. Getting this wrong costs a slightly wrong chain
// list; failing the whole recommendation run over it would be far worse.
func organizationCountry(db *orgdb.DB) string {
	if db == nil {
		return defaultAnchorCountry
	}
	// MUST filter by id explicitly. `organizations` carries an org_catalog_read
	// policy alongside org_isolation — the registry is deliberately readable
	// across tenants — so a bare "LIMIT 1" is NOT scoped by RLS the way a tenant
	// table would be. It returned whichever row Postgres handed back first,
	// which was ropacal (US), so a Canadian org silently scored against US chain
	// names. An earlier version of this comment asserted the opposite, which is
	// exactly why the bug was invisible: the query succeeded, logged nothing,
	// and returned a plausible answer.
	orgID := db.OrgID()
	if orgID == "" {
		// Passthrough handle (single-tenant/dark mode): there is only one
		// organization, so an unfiltered read is unambiguous.
		var only string
		if err := db.Get(&only, `SELECT COALESCE(country, '') FROM organizations LIMIT 1`); err != nil {
			log.Printf("⚠️  [Anchor] could not read organization country (%v) — using %s chains", err, defaultAnchorCountry)
			return defaultAnchorCountry
		}
		if strings.TrimSpace(only) == "" {
			return defaultAnchorCountry
		}
		return only
	}
	var country string
	if err := db.Get(&country, `SELECT COALESCE(country, '') FROM organizations WHERE id = $1`, orgID); err != nil {
		log.Printf("⚠️  [Anchor] could not read country for organization %s (%v) — using %s chains", orgID, err, defaultAnchorCountry)
		return defaultAnchorCountry
	}
	if strings.TrimSpace(country) == "" {
		return defaultAnchorCountry
	}
	log.Printf("🏷️  [Anchor] organization %s country=%s — using %s chain list", orgID, country, strings.ToUpper(country))
	return country
}
