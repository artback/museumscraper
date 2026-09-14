package extract

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

// themedPage renders a page in a fixed "CMS theme" — the markup a template
// generates — with the given entries. Two sites on one theme differ in their
// content and in nothing else, which is the case reuse exists for.
func themedPage(theme string, entries ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><html><head><title>%s</title></head><body>
	  <div class="%s-wrapper"><main class="%s-main"><div class="%s-grid">`, theme, theme, theme, theme)
	for _, entry := range entries {
		fmt.Fprintf(&b, `<article class="%s-card"><header class="%s-card__head">
		    <h2 class="%s-card__title">%s</h2></header>
		    <a class="%s-card__link" href="/x/%s">Find out more</a>
		    <time class="%s-card__date" datetime="2026-09-01">1 September</time></article>`,
			theme, theme, theme, entry, theme, entry, theme)
	}
	b.WriteString(`</div></main></div></body></html>`)
	return b.String()
}

// The absolute numbers below are fixture numbers and must not be calibrated on:
// a dozen elements means the html/head/body skeleton every document shares is a
// fifth of an unrelated pair's score, where on real pages it is a thousandth.
// Measured across five real museum sites, every unrelated pair scored 0.00-0.02.
// What these tests hold is the shape of the answer — same theme near 1,
// different theme well under the gate, and the sketch tracking the exact
// calculation.
func TestShapeEstimatesSimilarity(t *testing.T) {
	// The same theme, different content and different amounts of it.
	first := ShapeOf(testPage(t, themedPage("acme", "one", "two", "three")))
	second := ShapeOf(testPage(t, themedPage("acme", "quite another show")))

	// A different theme entirely.
	other := ShapeOf(testPage(t, themedPage("zeta", "one", "two", "three")))

	sameTheme := first.Similarity(second)
	if sameTheme < 0.9 {
		t.Errorf("two pages on one theme scored %.2f, want near 1 — content must not move the score", sameTheme)
	}

	crossTheme := first.Similarity(other)
	if crossTheme >= ReuseThreshold {
		t.Errorf("two unrelated themes scored %.2f, which is above the reuse threshold %.2f",
			crossTheme, ReuseThreshold)
	}

	// Symmetric, or the answer depends on which site was met first.
	if math.Abs(first.Similarity(second)-second.Similarity(first)) > 1e-9 {
		t.Error("Similarity is not symmetric")
	}
}

// TestShapeEstimateTracksTheExactAnswer holds the sketch to the thing it is a
// sketch of. It is a sample, so it is allowed to be wrong — but not by much,
// and not in a way that could move a pair across the gate.
func TestShapeEstimateTracksTheExactAnswer(t *testing.T) {
	cases := []struct{ a, b string }{
		{themedPage("acme", "one", "two"), themedPage("acme", "three")},
		{themedPage("acme", "one"), themedPage("zeta", "one")},
		{themedPage("acme", "one", "two", "three"), themedPage("acme", "one", "two", "three")},
	}

	for _, c := range cases {
		left, right := testPage(t, c.a), testPage(t, c.b)

		exact := Similarity(left, right)
		estimate := ShapeOf(left).Similarity(ShapeOf(right))

		if math.Abs(exact-estimate) > 0.15 {
			t.Errorf("sketch estimated %.2f where the exact Jaccard is %.2f", estimate, exact)
		}
	}
}

// TestShapeSurvivesStorage: the sketch is only useful because it can be kept on
// an artifact and compared against a page met months later, with the page it
// came from long gone.
func TestShapeSurvivesStorage(t *testing.T) {
	page := testPage(t, themedPage("acme", "one", "two"))

	encoded, err := json.Marshal(Artifact{Source: "a", Version: 1, Script: "x", Shape: ShapeOf(page)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Artifact
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := decoded.Shape.Similarity(ShapeOf(page)); got != 1 {
		t.Errorf("a stored shape scored %.2f against the page it came from, want 1", got)
	}

	// An artifact stored before shapes existed carries none, and must simply
	// never be a candidate rather than scoring as similar to everything.
	var old Artifact
	if err := json.Unmarshal([]byte(`{"source":"old","version":1,"script":"x"}`), &old); err != nil {
		t.Fatalf("unmarshal old artifact: %v", err)
	}
	if !old.Shape.Empty() {
		t.Error("an artifact with no stored shape did not report an empty one")
	}
	if got := ShapeOf(page).Similarity(old.Shape); got != 0 {
		t.Errorf("an empty shape scored %.2f, want 0", got)
	}
}

func TestMostSimilarRanksAndRefuses(t *testing.T) {
	page := testPage(t, themedPage("acme", "one", "two"))

	sibling := Artifact{Source: "sibling", Shape: ShapeOf(testPage(t, themedPage("acme", "three")))}
	stranger := Artifact{Source: "stranger", Shape: ShapeOf(testPage(t, themedPage("zeta", "three")))}
	unknown := Artifact{Source: "unknown"}

	matches := MostSimilar(ShapeOf(page), []Artifact{stranger, unknown, sibling}, ReuseThreshold)

	if len(matches) != 1 {
		t.Fatalf("MostSimilar returned %d matches, want only the sibling: %+v", len(matches), matches)
	}
	if matches[0].Artifact.Source != "sibling" {
		t.Errorf("matched %q, want the site built from the same theme", matches[0].Artifact.Source)
	}
	if matches[0].Similarity < ReuseThreshold {
		t.Errorf("match similarity %.2f is below the threshold it was filtered on", matches[0].Similarity)
	}

	// A page with no structure to compare cannot reuse anything.
	if got := MostSimilar(Shape{}, []Artifact{sibling}, ReuseThreshold); got != nil {
		t.Errorf("MostSimilar with no shape returned %+v, want nothing", got)
	}
}
