package exhibitions

import (
	"context"
	"testing"
	"time"

	"museum/internal/models"
)

// TestProvenance_EachRungStampsItself walks the ladder in readSite once per
// rung and checks that the entry says which one produced it.
//
// The rungs are the point of the field: they cost wildly different things —
// nothing, nothing, a model invocation, and two extra fetches — and without a
// stamp on the way out the results are indistinguishable afterwards.
func TestProvenance_EachRungStampsItself(t *testing.T) {
	if testing.Short() {
		t.Skip("starts HTTP servers and waits out the per-host rate limit")
	}

	tests := []struct {
		name     string
		pages    map[string]string
		fallback Fallback
		want     Provenance
	}{
		{
			// The museum published schema.org event data, so nothing had to be
			// guessed at.
			name: "declared",
			pages: map[string]string{
				"/": `<html><body><a href="/exhibitions">Exhibitions</a></body></html>`,
				"/exhibitions": `<html><body>
				  <script type="application/ld+json">
				  {"@context":"https://schema.org","@type":"ExhibitionEvent",
				   "name":"Bronze Age Britain","url":"/exhibition/bronze-age",
				   "startDate":"2026-09-01","endDate":"2027-01-15"}
				  </script>
				</body></html>`,
			},
			want: ProvenanceDeclared,
		},
		{
			// The same exhibition, published as ordinary markup: a link on an
			// exhibition-shaped path with dates beside it.
			name: "heuristic",
			pages: map[string]string{
				"/": `<html><body><a href="/exhibitions">Exhibitions</a></body></html>`,
				"/exhibitions": `<html><body>
				  <a href="/exhibition/bronze-age">Bronze Age Britain</a>
				  <time datetime="2026-09-01">1 September</time>
				  <time datetime="2027-01-15">15 January</time>
				</body></html>`,
			},
			want: ProvenanceHeuristic,
		},
		{
			// Nothing the heuristics can read, and a fallback that recovers it.
			name:  "generated",
			pages: map[string]string{"/": `<html><body><div id="app"></div></body></html>`},
			fallback: &countingFallback{found: []Exhibition{
				{Title: "Recovered By The Harness", URL: "/exhibition/one"},
			}},
			want: ProvenanceGenerated,
		},
		{
			// No programme at all, but a page describing what is on show.
			name: "description",
			pages: map[string]string{
				"/": `<html><body><a href="/museiinformation/">Museiinformation</a></body></html>`,
				"/museiinformation/": `<html><head><title>Museiinformation</title></head><body>
				  <h1>Museiinformation</h1>
				  <p>Njut av radiohistoria! Här visas teknikhistorien bakom dagens
				  ingenjörskonst. Här finns kristallmottagare, amatörradio,
				  militärradio och ett bibliotek med handböcker.</p>
				</body></html>`,
			},
			want: ProvenanceDescription,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			site := siteServing(t, tt.pages)
			scraper := NewScraper()
			scraper.Fallback = tt.fallback

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			found, err := scraper.ForMuseum(ctx, models.Museum{Name: "Test Museum", Website: site.URL})
			if err != nil {
				t.Fatalf("ForMuseum: %v", err)
			}
			if len(found) == 0 {
				t.Fatalf("the %s rung produced nothing to stamp", tt.name)
			}
			for _, e := range found {
				if e.Provenance != tt.want {
					t.Errorf("Provenance = %q for %q, want %q", e.Provenance, e.Title, tt.want)
				}
			}
		})
	}
}

// TestProvenance_GeneratedIsStampedByTheScraper checks the stamp is applied
// where the ladder knows the answer rather than taken on trust from the
// Fallback, which is what stops a wired-in implementation — present or future
// — from under-reporting its own share.
func TestProvenance_GeneratedIsStampedByTheScraper(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an HTTP server and waits out the per-host rate limit")
	}

	site := siteServing(t, map[string]string{"/": `<html><body><div id="app"></div></body></html>`})

	scraper := NewScraper()
	scraper.Fallback = &countingFallback{found: []Exhibition{{
		Title: "Claims To Be Declared",
		URL:   site.URL + "/exhibition/one",
		// A fallback reporting the cheapest rung for its own output.
		Provenance: ProvenanceDeclared,
	}}}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	found, err := scraper.ForMuseum(ctx, models.Museum{Name: "Opaque", Website: site.URL})
	if err != nil {
		t.Fatalf("ForMuseum: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("got %d entries, want the one the fallback returned: %+v", len(found), found)
	}
	if got := found[0].Provenance; got != ProvenanceGenerated {
		t.Errorf("Provenance = %q, want %q: the fallback's own claim was believed", got, ProvenanceGenerated)
	}
}

// TestProvenance_MergingPrefersWhatWasDeclared covers dedupe, where one
// exhibition read twice must not be counted under two rungs.
func TestProvenance_MergingPrefersWhatWasDeclared(t *testing.T) {
	march := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		first Provenance
		// second is the same exhibition, read again from another page.
		second Provenance
		want   Provenance
	}{
		{"declared then heuristic", ProvenanceDeclared, ProvenanceHeuristic, ProvenanceDeclared},
		{"heuristic then declared", ProvenanceHeuristic, ProvenanceDeclared, ProvenanceDeclared},
		// The rung whose share is being measured never inherits work another
		// rung did, in either direction.
		{"heuristic then generated", ProvenanceHeuristic, ProvenanceGenerated, ProvenanceHeuristic},
		{"generated then heuristic", ProvenanceGenerated, ProvenanceHeuristic, ProvenanceGenerated},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged := dedupe([]Exhibition{
				{Title: "Bronze Age Britain", URL: "/a", End: &march, Provenance: tt.first},
				{Title: "Bronze Age Britain", URL: "/b", End: &march, Provenance: tt.second},
			})
			if len(merged) != 1 {
				t.Fatalf("got %d entries, want the two folded into one: %+v", len(merged), merged)
			}
			if got := merged[0].Provenance; got != tt.want {
				t.Errorf("Provenance = %q, want %q", got, tt.want)
			}
		})
	}
}
