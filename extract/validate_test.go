package extract

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ruledSchema is the kind of schema an operator writes: required fields
// that make a record worth having, and rules that say what a plausible value
// looks like.
func ruledSchema() Schema {
	return Schema{
		Name:   "listings",
		Intent: "the events currently listed on this site",
		Fields: []Field{
			{
				Name: "title", Kind: KindString, Required: true,
				Description: "the entry's name",
				Rules: Rules{
					MinLength: 2, MaxLength: 200,
					Placeholders: []string{"Find out more", "Read more", "Book now", "More info"},
				},
			},
			{
				Name: "url", Kind: KindURL, Required: true,
				Description: "link to the entry's own page",
			},
			{
				Name: "opens", Kind: KindDate,
				Description: "opening date, ISO 8601",
				Rules:       Rules{Min: ptr(-3650.0), Max: ptr(3650.0)},
			},
		},
	}
}

func ptr[T any](v T) *T { return &v }

func ruledSource() Source {
	return Source{
		Name:   "example-source",
		URL:    "https://example.org/whats-on",
		Schema: ruledSchema(),
		Expect: Expectation{MinRecords: 1, Tolerance: 0.5},
	}
}

// goodRecords builds n plausible records.
func goodRecords(n int) []Record {
	records := make([]Record, 0, n)
	for i := range n {
		records = append(records, Record{
			"title": "Exhibition " + string(rune('A'+i%26)),
			"url":   "https://example.org/listings/" + string(rune('a'+i%26)),
			"opens": "2026-09-01",
		})
	}
	return records
}

func validateAt(t *testing.T, source Source, records []Record, history History) Assessment {
	t.Helper()
	v := &Validator{Now: func() time.Time { return time.Date(2026, time.August, 15, 0, 0, 0, 0, time.UTC) }}
	return v.Validate(context.Background(), source, records, history)
}

func TestValidatePasses(t *testing.T) {
	got := validateAt(t, ruledSource(), goodRecords(20), History{Counts: []int{19, 21, 20}, Complete: true})

	if got.Verdict != Pass {
		t.Errorf("Validate(20 good records) verdict = %s, want %s. Findings: %v",
			got.Verdict, Pass, got.Findings)
	}
	if len(got.Records) != 20 {
		t.Errorf("Validate() kept %d records, want 20", len(got.Records))
	}
}

// TestValidateEmptyIsAlwaysFail is the silent-failure case. An extraction that
// found nothing must never be publishable, whatever the history says.
func TestValidateEmptyIsAlwaysFail(t *testing.T) {
	got := validateAt(t, ruledSource(), nil, History{Counts: []int{20, 20, 20}, Complete: true})

	if got.Verdict != Fail {
		t.Errorf("Validate(no records) verdict = %s, want %s", got.Verdict, Fail)
	}
	if len(got.Findings) == 0 {
		t.Error("Validate(no records) reported no reason")
	}
}

// TestValidateVolumetricCollapse is the case the PRD singles out: every record
// is individually perfect, and the page has silently stopped listing all but a
// handful of them. Structural validity alone would call this a pass.
func TestValidateVolumetricCollapse(t *testing.T) {
	source := ruledSource()
	history := History{Counts: []int{200, 198, 205, 201}, Complete: true}

	got := validateAt(t, source, goodRecords(3), history)

	if got.Verdict != Suspect {
		t.Errorf("Validate(3 records against a trailing average of ~200) verdict = %s, want %s",
			got.Verdict, Suspect)
	}
	if got.Verdict.Publishable() {
		t.Error("a volumetric collapse was judged publishable")
	}

	finding := strings.Join(got.Findings, "; ")
	if !strings.Contains(finding, "outside the usual") {
		t.Errorf("Validate() findings = %q, want the count band explained", finding)
	}
}

