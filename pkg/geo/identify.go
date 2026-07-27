// Package geo provides heuristics for identifying countries in free-form text,
// primarily Wikipedia page titles such as "List of museums in France".
package geo

import (
	"slices"
	"strings"
)

// countries lists UN member states plus the naming variants Wikipedia uses
// interchangeably ("Czech Republic" and "Czechia", "Ivory Coast" and
// "Côte d'Ivoire"). Lookups go through countryIndex and ignore case.
var countries = []string{
	"Afghanistan", "Albania", "Algeria", "Andorra", "Angola", "Antigua and Barbuda", "Argentina", "Armenia", "Australia", "Austria", "Azerbaijan",
	"Bahamas", "Bahrain", "Bangladesh", "Barbados", "Belarus", "Belgium", "Belize", "Benin", "Bhutan", "Bolivia", "Bosnia and Herzegovina", "Botswana", "Brazil", "Brunei", "Bulgaria", "Burkina Faso", "Burundi",
	"Cabo Verde", "Cambodia", "Cameroon", "Canada", "Cape Verde", "Central African Republic", "Chad", "Chile", "China", "Colombia", "Comoros", "Costa Rica", "Croatia", "Cuba", "Cyprus", "Czech Republic", "Czechia", "Côte d'Ivoire",
	"Democratic Republic of the Congo", "Denmark", "Djibouti", "Dominica", "Dominican Republic",
	"East Timor", "Ecuador", "Egypt", "El Salvador", "Equatorial Guinea", "Eritrea", "Estonia", "Eswatini", "Ethiopia",
	"Fiji", "Finland", "France",
	"Gabon", "Gambia", "Georgia", "Germany", "Ghana", "Greece", "Grenada", "Guatemala", "Guinea", "Guinea-Bissau", "Guyana",
	"Haiti", "Honduras", "Hungary",
	"Iceland", "India", "Indonesia", "Iran", "Iraq", "Ireland", "Israel", "Italy", "Ivory Coast",
	"Jamaica", "Japan", "Jordan",
	"Kazakhstan", "Kenya", "Kiribati", "Kuwait", "Kyrgyzstan",
	"Laos", "Latvia", "Lebanon", "Lesotho", "Liberia", "Libya", "Liechtenstein", "Lithuania", "Luxembourg",
	"Madagascar", "Malawi", "Malaysia", "Maldives", "Mali", "Malta", "Marshall Islands", "Mauritania", "Mauritius", "Mexico", "Micronesia", "Moldova", "Monaco", "Mongolia", "Montenegro", "Morocco", "Mozambique", "Myanmar",
	"Namibia", "Nauru", "Nepal", "Netherlands", "New Zealand", "Nicaragua", "Niger", "Nigeria", "North Korea", "North Macedonia", "Norway",
	"Oman",
	"Pakistan", "Palau", "Palestine", "Panama", "Papua New Guinea", "Paraguay", "Peru", "Philippines", "Poland", "Portugal",
	"Qatar",
	"Republic of the Congo", "Romania", "Russia", "Rwanda",
	"Saint Kitts and Nevis", "Saint Lucia", "Saint Vincent and the Grenadines", "Samoa", "San Marino", "Sao Tome and Principe", "Saudi Arabia", "Senegal", "Serbia", "Seychelles", "Sierra Leone", "Singapore", "Slovakia", "Slovenia", "Solomon Islands", "Somalia", "South Africa", "South Korea", "South Sudan", "Spain", "Sri Lanka", "Sudan", "Suriname", "Sweden", "Switzerland", "Syria",
	"Taiwan", "Tajikistan", "Tanzania", "Thailand", "Togo", "Tonga", "Trinidad and Tobago", "Tunisia", "Turkey", "Turkmenistan", "Tuvalu", "the Federated States of Micronesia",
	"Uganda", "Ukraine", "United Arab Emirates", "United Kingdom", "United States", "Uruguay", "Uzbekistan",
	"Vanuatu", "Vatican City", "Venezuela", "Vietnam",
	"Yemen",
	"Zambia", "Zimbabwe",
}

