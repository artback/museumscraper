package location

import (
	"strings"
)

// museumOSMTypes are the OSM class+type combinations that indicate a museum.
var museumOSMTypes = map[string]map[string]bool{
	"tourism": {
		"museum":       true,
		"gallery":      true,
		"artwork":      true,
		"attraction":   true,
	},
	"amenity": {
		"arts_centre": true,
	},
	"building": {
		"museum": true,
	},
}

// museumNameKeywords are substrings in names that strongly indicate a museum,
// even if the OSM class/type doesn't match exactly.
var museumNameKeywords = []string{
	"museum", "gallery", "galerie", "musée", "museo", "muzeum", "muzej",
	"pinakothek", "pinacoteca", "kunsthalle", "exhibition",
}

// IsMuseum returns true if the geocoding result likely represents a museum
// based on its OSM classification and name.
func (g *GeoResult) IsMuseum() bool {
	// Check OSM class/type
	if types, ok := museumOSMTypes[g.Class]; ok {
		if types[g.Type] {
			return true
		}
	}

	// Fall back to name-based heuristic
	lower := strings.ToLower(g.Name + " " + g.DisplayName)
	for _, kw := range museumNameKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}

	return false
}