// TestValidateNoHistoryCannotCollapse records the limit of the volumetric
// rung honestly: with no baseline there is nothing to compare against, and
// only the declared floor applies.
func TestValidateNoHistoryUsesFloorOnly(t *testing.T) {
	source := ruledSource()
	source.Expect.MinRecords = 5

	if got := validateAt(t, source, goodRecords(3), History{Complete: true}); got.Verdict != Fail {
		t.Errorf("Validate(3 records, floor 5) verdict = %s, want %s", got.Verdict, Fail)
	}
	if got := validateAt(t, source, goodRecords(6), History{Complete: true}); got.Verdict != Pass {
		t.Errorf("Validate(6 records, floor 5, no history) verdict = %s, want %s. Findings: %v",
			got.Verdict, Pass, got.Findings)
	}
}

func TestValidateStructural(t *testing.T) {
	tests := []struct {
		name    string
		records []Record
		want    Verdict
	}{
		{
			name: "a few malformed rows are tolerated",
			records: append(goodRecords(19), Record{
				"title": "Untitled", // no url
			}),
			want: Pass,
		},
		{
			name: "mostly malformed means the artifact is reading the wrong element",
			records: append(goodRecords(4), func() []Record {
				var bad []Record
				for range 16 {
					bad = append(bad, Record{"title": "Untitled"})
				}
				return bad
			}()...),
			want: Fail,
		},
		{
			name: "a relative URL is not a URL",
			records: []Record{
				{"title": "Bronze Age Britain", "url": "/listings/bronze-age"},
			},
			want: Fail,
		},
		{
			name: "a human-readable date is not an ISO date",
			records: []Record{
				{"title": "Bronze Age Britain", "url": "https://example.org/a", "opens": "15 January"},
			},
			want: Fail,
		},
		{
			name: "a number where text was declared",
			records: []Record{
				{"title": 42.0, "url": "https://example.org/a"},
			},
			want: Fail,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateAt(t, ruledSource(), tt.records, History{Counts: []int{20, 20, 20}, Complete: true})
			if got.Verdict != tt.want {
				t.Errorf("Validate(%s) verdict = %s, want %s. Findings: %v",
					tt.name, got.Verdict, tt.want, got.Findings)
			}
		})
	}
}

// TestValidateRejectsPlaceholders covers output that is complete, well-typed
// and entirely furniture — a selector aimed one element too high collecting
// button labels instead of titles.
func TestValidateRejectsPlaceholders(t *testing.T) {
	var records []Record
	for range 20 {
		records = append(records, Record{
			"title": "Find out more",
			"url":   "https://example.org/listings/a",
			"opens": "2026-09-01",
		})
	}

	got := validateAt(t, ruledSource(), records, History{Counts: []int{20, 20, 20}, Complete: true})

	if got.Verdict != Fail {
		t.Errorf("Validate(all placeholder titles) verdict = %s, want %s. Findings: %v",
			got.Verdict, Fail, got.Findings)
	}
	if !strings.Contains(strings.Join(got.Findings, "; "), "placeholder") {
		t.Errorf("Validate() findings = %v, want the placeholder named", got.Findings)
	}
}

func TestValidateDateRules(t *testing.T) {
	source := ruledSource()

	// An entry opening in the year 3000 is how some sites write "no end
	// date". As an opening date it is a defect, and the rule is relative to
	// today so it does not expire.
	var records []Record
	for range 10 {
		records = append(records, Record{
			"title": "Permanent Collection",
			"url":   "https://example.org/listings/permanent",
			"opens": "3000-01-01",
		})
	}

	got := validateAt(t, source, records, History{Counts: []int{10, 10, 10}, Complete: true})
	if got.Verdict == Pass {
		t.Errorf("Validate(opening in the year 3000) verdict = %s, want it flagged. Findings: %v",
			got.Verdict, got.Findings)
	}
}

// judgeFunc adapts a function to the Judge interface.
type judgeFunc func(ctx context.Context, intent string, sample []Record) (bool, string, error)

func (f judgeFunc) Plausible(ctx context.Context, intent string, sample []Record) (bool, string, error) {
	return f(ctx, intent, sample)
}

