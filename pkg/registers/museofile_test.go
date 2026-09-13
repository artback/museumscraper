package registers

import (
	"slices"
	"testing"
)

const museofileHeader = "Identifiant|Nom_officiel|Ville|URL|Domaine_thematique|Coordonnees\n"

func TestParseMuseofile(t *testing.T) {
	museums, err := parseMuseofile([]byte(museofileHeader +
		"M1128|musée des sapeurs-pompiers de Lyon|Lyon|https://museepompiers.com/|Ethnologie;Histoire;Technique et industrie|45.7905, 4.7974\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(museums) != 1 {
		t.Fatalf("got %d museums, want 1", len(museums))
	}

	m := museums[0]
	if m.Name != "musée des sapeurs-pompiers de Lyon" {
		t.Errorf("Name = %q — a register that cases its own names properly must be left alone", m.Name)
	}
	if m.Locality != "Lyon" || m.Website != "https://museepompiers.com/" || m.SourcePage != "M1128" {
		t.Errorf("unexpected record: %+v", m)
	}
	if m.Latitude != 45.7905 || m.Longitude != 4.7974 {
		t.Errorf("coordinates = %f,%f", m.Latitude, m.Longitude)
	}
	want := []string{"Ethnologie", "Histoire", "Technique et industrie"}
	if !slices.Equal(m.Classes, want) {
		t.Errorf("Classes = %v, want %v", m.Classes, want)
	}
}

// TestThemesSplitsOnEitherSeparator: the export has used both, and a separator
// read as part of a value turns three subjects into one nonsense one.
func TestThemesSplitsOnEitherSeparator(t *testing.T) {
	want := []string{"Archéologie", "Beaux-Arts"}
	for _, raw := range []string{
		"Archéologie;Beaux-Arts",
		"Archéologie,Beaux-Arts",
		"['Archéologie', 'Beaux-Arts']",
		" Archéologie ; Beaux-Arts ",
	} {
		if got := themes(raw); !slices.Equal(got, want) {
			t.Errorf("themes(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestParseLatLon(t *testing.T) {
	cases := []struct {
		raw      string
		lat, lon float64
		ok       bool
	}{
		{"45.7905, 4.7974", 45.7905, 4.7974, true},
		{"-33.8,151.2", -33.8, 151.2, true},
		{"", 0, 0, false},
		{"45.7905", 0, 0, false},
		{"0, 0", 0, 0, false},      // null island, not a location
		{"95.0, 4.0", 0, 0, false}, // out of range
		{"not, coordinates", 0, 0, false},
	}
	for _, c := range cases {
		lat, lon, ok := parseLatLon(c.raw)
		if ok != c.ok || lat != c.lat || lon != c.lon {
			t.Errorf("parseLatLon(%q) = %f,%f,%v, want %f,%f,%v", c.raw, lat, lon, ok, c.lat, c.lon, c.ok)
		}
	}
}

func TestParseMuseofileSkipsUnnamedRows(t *testing.T) {
	museums, err := parseMuseofile([]byte(museofileHeader +
		"M1|  |Lyon|||45.7,4.7\n" +
		"M2|musée réel|Lyon|||45.8,4.8\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(museums) != 1 || museums[0].Name != "musée réel" {
		t.Errorf("got %+v, want only the named row", museums)
	}
}
