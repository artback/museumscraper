package command

import (
	"slices"
	"testing"
)

// TestParseSourcesAll: the widest crawl is the one most likely to be wanted and
// was the one hardest to ask for. A source added later also has to reach
// everyone who wrote the full list out by hand, and this is what makes that
// possible.
func TestParseSourcesAll(t *testing.T) {
	for _, raw := range []string{"all", "ALL", " all "} {
		got := parseSources(raw)
		if !slices.Equal(got, allSources) {
			t.Errorf("parseSources(%q) = %v, want %v", raw, got, allSources)
		}
	}
}

func TestParseSourcesStillValidatesNames(t *testing.T) {
	got := parseSources("wikidata,not-a-source,osm,wikidata")

	want := []string{"wikidata", "osm"}
	if !slices.Equal(got, want) {
		t.Errorf("parseSources = %v, want %v (unknown dropped, duplicate collapsed)", got, want)
	}
	if len(parseSources("nothing-real")) != 0 {
		t.Error("a flag naming no real source should select none, so the caller can report it")
	}
}

// TestAllSourcesAreKnown guards the two lists against drifting apart: a source
// in allSources that parseSources rejects would make "all" select less than
// everything, silently.
func TestAllSourcesAreKnown(t *testing.T) {
	for _, name := range allSources {
		if got := parseSources(name); len(got) != 1 || got[0] != name {
			t.Errorf("parseSources(%q) = %v, want it accepted", name, got)
		}
	}
}