// TestValidateJudgeIsLastResort checks the cost guarantee: a settled source
// producing its usual clean output must not reach the model at all.
func TestValidateJudgeIsLastResort(t *testing.T) {
	var asked int
	judge := judgeFunc(func(context.Context, string, []Record) (bool, string, error) {
		asked++
		return true, "", nil
	})

	v := &Validator{Judge: judge, Now: time.Now}
	v.Validate(context.Background(), ruledSource(), goodRecords(20), History{Counts: []int{20, 20, 20}, Complete: true})

	if asked != 0 {
		t.Errorf("the judge was asked %d times about a clean run with settled history, want 0", asked)
	}

	// With no history there is no baseline, so the doubt is real and the model
	// is worth paying for.
	v.Validate(context.Background(), ruledSource(), goodRecords(20), History{Complete: true})
	if asked != 1 {
		t.Errorf("the judge was asked %d times about a run with no baseline, want 1", asked)
	}
}

func TestValidateJudgeFailureDoesNotFailTheRun(t *testing.T) {
	judge := judgeFunc(func(context.Context, string, []Record) (bool, string, error) {
		return false, "", errors.New("connection refused")
	})

	v := &Validator{Judge: judge, Now: time.Now}
	got := v.Validate(context.Background(), ruledSource(), goodRecords(20), History{Complete: true})

	// An unreachable inference server must not quarantine a healthy source.
	if got.Verdict != Pass {
		t.Errorf("Validate() with an unreachable judge verdict = %s, want %s. Findings: %v",
			got.Verdict, Pass, got.Findings)
	}
	if !strings.Contains(strings.Join(got.Findings, "; "), "could not be model-judged") {
		t.Errorf("Validate() findings = %v, want the judge failure noted", got.Findings)
	}
}

func TestValidateJudgeImplausible(t *testing.T) {
	judge := judgeFunc(func(context.Context, string, []Record) (bool, string, error) {
		return false, "these are gift shop items, not entries", nil
	})

	v := &Validator{Judge: judge, Now: time.Now}
	got := v.Validate(context.Background(), ruledSource(), goodRecords(20), History{Complete: true})

	if got.Verdict != Suspect {
		t.Errorf("Validate() with an implausible judgement verdict = %s, want %s", got.Verdict, Suspect)
	}
}

func TestHistoryAverage(t *testing.T) {
	if _, ok := (History{Counts: []int{10, 12}, Complete: true}).Average(); ok {
		t.Errorf("History.Average() with %d runs was trusted, want it withheld until %d",
			2, minHistory)
	}

	got, ok := (History{Counts: []int{10, 20, 30}, Complete: true}).Average()
	if !ok || got != 20 {
		t.Errorf("History.Average() = %v, %v, want 20, true", got, ok)
	}
}

func TestExpectationBand(t *testing.T) {
	tests := []struct {
		name      string
		expect    Expectation
		average   float64
		wantLow   int
		wantHigh  int
		wantCount int // a count that should sit inside the band
	}{
		{
			name:      "default tolerance",
			expect:    Expectation{},
			average:   200,
			wantLow:   100,
			wantHigh:  301,
			wantCount: 150,
		},
		{
			name:      "a tight source",
			expect:    Expectation{Tolerance: 0.1},
			average:   20,
			wantLow:   18,
			wantHigh:  23,
			wantCount: 20,
		},
		{
			name:      "the floor overrides a low band",
			expect:    Expectation{Tolerance: 0.9, MinRecords: 10},
			average:   20,
			wantLow:   10,
			wantHigh:  39,
			wantCount: 15,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			low, high := tt.expect.Band(tt.average)
			if low != tt.wantLow || high != tt.wantHigh {
				t.Errorf("Band(%v) = %d, %d, want %d, %d",
					tt.average, low, high, tt.wantLow, tt.wantHigh)
			}
			if tt.wantCount < low || tt.wantCount > high {
				t.Errorf("Band(%v) = %d–%d, which excludes the ordinary count %d",
					tt.average, low, high, tt.wantCount)
			}
		})
	}
}

