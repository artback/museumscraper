package harvest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/artback/museumscraper/extract"
	"museum/internal/models"
)

func TestExhibitionSchemaIsValid(t *testing.T) {
	// The schema is written by hand and is what every generated extractor is
	// graded against. A defect in it would not fail loudly anywhere else.
	if err := ExhibitionSchema().Validate(); err != nil {
		t.Fatalf("ExhibitionSchema().Validate() error = %v", err)
	}

	schema := ExhibitionSchema()
	for _, name := range []string{"title", "url"} {
		field, ok := schema.Field(name)
		if !ok {
			t.Errorf("ExhibitionSchema() has no %q field", name)
			continue
		}
		if !field.Required {
			t.Errorf("ExhibitionSchema() field %q is not required; an entry without one is not an exhibition", name)
		}
	}
}

func TestSourceName(t *testing.T) {
	tests := []struct {
		name    string
		website string
		want    string
		wantErr bool
	}{
		{name: "plain host", website: "https://www.tate.org.uk/", want: "tate-org-uk"},
		{name: "no scheme", website: "moma.org", want: "moma-org"},
		{name: "path is ignored", website: "https://example.org/whats-on?x=1", want: "example-org"},
		{name: "case is folded", website: "HTTPS://Example.ORG", want: "example-org"},
		{
			// Tate publishes four galleries on one domain sharing one listing
			// page. Four sources would spend four model invocations producing
			// the same script.
			name: "subdomains stay distinct", website: "https://shop.tate.org.uk", want: "shop-tate-org-uk",
		},
		{name: "not a URL", website: "   ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SourceName(tt.website)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("SourceName(%q) error = %v, want error presence = %t", tt.website, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("SourceName(%q) = %q, want %q", tt.website, got, tt.want)
			}
		})
	}
}

func TestAsExhibitions(t *testing.T) {
	now := time.Date(2026, time.August, 15, 0, 0, 0, 0, time.UTC)
	fallback := &ExhibitionFallback{Now: func() time.Time { return now }}

	museum := models.Museum{
		Name:       "Example Museum",
		Website:    "https://example.org",
		WikidataID: "Q123",
		Latitude:   57.7,
		Longitude:  11.97,
	}

	records := []extract.Record{
		{"title": "Running Now", "url": "https://example.org/a", "start": "2026-06-01", "end": "2026-12-01"},
		{"title": "Opens Later", "url": "https://example.org/b", "start": "2026-11-01", "end": "2027-02-01"},
		{"title": "Always On", "url": "https://example.org/c", "permanent": true, "end": "2027-01-01"},
		{"title": "", "url": "https://example.org/d"}, // no title, dropped
		{"title": "No Link"}, // no url, dropped
	}

	got := fallback.asExhibitions(museum, records)
	if len(got) != 3 {
		t.Fatalf("asExhibitions() returned %d exhibitions, want 3: %+v", len(got), got)
	}

	byTitle := make(map[string]int, len(got))
	for i, e := range got {
		byTitle[e.Title] = i
	}

	running := got[byTitle["Running Now"]]
	if !running.Running || running.Upcoming {
		t.Errorf("Running Now: Running = %t, Upcoming = %t, want true, false", running.Running, running.Upcoming)
	}

	later := got[byTitle["Opens Later"]]
	if later.Running || !later.Upcoming {
		t.Errorf("Opens Later: Running = %t, Upcoming = %t, want false, true", later.Running, later.Upcoming)
	}

	// A permanent display has no dates by definition: it has none because it
	// has no end, not because the listing failed to give them. The catalogue's
	// own quality checks report a permanent entry carrying a closing date as
	// an error, so the flag has to win.
	permanent := got[byTitle["Always On"]]
	switch {
	case !permanent.Permanent:
		t.Error("Always On lost its permanent flag")
	case permanent.End != nil:
		t.Errorf("Always On kept a closing date (%v) despite being permanent", permanent.End)
	case !permanent.Running:
		t.Error("Always On is not marked running; a permanent display always is")
	}

	// The museum's identity and position come from the catalogue, never from
	// the page.
	if running.MuseumWikidataID != "Q123" || running.Latitude != 57.7 {
		t.Errorf("asExhibitions() did not carry the museum's identity onto the exhibition: %+v", running)
	}
	if running.ScrapedAt != now {
		t.Errorf("ScrapedAt = %v, want %v", running.ScrapedAt, now)
	}
}

func TestRecordDate(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  bool
	}{
		{name: "ISO date", value: "2026-09-01", want: true},
		{name: "RFC3339", value: "2026-09-01T18:00:00Z", want: true},
		{name: "year and month", value: "2026-09", want: true},
		{name: "absent", value: nil},
		{name: "empty", value: "  "},
		// Human-readable dates never reach here: the validator rejects them,
		// because emitting one means the artifact read the prose instead of
		// the datetime attribute beside it.
		{name: "human readable", value: "15 January"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := extract.Record{}
			if tt.value != nil {
				record["start"] = tt.value
			}

			got := recordDate(record, "start")
			if gotOK := got != nil; gotOK != tt.want {
				t.Errorf("recordDate(%v) = %v, want parsed = %t", tt.value, got, tt.want)
			}
		})
	}
}

func TestCompileBudget(t *testing.T) {
	fallback := &ExhibitionFallback{MaxCompiles: 2}

	// Without a cap, the first refresh after this was switched on would try to
	// compile an extractor for every unreadable site in the catalogue, at
	// minutes each.
	if !fallback.claimCompile() || !fallback.claimCompile() {
		t.Fatal("claimCompile() refused within the budget")
	}
	if fallback.claimCompile() {
		t.Error("claimCompile() allowed a third compile with a budget of 2")
	}
}

func TestSourceDurationRoundTrips(t *testing.T) {
	// A stored source is meant to be read by a person, so its cadence has to
	// survive a round trip as "24h" rather than as a count of nanoseconds.
	const definition = `{"name":"a","url":"https://example.org","every":"24h",
	  "schema":{"name":"s","fields":[{"name":"title","kind":"string","required":true}]}}`

	var source extract.Source
	if err := json.Unmarshal([]byte(definition), &source); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if source.Every.Every() != 24*time.Hour {
		t.Errorf("Every = %s, want 24h", source.Every)
	}

	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	// Go canonicalises a duration to its full form, so "24h" comes back as
	// "24h0m0s". Still a duration a person can read, and it parses back.
	if !strings.Contains(string(encoded), `"every":"24h0m0s"`) {
		t.Errorf("Marshal() = %s, want the cadence written as a readable string", encoded)
	}
}