// countryISO maps a canonical country name to its ISO 3166-1 alpha-2 code.
// Generated from Wikidata property P297, filtered to the names in countries
// above so the two lists cannot drift apart. The codes are what area-based
// OpenStreetMap queries select on.
var countryISO = map[string]string{
	"Afghanistan":                        "AF",
	"Albania":                            "AL",
	"Algeria":                            "DZ",
	"Andorra":                            "AD",
	"Angola":                             "AO",
	"Antigua and Barbuda":                "AG",
	"Argentina":                          "AR",
	"Armenia":                            "AM",
	"Australia":                          "AU",
	"Austria":                            "AT",
	"Azerbaijan":                         "AZ",
	"Bahamas":                            "BS",
	"Bahrain":                            "BH",
	"Bangladesh":                         "BD",
	"Barbados":                           "BB",
	"Belarus":                            "BY",
	"Belgium":                            "BE",
	"Belize":                             "BZ",
	"Benin":                              "BJ",
	"Bhutan":                             "BT",
	"Bolivia":                            "BO",
	"Bosnia and Herzegovina":             "BA",
	"Botswana":                           "BW",
	"Brazil":                             "BR",
	"Brunei":                             "BN",
	"Bulgaria":                           "BG",
	"Burkina Faso":                       "BF",
	"Burundi":                            "BI",
	"Cabo Verde":                         "CV",
	"Cambodia":                           "KH",
	"Cameroon":                           "CM",
	"Canada":                             "CA",
	"Cape Verde":                         "CV",
	"Central African Republic":           "CF",
	"Chad":                               "TD",
	"Chile":                              "CL",
	"China":                              "CN",
	"Colombia":                           "CO",
	"Comoros":                            "KM",
	"Costa Rica":                         "CR",
	"Croatia":                            "HR",
	"Cuba":                               "CU",
	"Cyprus":                             "CY",
	"Czech Republic":                     "CZ",
	"Czechia":                            "CZ",
	"Côte d'Ivoire":                      "CI",
	"Democratic Republic of the Congo":   "CD",
	"Denmark":                            "DK",
	"Djibouti":                           "DJ",
	"Dominica":                           "DM",
	"Dominican Republic":                 "DO",
	"East Timor":                         "TL",
	"Ecuador":                            "EC",
	"Egypt":                              "EG",
	"El Salvador":                        "SV",
	"Equatorial Guinea":                  "GQ",
	"Eritrea":                            "ER",
	"Estonia":                            "EE",
	"Eswatini":                           "SZ",
	"Ethiopia":                           "ET",
	"Fiji":                               "FJ",
	"Finland":                            "FI",
	"France":                             "FR",
	"Gabon":                              "GA",
	"Gambia":                             "GM",
	"Georgia":                            "GE",
	"Germany":                            "DE",
	"Ghana":                              "GH",
	"Greece":                             "GR",
	"Grenada":                            "GD",
	"Guatemala":                          "GT",
	"Guinea":                             "GN",
	"Guinea-Bissau":                      "GW",
	"Guyana":                             "GY",
	"Haiti":                              "HT",
	"Honduras":                           "HN",
	"Hungary":                            "HU",
	"Iceland":                            "IS",
	"India":                              "IN",
	"Indonesia":                          "ID",
	"Iran":                               "IR",
	"Iraq":                               "IQ",
	"Ireland":                            "IE",
	"Israel":                             "IL",
	"Italy":                              "IT",
	"Ivory Coast":                        "CI",
	"Jamaica":                            "JM",
	"Japan":                              "JP",
	"Jordan":                             "JO",
	"Kazakhstan":                         "KZ",
	"Kenya":                              "KE",
	"Kiribati":                           "KI",
	"Kuwait":                             "KW",
	"Kyrgyzstan":                         "KG",
	"Laos":                               "LA",
	"Latvia":                             "LV",
	"Lebanon":                            "LB",
	"Lesotho":                            "LS",
	"Liberia":                            "LR",
	"Libya":                              "LY",
	"Liechtenstein":                      "LI",
	"Lithuania":                          "LT",
	"Luxembourg":                         "LU",
	"Madagascar":                         "MG",
	"Malawi":                             "MW",
	"Malaysia":                           "MY",
	"Maldives":                           "MV",
	"Mali":                               "ML",
	"Malta":                              "MT",
	"Marshall Islands":                   "MH",
	"Mauritania":                         "MR",
	"Mauritius":                          "MU",
	"Mexico":                             "MX",
	"Micronesia":                         "FM",
	"Moldova":                            "MD",
	"Monaco":                             "MC",
	"Mongolia":                           "MN",
	"Montenegro":                         "ME",
	"Morocco":                            "MA",
	"Mozambique":                         "MZ",
	"Myanmar":                            "MM",
	"Namibia":                            "NA",
	"Nauru":                              "NR",
	"Nepal":                              "NP",
	"Netherlands":                        "NL",
	"New Zealand":                        "NZ",
	"Nicaragua":                          "NI",
	"Niger":                              "NE",
	"Nigeria":                            "NG",
	"North Korea":                        "KP",
	"North Macedonia":                    "MK",
	"Norway":                             "NO",
	"Oman":                               "OM",
	"Pakistan":                           "PK",
	"Palau":                              "PW",
	"Palestine":                          "PS",
	"Panama":                             "PA",
	"Papua New Guinea":                   "PG",
	"Paraguay":                           "PY",
	"Peru":                               "PE",
	"Philippines":                        "PH",
	"Poland":                             "PL",
	"Portugal":                           "PT",
	"Qatar":                              "QA",
	"Republic of the Congo":              "CG",
	"Romania":                            "RO",
	"Russia":                             "RU",
	"Rwanda":                             "RW",
	"Saint Kitts and Nevis":              "KN",
	"Saint Lucia":                        "LC",
	"Saint Vincent and the Grenadines":   "VC",
	"Samoa":                              "WS",
	"San Marino":                         "SM",
	"Sao Tome and Principe":              "ST",
	"Saudi Arabia":                       "SA",
	"Senegal":                            "SN",
	"Serbia":                             "RS",
	"Seychelles":                         "SC",
	"Sierra Leone":                       "SL",
	"Singapore":                          "SG",
	"Slovakia":                           "SK",
	"Slovenia":                           "SI",
	"Solomon Islands":                    "SB",
	"Somalia":                            "SO",
	"South Africa":                       "ZA",
	"South Korea":                        "KR",
	"South Sudan":                        "SS",
	"Spain":                              "ES",
	"Sri Lanka":                          "LK",
	"Sudan":                              "SD",
	"Suriname":                           "SR",
	"Sweden":                             "SE",
	"Switzerland":                        "CH",
	"Syria":                              "SY",
	"Taiwan":                             "TW",
	"Tajikistan":                         "TJ",
	"Tanzania":                           "TZ",
	"Thailand":                           "TH",
	"Togo":                               "TG",
	"Tonga":                              "TO",
	"Trinidad and Tobago":                "TT",
	"Tunisia":                            "TN",
	"Turkey":                             "TR",
	"Turkmenistan":                       "TM",
	"Tuvalu":                             "TV",
	"Uganda":                             "UG",
	"Ukraine":                            "UA",
	"United Arab Emirates":               "AE",
	"United Kingdom":                     "GB",
	"United States":                      "US",
	"Uruguay":                            "UY",
	"Uzbekistan":                         "UZ",
	"Vanuatu":                            "VU",
	"Vatican City":                       "VA",
	"Venezuela":                          "VE",
	"Vietnam":                            "VN",
	"Yemen":                              "YE",
	"Zambia":                             "ZM",
	"Zimbabwe":                           "ZW",
	"the Federated States of Micronesia": "FM",
}

