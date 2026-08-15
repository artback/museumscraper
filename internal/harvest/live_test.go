package harvest

import (
	"context"
	"encoding/json"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/artback/museumscraper/extract"
	"museum/pkg/exhibitions"
	"museum/pkg/model"
)

// This file compiles real extractors for real museum websites, with a real
// model, and reports what came back. It is excluded by -short like the other
// live tests in this repository, and unlike them it costs model invocations,
// so it also needs -harvest.live to run at all.
//
// The museums are deliberately small and obscure. A national gallery publishes
// schema.org data and hires people to keep its markup tidy; the sites this has
// to work on are run by volunteers, and they are where a hand-written scraper
// gives up.

var live = flag.Bool("harvest.live", false,
	"compile extractors against real museum websites using a real model")

// liveMuseums are the targets. Each is a small institution whose programme
// page is public and whose markup nobody has designed for scraping.
var liveMuseums = []struct {
	name string
	url  string
	note string
}{
	{
		name: "radiomuseet",
		url:  "https://www.radiomuseet.se/",
		note: "Göteborg volunteer radio museum — named in the README as the case that defeats date-based extraction",
	},
	{
		name: "kalmar-lansmuseum",
		url:  "https://www.kalmarlansmuseum.se/utstallningar/",
		note: "Kalmar county museum, the deployment's own region",
	},
	{
		name: "postmuseum",
		url:  "https://www.postmuseum.se/utstallningar/",
		note: "small Stockholm postal museum",
	},
	{
		name: "textilmuseet",
		url:  "https://textilmuseet.se/utstallningar/",
		note: "Borås textile museum",
	},
	{
		name: "bohuslansmuseum",
		url:  "https://www.bohuslansmuseum.se/utstallningar/",
		note: "Uddevalla regional museum",
	},
	{
		name: "arbetetsmuseum",
		url:  "https://arbetetsmuseum.se/utstallningar/",
		note: "Norrköping museum of work",
	},
}

func liveHarvester(t *testing.T) (*Harvester, *model.ClaudeCode) {
	t.Helper()

	client, err := model.NewClaudeCode()
	if err != nil {
		t.Skipf("no model available: %v", err)
	}

	return &Harvester{
		// No store and no sink: this compiles and trials, it does not persist.
		// The point is to find out whether an extractor can be written for
		// these pages at all.
		Fetch:     exhibitions.NewFetcher(),
		Generator: &extract.Generator{Model: client},
	}, client
}

// TestLiveCompileRealMuseums is the end-to-end evidence: fetch a real page,
// reduce it, have a model write an extractor, run that extractor in the
// sandbox, and grade what it produced.
func TestLiveCompileRealMuseums(t *testing.T) {
	if testing.Short() || !*live {
		t.Skip("live model test; run with -harvest.live and without -short")
	}

	harvester, client := liveHarvester(t)
	source := func(name, url string) extract.Source {
		s := extract.Source{
			Name:   name,
			URL:    url,
			Schema: ExhibitionSchema(),
			Expect: extract.Expectation{MinRecords: 1, Tolerance: 0.75},
		}
		return s
	}

	for _, museum := range liveMuseums {
		t.Run(museum.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			started := time.Now()
			artifact, report, err := harvester.Draft(ctx, source(museum.name, museum.url))

			t.Logf("%s — %s", museum.name, museum.note)
			t.Logf("  page reduced: %s", report.Reduction)
			for _, attempt := range report.Attempts {
				if attempt.Problem == "" {
					t.Logf("  attempt %d: accepted, %d records", attempt.Number, attempt.Records)
					continue
				}
				t.Logf("  attempt %d: %s", attempt.Number, attempt.Problem)
				for _, finding := range attempt.Findings {
					t.Logf("      %s", finding)
				}
			}

			if err != nil {
				// A site that cannot be compiled is a result, not a test
				// failure: the honest claim for this harness is that it works
				// on some pages a hand-written scraper cannot read, not on all
				// of them.
				t.Logf("  RESULT: no working extractor after %s (%v)",
					time.Since(started).Round(time.Second), err)
				return
			}

			t.Logf("  RESULT: compiled in %s using %s",
				time.Since(started).Round(time.Second), client.Name())
			t.Logf("  script:\n%s", indentLines(artifact.Script))

			// Re-run the stored artifact to show the steady-state path: the
			// same page, no model, deterministic output.
			page, err := refetch(ctx, harvester, museum.url)
			if err != nil {
				t.Fatalf("refetch: %v", err)
			}

			// Through the harvester's own sandbox, not a bare one: a stored
			// artifact may use the standard library, and running it without
			// gives "museum is not defined". This test made that mistake and
			// it is worth keeping the comment — anything that executes a
			// stored artifact must configure the sandbox the same way the
			// generator did.
			out, err := harvester.sandbox().Run(ctx, artifact.Script, page)
			if err != nil {
				t.Fatalf("stored artifact failed on re-run: %v", err)
			}

			assessment := (&extract.Validator{}).Validate(ctx,
				source(museum.name, museum.url), out.Records,
				extract.History{Complete: true})

			t.Logf("  re-run with no model: %d records in %s, graded %s",
				len(out.Records), out.Duration.Round(time.Millisecond), assessment.Verdict)
			for _, finding := range assessment.Findings {
				t.Logf("      %s", finding)
			}
			for i, record := range assessment.Records {
				if i >= 8 {
					t.Logf("      … and %d more", len(assessment.Records)-i)
					break
				}
				encoded, _ := json.Marshal(record)
				t.Logf("      %s", encoded)
			}
		})
	}
}

