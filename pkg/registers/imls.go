package registers

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"museum/internal/models"
	"museum/internal/search"
)

// IMLSSource identifies records from the United States museum file.
const IMLSSource = "imls"

// imls is the Institute of Museum and Library Services' museum file: every
// museum the agency could identify in the fifty states and DC, with a position
// for all of them and a website for about two thirds.
//
// A 2018 snapshot, and the agency has said there will be no further releases.
// It is used anyway because nothing else covers small American museums at all —
// the county historical museum that never acquired an article or a map pin is
// exactly what this file is made of — and because every record carries
// coordinates, which is the field the catalogue is otherwise shortest of.
var imls = Dataset{
	Source:  IMLSSource,
	Country: "United States",
	URL:     "https://www.imls.gov/sites/default/files/2018_csv_museum_data_files.zip",
	Parse:   parseIMLS,
}

// museumDisciplines are the IMLS discipline codes this catalogue treats as
// museums.
//
// The file covers more than museums, and the three codes left out are left out
// for the reason the OSM query leaves out arts centres and archaeological
// sites: they sit next to museums without being them, and nothing downstream
// could tell the difference afterwards.
//
//	HSC  historical societies and historic preservation — 14,785 records, the
//	     largest group in the file and mostly organisations rather than places
//	     open to visitors. Some run a museum; the file does not say which, so
//	     admitting them would add fourteen thousand records of which an unknown
//	     fraction are museums.
//	BOT  arboretums, botanical gardens and nature centres — 1,029
//	ZAW  zoos, aquariums and wildlife conservation — 465
//
// That leaves 13,898 of 30,178. The line is the same one the rest of the
// catalogue draws, and it is drawn here rather than downstream because the
// discipline code is the only place the distinction is recorded.
var museumDisciplines = map[string]string{
	"ART": "art museum",
	"CMU": "children's museum",
	"GMU": "general museum",
	"HST": "history museum",
	"NAT": "natural history museum",
	"SCI": "science museum",
}

// parseIMLS reads the three CSVs inside the published zip.
func parseIMLS(data []byte) ([]models.Museum, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}

	var (
		museums []models.Museum
		// seen collapses the duplicates the file itself carries: an institution
		// occasionally appears in two of the three discipline files.
		seen = make(map[string]struct{})
	)

	for _, file := range archive.File {
		if !strings.HasSuffix(strings.ToLower(file.Name), ".csv") {
			continue
		}

		contents, err := readZipFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file.Name, err)
		}

		rows, err := readCSV(contents, ',')
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", file.Name, err)
		}

		for _, row := range rows {
			museum, ok := imlsMuseum(row)
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
	}
	return museums, nil
}

// imlsMuseum converts one row, reporting false for a row this catalogue does
// not treat as a museum.
func imlsMuseum(row map[string]string) (models.Museum, bool) {
	// The column is DISCIPL in one file and DISCIPLINE in the other two, which
	// is the sort of thing that silently drops a third of a dataset.
	discipline := strings.ToUpper(strings.TrimSpace(firstOf(row, "DISCIPL", "DISCIPLINE")))
	class, wanted := museumDisciplines[discipline]
	if !wanted {
		return models.Museum{}, false
	}

	name := titleCase(firstOf(row, "COMMONNAME", "LEGALNAME"))
	if name == "" {
		return models.Museum{}, false
	}

	museum := models.Museum{
		Name:     name,
		Locality: titleCase(firstOf(row, "ADCITY", "GCITY", "PHCITY")),
		Website:  cleanURL(row["WEBURL"]),
		Classes:  []string{class},
		// The register's own identifier, so a surprising record can be looked
		// up in the file it came from.
		SourcePage: strings.TrimSpace(row["MID"]),
	}

	// Every row in the file has coordinates, but a row that somehow does not
	// must not be stored at 0,0 — which is in the Atlantic, and is how a
	// catalogue grows a cluster of museums off the coast of Ghana.
	lat, latOK := parseCoord(row["LATITUDE"], 90)
	lon, lonOK := parseCoord(row["LONGITUDE"], 180)
	if latOK && lonOK {
		museum.Latitude, museum.Longitude = lat, lon
	}

	museum.AlsoKnownAs = aliases(name, row)
	return museum, true
}