// countryAliases collapse the spellings that name the same country onto one of
// them. Without this every consumer sees two countries: the merger keys a
// museum in "Czechia" differently from one in "Czech Republic" and never folds
// them together, and an audit comparing a record's country against its
// description reports 181 contradictions that are only a difference in wording.
var countryAliases = map[string]string{
	"czechia":                            "Czech Republic",
	"cabo verde":                         "Cape Verde",
	"côte d'ivoire":                      "Ivory Coast",
	"cote d'ivoire":                      "Ivory Coast",
	"the federated states of micronesia": "Micronesia",
	"east timor":                         "East Timor",
	"timor-leste":                        "East Timor",
	"holland":                            "Netherlands",
	"burma":                              "Myanmar",
	"swaziland":                          "Eswatini",
	"macedonia":                          "North Macedonia",
	"great britain":                      "United Kingdom",
	"britain":                            "United Kingdom",
	"usa":                                "United States",
	"united states of america":           "United States",
	"uae":                                "United Arab Emirates",
	"south korea":                        "South Korea",
	"republic of korea":                  "South Korea",
	"north korea":                        "North Korea",
	"vatican":                            "Vatican City",
	"holy see":                           "Vatican City",
}

// AmbiguousWithSubdivision lists names that are a country and also a
// first-level division of another country. "Atlanta, Georgia" is in the United
// States, not in the Caucasus, and treating the two as interchangeable produces
// confident nonsense.
var AmbiguousWithSubdivision = map[string][]string{
	"Georgia":    {"United States"},
	"Luxembourg": {"Belgium"},
	"Mexico":     {"Mexico"},
}