func TestSchemaValidate(t *testing.T) {
	tests := []struct {
		name    string
		schema  Schema
		wantErr bool
	}{
		{name: "good", schema: ruledSchema()},
		{name: "no name", schema: Schema{Fields: []Field{{Name: "a", Kind: KindString, Required: true}}}, wantErr: true},
		{name: "no fields", schema: Schema{Name: "s"}, wantErr: true},
		{
			name:    "unknown kind",
			schema:  Schema{Name: "s", Fields: []Field{{Name: "a", Kind: "colour", Required: true}}},
			wantErr: true,
		},
		{
			name:    "duplicate field",
			schema:  Schema{Name: "s", Fields: []Field{{Name: "a", Kind: KindString, Required: true}, {Name: "a", Kind: KindString}}},
			wantErr: true,
		},
		{
			// Nothing required means nothing can ever fail structurally, which
			// is the silent failure this package exists to prevent.
			name:    "nothing required",
			schema:  Schema{Name: "s", Fields: []Field{{Name: "a", Kind: KindString}}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.schema.Validate()
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("Schema.Validate(%s) error = %v, want error presence = %t", tt.name, err, tt.wantErr)
			}
		})
	}
}

func TestSourceValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Source)
		wantErr bool
	}{
		{name: "good", mutate: func(*Source) {}},
		{name: "no name", mutate: func(s *Source) { s.Name = "" }, wantErr: true},
		{name: "slash in name", mutate: func(s *Source) { s.Name = "a/b" }, wantErr: true},
		{name: "space in name", mutate: func(s *Source) { s.Name = "a b" }, wantErr: true},
		{name: "no URL", mutate: func(s *Source) { s.URL = "" }, wantErr: true},
		{name: "not http", mutate: func(s *Source) { s.URL = "file:///etc/passwd" }, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := ruledSource()
			tt.mutate(&source)

			err := source.Validate()
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("Source.Validate(%s) error = %v, want error presence = %t", tt.name, err, tt.wantErr)
			}
		})
	}
}

// TestValidateWithheldWhenBaselineUnreadable covers the path that turned a
// storage hiccup into a silent failure.
//
// A history that could not be read is not a history that is empty. Treating
// them alike skipped the volumetric rung and let a 200-to-2 collapse — the
// exact thing that rung exists to catch — be graded Pass and published.
func TestValidateWithheldWhenBaselineUnreadable(t *testing.T) {
	source := ruledSource()

	// What the harvester passes when Store.History returned an error.
	unreadable := History{}

	got := validateAt(t, source, goodRecords(2), unreadable)

	// The verdict is deliberately not forced to Fail: doing so would authorise
	// a heal for every source on the schedule the moment object storage
	// hiccuped, at minutes of model time each, to fix nothing.
	if got.Verdict != Pass {
		t.Errorf("Validate(unreadable history) verdict = %s, want %s — a storage fault is not a bad extraction",
			got.Verdict, Pass)
	}
	if !got.BaselineMissing {
		t.Error("Validate() did not record that it graded without a baseline")
	}
	// But it must not be published, because the check that would have caught a
	// collapse did not run.
	if got.Publishable() {
		t.Error("output graded without the volumetric rung was judged publishable")
	}

	// A genuinely new source is a different thing and must publish normally.
	fresh := validateAt(t, source, goodRecords(2), History{Complete: true})
	if fresh.BaselineMissing {
		t.Error("a new source with no runs yet was reported as having an unreadable baseline")
	}
	if !fresh.Publishable() {
		t.Errorf("a new source's first extraction was withheld: %v", fresh.Findings)
	}
}

func TestExpectationBandNeverAdmitsNothing(t *testing.T) {
	// A tolerance of 1 or more puts the floor at zero, which admits an empty
	// extraction — the failure the rung exists to catch.
	low, _ := Expectation{Tolerance: 1.5}.Band(200)
	if low < 1 {
		t.Errorf("Band(200) with tolerance 1.5 = floor %d, want at least 1", low)
	}

	source := ruledSource()
	source.Expect.Tolerance = 1
	if err := source.Validate(); err == nil {
		t.Error("Source.Validate() accepted a tolerance of 1, which makes the volumetric rung vacuous")
	}
}