// refetch reads a page again through the harvester's own fetcher.
func refetch(ctx context.Context, h *Harvester, url string) (*extract.Page, error) {
	body, final, err := h.Fetch.Get(ctx, url)
	if err != nil {
		return nil, err
	}
	if final == "" {
		final = url
	}
	return extract.ParsePage(final, body)
}

func indentLines(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "    " + line
	}
	return strings.Join(lines, "\n")
}

// TestLiveDeterminism checks the claim the whole design rests on: once an
// artifact exists, running it twice against the same page produces byte-identical
// output and costs nothing.
func TestLiveDeterminism(t *testing.T) {
	if testing.Short() || !*live {
		t.Skip("live model test; run with -harvest.live and without -short")
	}

	harvester, _ := liveHarvester(t)
	target := liveMuseums[0]

	source := extract.Source{
		Name:   target.name,
		URL:    target.url,
		Schema: ExhibitionSchema(),
		Expect: extract.Expectation{MinRecords: 1, Tolerance: 0.75},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	artifact, _, err := harvester.Draft(ctx, source)
	if err != nil {
		t.Skipf("could not compile an extractor for %s: %v", target.name, err)
	}

	page, err := refetch(ctx, harvester, target.url)
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}

	var previous string
	for run := range 3 {
		out, err := harvester.sandbox().Run(ctx, artifact.Script, page)
		if err != nil {
			t.Fatalf("run %d: %v", run+1, err)
		}
		encoded, err := json.Marshal(out.Records)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}

		t.Logf("run %d: %d records in %s", run+1, len(out.Records), out.Duration.Round(time.Microsecond))
		if run > 0 && string(encoded) != previous {
			t.Errorf("run %d produced different output from run %d; extraction must be deterministic",
				run+1, run)
		}
		previous = string(encoded)
	}
}