// countryIndex maps a lowercased country name to its canonical spelling, making
// lookups constant time and case-insensitive.
var countryIndex = func() map[string]string {
	index := make(map[string]string, len(countries))
	for _, c := range countries {
		index[strings.ToLower(c)] = c
	}
	return index
}()

// ISOCode returns the ISO 3166-1 alpha-2 code for a country, and whether one is
// known. The name is resolved to its canonical spelling first, so "the
// Netherlands" and "Czechia" both work.
func ISOCode(country string) (string, bool) {
	canonical, ok := Canonical(country)
	if !ok {
		return "", false
	}
	code, ok := countryISO[canonical]
	return code, ok
}

// Countries returns every country this package recognises, in canonical
// spelling and sorted, so callers can iterate a stable list.
func Countries() []string {
	names := make([]string, 0, len(countries))
	seen := make(map[string]struct{}, len(countries))
	for _, c := range countries {
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		names = append(names, c)
	}
	slices.Sort(names)
	return names
}

// IsCountry reports whether place names a known country, ignoring case.
func IsCountry(place string) bool {
	_, ok := Canonical(place)
	return ok
}

// Canonical resolves place to the canonical spelling of a country, reporting
// whether it matched. A leading article is tolerated, since Wikipedia titles
// them "List of museums in the Netherlands".
func Canonical(place string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(place))

	for _, candidate := range []string{normalized, strings.TrimPrefix(normalized, "the ")} {
		if alias, ok := countryAliases[candidate]; ok {
			return alias, true
		}
		if canonical, ok := countryIndex[candidate]; ok {
			// A canonical entry may itself be an alias of another spelling.
			if alias, ok := countryAliases[strings.ToLower(canonical)]; ok {
				return alias, true
			}
			return canonical, true
		}
	}
	return "", false
}

// IdentifyPlace classifies place as either "country" or "city". Anything not
// recognised as a country is assumed to be a city.
func IdentifyPlace(place string) string {
	if IsCountry(place) {
		return "country"
	}
	return "city"
}

// ExtractCountry pulls the place name out of a title such as
// "List of museums in France". It returns the canonical country spelling when
// the candidate is recognised, the raw candidate when it is not, and an empty
// string when no place could be located.
func ExtractCountry(text string) string {
	text = strings.TrimSpace(text)

	// Look for common prepositions.
	prepositions := []string{" in ", " at "}
	candidate := ""
	for _, prep := range prepositions {
		if idx := lastIndexFold(text, prep); idx != -1 {
			candidate = strings.TrimSpace(text[idx+len(prep):])
			break
		}
	}

	if candidate == "" {
		return ""
	}

	if canonical, ok := Canonical(candidate); ok {
		return canonical
	}
	return candidate
}

// lastIndexFold returns the byte offset in text of the last case-insensitive
// occurrence of sep, or -1. Lowercasing can change byte length for non-ASCII
// input, so the lowered copy is only used for the search when its length still
// lines up with the original.
func lastIndexFold(text, sep string) int {
	lower := strings.ToLower(text)
	if len(lower) != len(text) {
		return strings.LastIndex(text, sep)
	}
	return strings.LastIndex(lower, sep)
}