// TestValidateFieldNameWithColon covers a field name that used to be
// misattributed: the violation key was built as "field: problem" and split
// back on the first colon, so "date:raw" resolved to "date" and was graded
// against a different field's Required flag.
func TestValidateFieldNameWithColon(t *testing.T) {
	source := Source{
		Name:   "odd",
		URL:    "https://example.org/",
		Expect: Expectation{MinRecords: 1},
		Schema: Schema{
			Name: "odd",
			Fields: []Field{
				{Name: "title", Kind: KindString, Required: true},
				// Not required, and its violations must not be graded as if
				// they belonged to "date", which is.
				{Name: "date:raw", Kind: KindString, Rules: Rules{MinLength: 30}},
				{Name: "date", Kind: KindString, Required: true},
			},
		},
	}

	var records []Record
	for range 20 {
		records = append(records, Record{"title": "A Show", "date:raw": "short", "date": "2026-09-01"})
	}

	got := validateAt(t, source, records, History{Counts: []int{20, 20, 20}, Complete: true})

	// Every record violates a rule on an optional field. That is worth
	// reporting, but it is not a failure — which is what it became when the
	// name resolved to the required "date".
	if got.Verdict == Fail {
		t.Errorf("Validate() failed the run over an optional field's rule: %v", got.Findings)
	}
	if !strings.Contains(strings.Join(got.Findings, "; "), "date:raw") {
		t.Errorf("Validate() findings = %v, want the offending field named in full", got.Findings)
	}
}

func TestSchemaValidateRejectsBadPattern(t *testing.T) {
	// Reported once, at definition, rather than two hundred times per run.
	schema := ruledSchema()
	schema.Fields[0].Rules.Pattern = "([unclosed"

	if err := schema.Validate(); err == nil {
		t.Error("Schema.Validate() accepted an uncompilable pattern")
	}
}

// TestSourceValidateRefusesInternalAddresses covers the one field of a site
// record that gets dereferenced. Websites come from OpenStreetMap and Wikidata,
// both world-editable, and the deployment shares a host with sixteen other
// services.
func TestSourceValidateRefusesInternalAddresses(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "loopback", url: "http://127.0.0.1:8090/", wantErr: true},
		{name: "loopback by name", url: "http://localhost:9000/", wantErr: true},
		{name: "ipv6 loopback", url: "http://[::1]/", wantErr: true},
		{name: "private", url: "http://192.168.1.10/", wantErr: true},
		{name: "private class A", url: "http://10.0.0.5/", wantErr: true},
		{name: "cloud metadata", url: "http://169.254.169.254/latest/meta-data/", wantErr: true},
		{name: "ordinary site", url: "https://www.tate.org.uk/whats-on"},
		{name: "public IP", url: "http://93.184.216.34/"},
		{
			// The operator reaches their own Pi over Tailscale, so this range
			// must keep working or harvest add breaks on the deployment host.
			name: "tailscale", url: "http://100.116.81.88/map",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := ruledSource()
			source.URL = tt.url

			err := source.Validate()
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("Source.Validate(%s) error = %v, want error presence = %t", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestValidateDropsUndeclaredFields(t *testing.T) {
	records := []Record{{
		"title": "Bronze Age Britain",
		"url":   "https://example.org/a",
		// Nothing has checked these: no kind, no rules, no meaning downstream.
		"price":    "£18",
		"internal": "debug output",
	}}

	got := validateAt(t, ruledSource(), records, History{Complete: true})

	if len(got.Records) != 1 {
		t.Fatalf("Validate() kept %d records, want 1", len(got.Records))
	}
	for _, unwanted := range []string{"price", "internal"} {
		if _, present := got.Records[0][unwanted]; present {
			t.Errorf("Validate() published undeclared field %q unexamined", unwanted)
		}
	}
	if title, _ := got.Records[0].String("title"); title != "Bronze Age Britain" {
		t.Errorf("Validate() lost a declared field: %v", got.Records[0])
	}
}
