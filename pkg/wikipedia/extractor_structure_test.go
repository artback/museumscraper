package wikipedia

import (
	"slices"
	"testing"
)

// TestExtract_NestedListsSkipGroupHeaders covers the shape used by
// "List of museums in France", where "* [[Town]]" groups "** [[Museum]]".
func TestExtract_NestedListsSkipGroupHeaders(t *testing.T) {
	content := `==[[Auvergne-Rhône-Alpes]]==
===01 - [[Ain]]===

* [[Ambérieu-en-Bugey]]
** [[Musée du cheminot]]
* [[Bourg-en-Bresse]]
** [[Municipal Museum of Bourg-en-Bresse]]
** [[Musée départemental des Pays de l'Ain]]
`

	got := NewMuseumExtractor(nil).Extract(content)

	want := []Candidate{
		{Title: "Musée du cheminot", Locality: "Ambérieu-en-Bugey"},
		{Title: "Municipal Museum of Bourg-en-Bresse", Locality: "Bourg-en-Bresse"},
		{Title: "Musée départemental des Pays de l'Ain", Locality: "Bourg-en-Bresse"},
	}
	assertCandidates(t, got.Candidates, want)
}

// TestExtract_WikitableUsesNameColumn covers the shape used by
// "List of museums in Ghana": the museum is in the "Name" column, held in a
// row-scoped header cell, while other columns hold photos, towns and dates.
func TestExtract_WikitableUsesNameColumn(t *testing.T) {
	content := `== List ==
{|class="wikitable sortable plainrowheaders"
!Name
! class="unsortable" | Photo
!Location
!Year established
|-
!scope="row" | [[Armed Forces Museum (Ghana)|Armed Forces Museum]]
|[[File:Some Photo.jpg|150px]]
|[[Kumasi]]
|1953
|-
!scope="row" | [[Bisa Aberwa Museum]]
|
|[[Sekondi-Takoradi]]
|2019
|}
`

	got := NewMuseumExtractor(nil).Extract(content)

	want := []Candidate{
		{Title: "Armed Forces Museum (Ghana)", Locality: "Kumasi"},
		{Title: "Bisa Aberwa Museum", Locality: "Sekondi-Takoradi"},
	}
	assertCandidates(t, got.Candidates, want)
}

// TestExtract_InlineTableCells checks the "|| separated" table form.
func TestExtract_InlineTableCells(t *testing.T) {
	content := `{| class="wikitable"
! Museum !! Town !! Type
|-
| [[Ulster Museum]] || [[Belfast]] || Art
|-
| [[Titanic Belfast]] || [[Belfast]] || Maritime
|}
`

	got := NewMuseumExtractor(nil).Extract(content)

	want := []Candidate{
		{Title: "Ulster Museum", Locality: "Belfast"},
		{Title: "Titanic Belfast", Locality: "Belfast"},
	}
	assertCandidates(t, got.Candidates, want)
}

func TestExtract_SkipsNoiseSections(t *testing.T) {
	content := `* [[Museo Torres García]]

== See also ==
* [[List of museums in Montevideo]]
* [[List of museums by country]]

== References ==
* [[Some Publisher]]

== External links ==
* [[Another Thing]]
`

	got := NewMuseumExtractor(nil).Extract(content)

	assertCandidates(t, got.Candidates, []Candidate{{Title: "Museo Torres García"}})

	// Museum lists linked from "See also" are still worth following — that is
	// where sub-lists like the Montevideo one are announced — even though none
	// of the section's links may be emitted as museums.
	for _, want := range []string{"List of museums in Montevideo", "List of museums by country"} {
		if !slices.Contains(got.NestedLists, want) {
			t.Errorf("NestedLists = %v, want it to contain %q", got.NestedLists, want)
		}
	}

	// Non-museum links in the skipped section are ignored entirely.
	for _, unwanted := range []string{"Some Publisher", "Another Thing"} {
		if slices.Contains(got.NestedLists, unwanted) {
			t.Errorf("NestedLists = %v, should not contain %q", got.NestedLists, unwanted)
		}
	}
}

func TestExtract_DropsMarkupNoise(t *testing.T) {
	content := `<!-- * [[Commented Out Museum]] -->
* [[Real Museum]]<ref>{{cite web|url=http://x|publisher=[[International Council of Museums]]}}</ref>
* [[File:Photo.jpg|thumb|A museum]]
* [[Category:Museums in Testland]]
* [[fr:Musée français]]
* [[:de:Deutsches Museum]]
* [[1953]]
* [[Second Museum#History|Second]] in [[Some City]]
* Plain text with an [http://example.com external link]
`

	got := NewMuseumExtractor(nil).Extract(content)

	want := []Candidate{
		{Title: "Real Museum"},
		{Title: "Second Museum"},
	}
	assertCandidates(t, got.Candidates, want)
}

