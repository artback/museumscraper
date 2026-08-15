package extract

import (
	"fmt"
	"strings"
	"testing"
)

// bigListing is the shape that makes reduction worth doing: a page whose
// interesting structure is three lines long and whose bulk is the same row
// repeated, wrapped in the scripts and styling a real site carries.
func bigListing(rows int) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><title>What's On</title>`)
	b.WriteString(`<style>` + strings.Repeat(".exhibition{color:red}", 200) + `</style>`)
	b.WriteString(`<script>var analytics=` + strings.Repeat(`"padding",`, 500) + `1;</script>`)
	b.WriteString(`</head><body><main><ul class="exhibitions">`)
	for i := range rows {
		fmt.Fprintf(&b, `<li class="exhibition" data-id="%d">`+
			`<h3 class="title">Exhibition %d</h3>`+
			`<a href="/exhibitions/%d?utm_source=nav&utm_campaign=whats-on">Find out more</a>`+
			`<time datetime="2026-09-%02d">1 September</time>`+
			`</li>`, i, i, i, i%28+1)
	}
	b.WriteString(`</ul></main></body></html>`)
	return b.String()
}

func TestReduceCollapsesRepeatedRows(t *testing.T) {
	page := testPage(t, bigListing(200))
	got := NewReducer().Reduce(page)

	// Three rows kept, the rest summarised. The count matters: it tells the
	// model the page is a long list rather than a three-item one.
	if strings.Count(got.Text, `<h3 class="title">`) != DefaultMaxRepeats {
		t.Errorf("Reduce() kept %d rows, want %d:\n%s",
			strings.Count(got.Text, `<h3 class="title">`), DefaultMaxRepeats, got.Text)
	}
	if !strings.Contains(got.Text, "197 more li.exhibition") {
		t.Errorf("Reduce() did not summarise the collapsed rows:\n%s", got.Text)
	}

	if strings.Contains(got.Text, "analytics") {
		t.Error("Reduce() kept an inline script")
	}
	if strings.Contains(got.Text, "color:red") {
		t.Error("Reduce() kept a stylesheet")
	}

	// Tracking parameters say nothing about what a link is for.
	if strings.Contains(got.Text, "utm_source") {
		t.Errorf("Reduce() kept query-string tracking:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, `href="/exhibitions/0?…"`) {
		t.Errorf("Reduce() lost the link path:\n%s", got.Text)
	}

	if got.Ratio() < 10 {
		t.Errorf("Reduce() ratio = %.1f, want a large page to reduce at least tenfold (%s)",
			got.Ratio(), got)
	}
}

// TestReduceKeepsJSONLD covers the one kind of script worth keeping: a site
// that declares schema.org data is handing over exact values, and an artifact
// that reads them beats one guessing at markup.
func TestReduceKeepsJSONLD(t *testing.T) {
	const page = `<html><head>
	<script>var tracking = 1;</script>
	<script type="application/ld+json">
	{"@type":"ExhibitionEvent","name":"Silk Roads","startDate":"2026-10-12"}
	</script></head><body><p>Hello</p></body></html>`

	got := NewReducer().Reduce(testPage(t, page))

	if !strings.Contains(got.Text, "ExhibitionEvent") {
		t.Errorf("Reduce() dropped a JSON-LD declaration:\n%s", got.Text)
	}
	if strings.Contains(got.Text, "var tracking") {
		t.Errorf("Reduce() kept a plain script:\n%s", got.Text)
	}
}

func TestReduceTruncatesLongText(t *testing.T) {
	page := testPage(t, `<html><body><p>`+strings.Repeat("essay ", 400)+`</p></body></html>`)
	got := NewReducer().Reduce(page)

	if !strings.Contains(got.Text, "…") {
		t.Errorf("Reduce() did not truncate a long text node:\n%s", got.Text)
	}
	if len([]rune(got.Text)) > DefaultMaxTextRunes+200 {
		t.Errorf("Reduce() produced %d runes for one long paragraph, want it truncated",
			len([]rune(got.Text)))
	}
}

func TestReduceRespectsByteCap(t *testing.T) {
	reducer := NewReducer()
	reducer.MaxBytes = 500
	reducer.MaxRepeats = 1000 // force the byte cap to be the binding limit

	got := reducer.Reduce(testPage(t, bigListing(500)))

	if !got.Truncated {
		t.Error("Reduce() did not report truncation despite hitting the byte cap")
	}
	if got.ReducedBytes > 2*reducer.MaxBytes {
		t.Errorf("Reduce() produced %d bytes, want it near the cap of %d",
			got.ReducedBytes, reducer.MaxBytes)
	}
}

func TestFingerprintIgnoresContent(t *testing.T) {
	// The same page with three entries and with two hundred. A fingerprint
	// that changed here would report drift every time a museum's programme
	// turned over, which is the commonest thing that happens to these pages
	// and the one thing that is not a layout change.
	few := Fingerprint(testPage(t, bigListing(3)))
	many := Fingerprint(testPage(t, bigListing(200)))

	if few != many {
		t.Errorf("Fingerprint() differs between 3 rows (%s) and 200 rows (%s); it must not",
			few[:12], many[:12])
	}
	if few == "" {
		t.Error("Fingerprint() returned an empty signature")
	}
}

func TestFingerprintDetectsLayoutChange(t *testing.T) {
	before := Fingerprint(testPage(t, bigListing(20)))

	// The site rebuilds its listing with different markup: the change that
	// actually breaks an artifact.
	redesigned := strings.NewReplacer(
		`ul class="exhibitions"`, `div class="programme-grid"`,
		`li class="exhibition"`, `article class="card"`,
		`h3 class="title"`, `h2 class="card__heading"`,
	).Replace(bigListing(20))

	after := Fingerprint(testPage(t, redesigned))
	if !Drifted(before, after) {
		t.Error("Fingerprint() did not change when the listing markup was rewritten")
	}
}

// TestFingerprintIgnoresGeneratedClasses covers the false-drift case: a site
// rebuilt with no layout change at all, but new content hashes in every class
// name.
func TestFingerprintIgnoresGeneratedClasses(t *testing.T) {
	const build1 = `<html><body><div class="layout sc-bdVaJaXY">
	  <ul class="list styles__row___3xY7a"><li class="item is-active">A</li>
	  <li class="item">B</li></ul></div></body></html>`

	const build2 = `<html><body><div class="layout sc-KpTuwWzz">
	  <ul class="list styles__row___9qQ2b"><li class="item">A</li>
	  <li class="item is-active">B</li></ul></div></body></html>`

	before := Fingerprint(testPage(t, build1))
	after := Fingerprint(testPage(t, build2))

	if Drifted(before, after) {
		t.Errorf("Fingerprint() reported drift for a rebuild that only changed generated class names")
	}
}

func TestDriftedWithMissingFingerprint(t *testing.T) {
	// An artifact stored before fingerprinting existed has none. Reporting
	// drift for it would manufacture a reason to spend a model invocation.
	if Drifted("", "abc") || Drifted("abc", "") {
		t.Error("Drifted() reported drift against an unknown fingerprint")
	}
}

// TestSimilarity covers the gate that decides whether one site's extractor may
// be tried on another.
//
// Validation alone cannot be that gate. An extractor generated for Textilmuseet
// was measured extracting four records from Arbetets museum and grading pass,
// where that museum's own extractor found twenty-four — a first run has no
// volumetric baseline, so nothing in the ladder sees the missing eighty-three
// per cent. Structural similarity is what separates "the same page shape, so
// the same selectors mean the same thing" from "different page, matched by
// accident": the two live pages above score 0.00.
func TestSimilarity(t *testing.T) {
	// The same site with different content: same shape, so a high score.
	few := testPage(t, bigListing(3))
	many := testPage(t, bigListing(200))

	if got := Similarity(few, many); got < 0.9 {
		t.Errorf("Similarity(same site, different content) = %.2f, want near 1", got)
	}

	// A different site: different theme, different structure throughout. Sized
	// realistically on purpose — on a page of only a few elements the shared
	// html/body skeleton is most of the paths and every pair scores high, which
	// is why this measure is only meaningful on real pages. The five live
	// museum sites score 0.00 to 0.02 against each other.
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><title>Programme</title></head>`)
	b.WriteString(`<body><div class="wrap"><section class="programme-grid">`)
	for i := range 200 {
		fmt.Fprintf(&b, `<article class="card"><header class="card__head">`+
			`<h2 class="card__heading">Show %d</h2></header>`+
			`<a class="card__link" href="/programme/%d">more</a>`+
			`<span class="card__dates">1 Sept</span></article>`, i, i)
	}
	b.WriteString(`</section></div></body></html>`)
	other := testPage(t, b.String())

	// The property that matters is discrimination, not an absolute value: the
	// same site must score far above a different one. These fixtures have only
	// a dozen distinct paths each, so their shared skeleton keeps the floor
	// well above the 0.00–0.02 real sites score.
	same, different := Similarity(few, many), Similarity(few, other)
	if different > 0.5 {
		t.Errorf("Similarity(unrelated sites) = %.2f, want well below the same-site score", different)
	}
	if same <= 2*different {
		t.Errorf("Similarity does not discriminate: same site %.2f, different site %.2f", same, different)
	}

	if got := Similarity(nil, few); got != 0 {
		t.Errorf("Similarity(nil, page) = %.2f, want 0", got)
	}
	if got := Similarity(few, few); got != 1 {
		t.Errorf("Similarity(page, itself) = %.2f, want 1", got)
	}
}