// TestLiveJudgeOnRealOutput is an empirical test of the last rung of the
// validation ladder, against real output from the live runs above.
//
// What it established, and the reason it is written the way it is: the judge
// does NOT reject Radiomuseet's output, and on inspection it is right not to.
// Asked whether 35 records plausibly answer "the exhibitions and displays
// currently on show", a set that is mostly genuine display topics at a radio
// museum does answer it — even though two entries ("Bibliotek", the library;
// "Västkustens DX-klubb", a club that meets there) are facilities rather than
// displays. It says so in its reason, and still answers true.
//
// That is a set-level question giving a set-level answer, and it is the honest
// limit of this rung: it catches a wholesale wrong extraction, not a few wrong
// records inside a right one. Nothing else in the ladder catches those either
// — the semantic rung only applies rules the operator declared, and the
// catalogue's own PruneNavigationListings skips permanent rows, which these
// are. See the README.
//
// So the assertion here is the one that matters operationally: the rung must
// not raise a false alarm on good output, because a false alarm withholds
// correct data. Its behaviour on partially-navigational output is logged
// rather than asserted, because it is a judgement call by a model and pinning
// it to an exact answer would make this test a coin toss.
func TestLiveJudgeOnRealOutput(t *testing.T) {
	if testing.Short() || !*live {
		t.Skip("live model test; run with -harvest.live and without -short")
	}

	client, err := model.NewClaudeCode()
	if err != nil {
		t.Skipf("no model available: %v", err)
	}
	judge := extract.NewJudge(client)
	intent := ExhibitionSchema().Intent

	cases := []struct {
		name    string
		records []extract.Record
		// assert is false for the case whose right answer is genuinely
		// arguable; that run only reports what the judge said.
		assert bool
		want   bool
	}{
		{
			// Verbatim from the live run against radiomuseet.se.
			name: "part navigation, part genuine permanent displays",
			records: []extract.Record{
				{"title": "Amatörradio", "url": "https://radiomuseet.se/amatorradio-2/", "permanent": true},
				{"title": "Bibliotek", "url": "https://radiomuseet.se/bibliotek/", "permanent": true},
				{"title": "Bilradio", "url": "https://radiomuseet.se/bilradio/", "permanent": true},
				{"title": "DX-ing", "url": "https://radiomuseet.se/dx-ing/", "permanent": true},
				{"title": "Västkustens DX-klubb", "url": "https://radiomuseet.se/vastkustens-dx-klubb/", "permanent": true},
			},
		},
		{
			// Verbatim from the live run against arbetetsmuseum.se. A false
			// alarm here would withhold correct data, which is the expensive
			// mistake this rung must not make.
			name: "genuine exhibitions",
			records: []extract.Record{
				{"title": "Vår verklighet – Genom våra ögon, bortom ord", "url": "https://www.arbetetsmuseum.se/utstallning/var-verklighet/", "start": "2026-05-30", "end": "2026-09-13"},
				{"title": "Unsheltered Island", "url": "https://www.arbetetsmuseum.se/utstallning/unsheltered-island/", "start": "2026-02-07", "end": "2026-09-06"},
				{"title": "Dokfotosalong 2026: Händer", "url": "https://www.arbetetsmuseum.se/utstallning/dokfotosalong-2026-hander/", "start": "2026-09-26", "end": "2027-01-17"},
				{"title": "Alva – arbetarminnen från Strykjärnet", "url": "https://www.arbetetsmuseum.se/utstallning/alva/", "permanent": true},
				{"title": "Jobbcirkus", "url": "https://www.arbetetsmuseum.se/utstallning/jobbcirkus/", "permanent": true},
			},
			assert: true,
			want:   true,
		},
		{
			// Wholesale wrong: the shop, not the programme. This is the shape
			// the rung exists to catch, and it must.
			name: "wholesale wrong output",
			records: []extract.Record{
				{"title": "Tygkasse med tryck", "url": "https://example-museum.se/butik/tygkasse/"},
				{"title": "Kaffemugg", "url": "https://example-museum.se/butik/kaffemugg/"},
				{"title": "Vykort 10-pack", "url": "https://example-museum.se/butik/vykort/"},
				{"title": "Presentkort 500 kr", "url": "https://example-museum.se/butik/presentkort/"},
				{"title": "Medlemskap 2026", "url": "https://example-museum.se/bli-medlem/"},
			},
			assert: true,
			want:   false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			ok, reason, err := judge.Plausible(ctx, intent, tt.records)
			if err != nil {
				t.Fatalf("Plausible() error = %v", err)
			}

			t.Logf("plausible=%t  reason=%q", ok, reason)
			if tt.assert && ok != tt.want {
				t.Errorf("Plausible(%s) = %t, want %t — reason: %s", tt.name, ok, tt.want, reason)
			}
		})
	}
}
