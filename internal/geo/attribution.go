package geo

// DataAttribution is one embedded dataset's required credit line.
type DataAttribution struct {
	Dataset string `json:"dataset"`
	Notice  string `json:"notice"`
	URL     string `json:"url"`
}

// DataAttributions lists the credit lines the embedded geo assets REQUIRE.
//
// These are licence obligations, not courtesies: GeoNames is CC BY 4.0, the LA
// Times neighborhood set is CC BY 4.0, and the Ontario municipal layer is under
// the Open Government Licence - Ontario. All three oblige us to credit the
// source wherever the data is surfaced.
//
// NOT YET RENDERED ANYWHERE IN THE UI. Keeping the list here makes the
// obligation explicit and gives the dashboard a single place to read from; a
// comment in a source file is not attribution to an end user. Whoever wires the
// map/recommendation surfaces should render these.
//
// TIGER/Line (the California city polygons) is US Government public domain and
// needs no notice, so it is deliberately absent.
func DataAttributions() []DataAttribution {
	return []DataAttribution{
		{
			Dataset: "City gazetteer (populations, coordinates)",
			Notice:  "Includes data from GeoNames, CC BY 4.0.",
			URL:     "https://www.geonames.org/",
		},
		{
			Dataset: "Ontario municipal boundaries",
			Notice:  "Contains information licensed under the Open Government Licence – Ontario.",
			URL:     "https://www.ontario.ca/page/open-government-licence-ontario",
		},
		{
			Dataset: "Los Angeles neighborhood boundaries",
			Notice:  "Los Angeles Times, “Mapping L.A.”, CC BY 4.0.",
			URL:     "http://maps.latimes.com/neighborhoods/",
		},
	}
}