func TestExtract_DeduplicatesWithinPage(t *testing.T) {
	content := `* [[Repeated Museum]]
* [[Repeated Museum]]
* [[repeated Museum]]
`

	got := NewMuseumExtractor(nil).Extract(content)
	assertCandidates(t, got.Candidates, []Candidate{{Title: "Repeated Museum"}})
}

func TestNormalizeTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Regression: slicing the first byte split the two-byte "Ü" and
		// produced a title the API could not resolve.
		{"multi-byte first letter", "Übersee Museum Bremen", "Übersee Museum Bremen"},
		{"lowercase first letter is raised", "musée du cheminot", "Musée du cheminot"},
		{"underscores become spaces", "Museum_of_Art", "Museum of Art"},
		{"fragment removed", "Museum of Art#Collection", "Museum of Art"},
		{"leading colon removed", ":Museum of Art", "Museum of Art"},
		{"html entity decoded", "Art &amp; Design Museum", "Art & Design Museum"},
		{"whitespace collapsed", "  Museum   of   Art  ", "Museum of Art"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeTitle(tc.in); got != tc.want {
				t.Errorf("normalizeTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name         string
		meta         PageMetadata
		wantDecision Decision
		wantReason   Rejection
	}{
		{
			name:         "museum by description",
			meta:         PageMetadata{Title: "Le Magasin", Description: "Museum in Grenoble, France"},
			wantDecision: Accept,
		},
		{
			name:         "museum with no description is kept",
			meta:         PageMetadata{Title: "Palacio Taranco"},
			wantDecision: Accept,
		},
		{
			name:         "settlement rejected",
			meta:         PageMetadata{Title: "Kumasi", Description: "City in Ashanti Region, Ghana"},
			wantDecision: Reject,
			wantReason:   RejectSettlement,
		},
		{
			name:         "person rejected by profession",
			meta:         PageMetadata{Title: "Martiros Saryan", Description: "Armenian painter"},
			wantDecision: Reject,
			wantReason:   RejectPerson,
		},
		{
			name:         "person rejected by lifespan",
			meta:         PageMetadata{Title: "Andranik", Description: "Armenian military leader (1865–1927)"},
			wantDecision: Reject,
			wantReason:   RejectPerson,
		},
		{
			name:         "museum keyword beats place-shaped description",
			meta:         PageMetadata{Title: "Museum Island", Description: "District of Berlin, Germany"},
			wantDecision: Accept,
		},
		{
			name:         "disambiguation rejected",
			meta:         PageMetadata{Title: "Mercury", IsDisambiguation: true},
			wantDecision: Reject,
			wantReason:   RejectDisambiguation,
		},
		{
			name:         "red link kept as unverified",
			meta:         PageMetadata{Title: "Musée du cheminot", Missing: true},
			wantDecision: AcceptUnverified,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision, reason := Classify(tc.meta)
			if decision != tc.wantDecision {
				t.Errorf("decision = %v, want %v (reason %q)", decision, tc.wantDecision, reason)
			}
			if tc.wantReason != "" && reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

func TestCountryFor(t *testing.T) {
	cases := []struct {
		name  string
		title string
		hint  string
		want  string
	}{
		{"country in title", "List of museums in France", "", "France"},
		{"definite article tolerated", "List of museums in the Netherlands", "", "Netherlands"},
		{"city title falls back to hint", "List of museums in Yerevan", "Armenia", "Armenia"},
		{"of form", "Museums of Spain", "", "Spain"},
		{"no country anywhere", "List of museums in Atlantis", "", "Atlantis"},
		{"nothing at all", "Museums", "", "unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countryFor(tc.title, tc.hint); got != tc.want {
				t.Errorf("countryFor(%q, %q) = %q, want %q", tc.title, tc.hint, got, tc.want)
			}
		})
	}
}

// assertCandidates compares extraction output against the expected entries,
// ignoring locality when the expectation leaves it empty.
func assertCandidates(t *testing.T, got, want []Candidate) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Title != want[i].Title {
			t.Errorf("candidate %d: title = %q, want %q", i, got[i].Title, want[i].Title)
		}
		if want[i].Locality != "" && got[i].Locality != want[i].Locality {
			t.Errorf("candidate %d (%s): locality = %q, want %q", i, want[i].Title, got[i].Locality, want[i].Locality)
		}
	}
}
