package geo

import "strings"

var countries = []string{
	"Afghanistan", "Albania", "Algeria", "Andorra", "Angola", "Antigua and Barbuda", "Argentina", "Armenia", "Australia", "Austria", "Azerbaijan",
	"Bahamas", "Bahrain", "Bangladesh", "Barbados", "Belarus", "Belgium", "Belize", "Benin", "Bhutan", "Bolivia", "Bosnia and Herzegovina", "Botswana", "Brazil", "Brunei", "Bulgaria", "Burkina Faso", "Burundi",
	"Denmark", "Djibouti", "Dominica", "Dominican Republic",
	"East Timor", "Ecuador", "Egypt", "El Salvador", "Equatorial Guinea", "Eritrea", "Estonia", "Eswatini", "Ethiopia",
	"Fiji", "Finland", "France",
	"Gabon", "Gambia", "Georgia", "Germany", "Ghana", "Greece", "Grenada", "Guatemala", "Guinea", "Guinea-Bissau", "Guyana",
	"Haiti", "Honduras", "Hungary",
	"Iceland", "India", "Indonesia", "Iran", "Iraq", "Ireland", "Israel", "Italy",
	"Jamaica", "Japan", "Jordan",
	"Kazakhstan", "Kenya", "Kiribati", "North Korea", "South Korea", "Kuwait", "Kyrgyzstan",
	"Laos", "Latvia", "Lebanon", "Lesotho", "Liberia", "Libya", "Liechtenstein", "Lithuania", "Luxembourg",
	"Madagascar", "Malawi", "Malaysia", "Maldives", "Mali", "Malta", "Marshall Islands", "Mauritania", "Mauritius", "Mexico", "Micronesia", "Moldova", "Monaco", "Mongolia", "Montenegro", "Morocco", "Mozambique", "Myanmar",
	"Namibia", "Nauru", "Nepal", "Netherlands", "New Zealand", "Nicaragua", "Niger", "Nigeria", "North Macedonia", "Norway",
	"Oman",
	"Pakistan", "Palau", "Panama", "Papua New Guinea", "Paraguay", "Peru", "Philippines", "Poland", "Portugal",
	"Qatar",
	"Romania", "Russia", "Rwanda",
	"Saint Kitts and Nevis", "Saint Lucia", "Saint Vincent and the Grenadines", "Samoa", "San Marino", "Sao Tome and Principe", "Saudi Arabia", "Senegal", "Serbia", "Seychelles", "Sierra Leone", "Singapore", "Slovakia", "Slovenia", "Solomon Islands", "Somalia", "South Africa", "South Sudan", "Spain", "Sri Lanka", "Sudan", "Suriname", "Sweden", "Switzerland", "Syria",
	"Taiwan", "Tajikistan", "Tanzania", "Thailand", "Togo", "Tonga", "Trinidad and Tobago", "Tunisia", "Turkey", "Turkmenistan", "Tuvalu", "the Federated States of Micronesia",
	"Uganda", "Ukraine", "United Arab Emirates", "United Kingdom", "United States", "Uruguay", "Uzbekistan",
	"Vanuatu", "Vatican City", "Venezuela", "Vietnam",
	"Yemen",
	"Zambia", "Zimbabwe",
}

func IsCountry(place string) bool {
	for _, c := range countries {
		if strings.EqualFold(c, place) {
			return true
		}
	}
	return false
}

func IdentifyPlace(place string) string {
	if IsCountry(place) {
		return "country"
	}
	return "city"
}

func ExtractCountry(text string) string {
	text = strings.TrimSpace(text)

	// Look for common prepositions
	prepositions := []string{" in ", " at "}
	candidate := ""
	for _, prep := range prepositions {
		if idx := strings.LastIndex(strings.ToLower(text), prep); idx != -1 {
			candidate = strings.TrimSpace(text[idx+len(prep):])
			break
		}
	}

	if candidate == "" {
		return ""
	}

	// Match against known countries
	for _, c := range countries {
		if strings.EqualFold(c, candidate) {
			return c
		}
	}

	return candidate
}