// aliases keeps the other names on a row, but only the ones that plausibly name
// the same institution.
//
// The legal name is matched from IRS filings and is sometimes a different
// museum altogether: the row for the History Museum of Mobile carries "Mobile
// Medical Museum Inc", which is a real and separate museum in the same city.
// 1,574 of 7,431 rows in one file have a legal name that is not a variant of
// the common one.
//
// That matters more than a wrong label would, because the merger matches on
// every name a source supplies: a bad alias does not sit there looking odd, it
// folds two distinct museums into one record and there is nothing downstream
// that could tell. So an alias is kept only where one name contains the other
// once corporate suffixes are stripped — which drops some genuine variants
// along with the wrong ones, and that is the right way to be wrong here.
func aliases(name string, row map[string]string) []string {
	var out []string
	seen := map[string]struct{}{search.Normalize(name): {}}
	base := withoutCorporateSuffix(name)

	for _, column := range []string{"LEGALNAME", "ALTNAME", "AKADBA"} {
		alt := titleCase(strings.TrimSpace(row[column]))
		if alt == "" {
			continue
		}
		key := search.Normalize(alt)
		if _, dup := seen[key]; dup {
			continue
		}

		other := withoutCorporateSuffix(alt)
		if base == "" || other == "" ||
			(!strings.Contains(base, other) && !strings.Contains(other, base)) {
			continue
		}

		seen[key] = struct{}{}
		out = append(out, alt)
	}
	return out
}

// corporateSuffixes are the words a legal name ends with and a museum's own
// name does not.
var corporateSuffixes = []string{
	"incorporated", "inc", "association", "assn", "foundation", "trust",
	"corporation", "corp", "company", "llc", "ltd", "the",
}

// withoutCorporateSuffix normalises a name for comparison and strips the
// trailing legal-entity words, so "The Kentuck Museum Association Inc" and
// "Kentuck Museum" compare as the same stem.
func withoutCorporateSuffix(name string) string {
	normalised := search.Normalize(name)
	for changed := true; changed; {
		changed = false
		for _, suffix := range corporateSuffixes {
			if trimmed := strings.TrimSuffix(normalised, suffix); trimmed != normalised && trimmed != "" {
				normalised, changed = trimmed, true
			}
		}
	}
	return normalised
}

// parseCoord reads a coordinate, rejecting one outside its range or at exactly
// zero. Registers use an empty string and a literal 0 interchangeably for
// "unknown".
func parseCoord(raw string, limit float64) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || value == 0 || value < -limit || value > limit {
		return 0, false
	}
	return value, true
}

// readZipFile reads one entry, bounded like any other download.
func readZipFile(file *zip.File) ([]byte, error) {
	rc, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxDownloadBytes))
}

// firstOf returns the first of the named columns that carries a value, so a
// reader survives a register renaming a column between files or releases.
func firstOf(row map[string]string, columns ...string) string {
	for _, column := range columns {
		if value := strings.TrimSpace(row[column]); value != "" {
			return value
		}
	}
	return ""
}

// readCSV reads a delimited file into rows keyed by column name.
//
// Government registers are not always UTF-8 — the IMLS file is Latin-1, and
// reading its bytes as UTF-8 produces names Postgres will not store — and they
// are not always consistent about how many fields a row has, so a short or long
// row is taken as it comes rather than failing the file.
func readCSV(data []byte, delimiter rune) ([]map[string]string, error) {
	reader := csv.NewReader(bytes.NewReader(data))
	reader.Comma = delimiter
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	for i, column := range header {
		header[i] = strings.TrimSpace(strings.TrimPrefix(column, "\ufeff"))
	}

	var rows []map[string]string
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// One malformed row should not cost the other thirty thousand.
			continue
		}

		row := make(map[string]string, len(header))
		for i, column := range header {
			if i < len(record) {
				row[column] = decodeLatin1(record[i])
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// decodeLatin1 repairs a Latin-1 byte that arrived where UTF-8 was expected.
//
// Only where the text is not already valid UTF-8: a file that is properly
// encoded must pass through untouched, and re-decoding it would turn every
// accented character into mojibake.
func decodeLatin1(s string) string {
	if isUTF8(s) {
		return s
	}
	runes := make([]rune, 0, len(s))
	for i := 0; i < len(s); i++ {
		runes = append(runes, rune(s[i]))
	}
	return string(runes)
}

// isUTF8 reports whether s is already valid UTF-8.
func isUTF8(s string) bool { return utf8.ValidString(s) }
