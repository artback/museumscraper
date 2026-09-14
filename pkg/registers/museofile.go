package registers

import (
	"strconv"
	"strings"

	"museum/internal/models"
	"museum/internal/search"
)

// MuseofileSource identifies records from the French museum register.
const MuseofileSource = "museofile"

// museofile is Muséofile, the French ministry of culture's register of the
// institutions holding the "Musée de France" designation.
//
// Smaller than the American file by a factor of ten and considerably better
// kept: it is maintained rather than snapshotted, and the designation is a
// legal status, so a museum is in it because the state says it is a museum.
// That makes it the cleanest evidence in the catalogue about any French museum
// — and it names the museum as the museum names itself, which is what the
// wiki sources most often get wrong about France.
var museofile = Dataset{
	Source:  MuseofileSource,
	Country: "France",
	URL:     "https://ministere-culture.s3.sbg.io.cloud.ovh.net/POP/museofile.csv",
	Parse:   parseMuseofile,
}

// parseMuseofile reads the register's pipe-delimited export.
func parseMuseofile(data []byte) ([]models.Museum, error) {
	rows, err := readCSV(data, '|')
	if err != nil {
		return nil, err
	}

	museums := make([]models.Museum, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))

	for _, row := range rows {
		museum, ok := museofileMuseum(row)
		if !ok {
			continue
		}
		key := search.Normalize(museum.Name) + "|" + strings.ToLower(museum.Locality)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		museums = append(museums, museum)
	}
	return museums, nil
}

// museofileMuseum converts one row, reporting false for a row with no name.
func museofileMuseum(row map[string]string) (models.Museum, bool) {
	name := strings.TrimSpace(row["Nom_officiel"])
	if name == "" {
		return models.Museum{}, false
	}

	museum := models.Museum{
		Name:     name,
		Locality: strings.TrimSpace(row["Ville"]),
		Website:  cleanURL(row["URL"]),
		// The register's own identifier ("M1128"), which is also what the
		// ministry's own pages are keyed by.
		SourcePage: strings.TrimSpace(row["Identifiant"]),
	}

	if lat, lon, ok := parseLatLon(row["Coordonnees"]); ok {
		museum.Latitude, museum.Longitude = lat, lon
	}
	if theme := strings.TrimSpace(row["Domaine_thematique"]); theme != "" {
		museum.Classes = themes(theme)
	}
	return museum, true
}

// parseLatLon reads the "lat, lon" pair the register stores as one field.
func parseLatLon(raw string) (lat, lon float64, ok bool) {
	first, second, found := strings.Cut(strings.TrimSpace(raw), ",")
	if !found {
		return 0, 0, false
	}

	lat, err := strconv.ParseFloat(strings.TrimSpace(first), 64)
	if err != nil {
		return 0, 0, false
	}
	lon, err = strconv.ParseFloat(strings.TrimSpace(second), 64)
	if err != nil {
		return 0, 0, false
	}
	if lat == 0 || lon == 0 || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, false
	}
	return lat, lon, true
}

// themes splits the register's thematic field, which holds a list of subjects
// in one string:
//
//	Ethnologie;Histoire;Technique et industrie
//
// Kept because it is the same kind of statement as Wikidata's P31 labels — what
// this museum is about, in the source's own words — and the catalogue already
// carries those as Classes. Semicolons and commas both, because the export has
// used each: a separator read as part of a value turns three subjects into one
// nonsense one.
func themes(raw string) []string {
	raw = strings.Trim(strings.TrimSpace(raw), "[]")

	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == ',' }) {
		part = strings.Trim(strings.TrimSpace(part), "'\"")
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
