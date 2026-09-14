package harvest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/artback/museumscraper/extract"
)

// A second site on the same CMS theme as beforePage: identical markup,
// different content. This is what a corpus of museum websites is actually full
// of — one template sold to eighty institutions.
const siblingPage = `<html><body><ul class="exhibitions">
  <li class="exhibition"><h3>Textiles of the North</h3>
    <a href="/exhibitions/textiles">more</a></li>
  <li class="exhibition"><h3>Herring and the Sea</h3>
    <a href="/exhibitions/herring">more</a></li>
</ul></body></html>`

// storedSibling is a source with a working extractor, as the store would hold
// it after that site was compiled.
func storedSibling(t *testing.T, store *memory, name, page, script string) {
	t.Helper()

	parsed, err := extract.ParsePage("https://sibling.example/whats-on", page)
	if err != nil {
		t.Fatalf("parse sibling page: %v", err)
	}

	source := testSource()
	source.Name, source.URL = name, "https://sibling.example/whats-on"
	store.mu.Lock()
	store.sources[name] = source
	store.mu.Unlock()
	if err := store.SaveArtifact(context.Background(), extract.Artifact{
		Source:      name,
		Version:     1,
		Script:      script,
		Fingerprint: extract.Fingerprint(parsed),
		Shape:       extract.ShapeOf(parsed),
		Provenance:  extract.Provenance{Model: "qwen2.5-coder:7b", Prompt: extract.PromptVersion},
	}); err != nil {
		t.Fatalf("save sibling artifact: %v", err)
	}
}

func reuseHarvester(store *memory, model *fixedModel, body string) *Harvester {
	return &Harvester{
		Store:     store,
		Fetch:     &pageFetcher{body: body},
		Generator: &extract.Generator{Model: model},
		Now:       func() time.Time { return time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC) },
	}
}

// TestCompileReusesAnExtractorFromAStructurallyIdenticalSite is the saving. An
// extractor written against one site built from a template is written against
// every site built from that template, and generating a second one is the model
// being paid to learn the same page twice.
func TestCompileReusesAnExtractorFromAStructurallyIdenticalSite(t *testing.T) {
	store := newMemory()
	storedSibling(t, store, "sibling-museum", siblingPage, beforeScript)

	model := &fixedModel{script: beforeScript}
	harvester := reuseHarvester(store, model, beforePage)

	source := testSource()
	artifact, report, err := harvester.Compile(context.Background(), source)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	if model.calls.Load() != 0 {
		t.Errorf("the model was invoked %d times for a page an existing extractor already reads",
			model.calls.Load())
	}
	if artifact.Provenance.ReusedFrom != "sibling-museum" {
		t.Errorf("Provenance.ReusedFrom = %q, want the source it was taken from", artifact.Provenance.ReusedFrom)
	}
	if artifact.Provenance.Similarity < extract.ReuseThreshold {
		t.Errorf("Provenance.Similarity = %.2f, below the gate it passed", artifact.Provenance.Similarity)
	}
	if artifact.Script != beforeScript {
		t.Error("the adopted artifact does not carry the script it adopted")
	}
	if report.Reused != "sibling-museum" || report.Tried != 1 {
		t.Errorf("Report = %+v, want the reuse recorded", report)
	}

	// Stored under its own name, with its own page's fingerprint and shape —
	// not a pointer at the other source, which would make one site's heal
	// silently rewrite another's extractor.
	stored, err := store.CurrentArtifact(context.Background(), source.Name)
	if err != nil {
		t.Fatalf("CurrentArtifact: %v", err)
	}
	if stored.Source != source.Name || stored.Version != 1 {
		t.Errorf("stored artifact = %s v%d, want %s v1", stored.Source, stored.Version, source.Name)
	}
	if stored.Shape.Empty() {
		t.Error("the adopted artifact carries no shape, so nothing could ever be reused from it")
	}

	// And it runs: an adopted artifact is a working extractor for this source
	// like any other, with no model configured at all.
	harvester.Generator = nil
	outcome, err := harvester.Once(context.Background(), source)
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}
	if outcome.Run.Verdict != extract.Pass {
		t.Errorf("a run of the adopted artifact graded %s: %v", outcome.Run.Verdict, outcome.Run.Findings)
	}
}

// TestCompileGeneratesWhenNothingStoredFitsThePage is the other half, and the
// one that matters for correctness: reuse must be the exception it can prove,
// not a default that quietly answers for sites nobody checked.
func TestCompileGeneratesWhenNothingStoredFitsThePage(t *testing.T) {
	store := newMemory()
	// A stored extractor for a site with entirely different markup.
	storedSibling(t, store, "unrelated-museum", afterPage, afterScript)

	model := &fixedModel{script: beforeScript}
	harvester := reuseHarvester(store, model, beforePage)

	source := testSource()
	artifact, report, err := harvester.Compile(context.Background(), source)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	if model.calls.Load() == 0 {
		t.Error("nothing stored fitted this page and the model was still not asked")
	}
	if artifact.Provenance.ReusedFrom != "" || report.Reused != "" {
		t.Errorf("an unrelated site's extractor was adopted: %+v", artifact.Provenance)
	}
}

// TestReuseIsRefusedWhenTheScriptDoesNotReadThePage: similarity opens the door
// and the trial closes it. Two sites can share a theme and still differ where it
// matters, and an extractor that comes back with nothing here is no more
// adoptable than an unrelated one.
func TestReuseIsRefusedWhenTheScriptDoesNotReadThePage(t *testing.T) {
	store := newMemory()
	// Same markup as the page being compiled, but the stored script selects
	// something that page does not have.
	storedSibling(t, store, "sibling-museum", siblingPage,
		`function extract(document) { return [...document.querySelectorAll('div.nothing-here')].map(n => ({title: n.innerText, url: '/x'})); }`)

	model := &fixedModel{script: beforeScript}
	harvester := reuseHarvester(store, model, beforePage)

	artifact, _, err := harvester.Compile(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if artifact.Provenance.ReusedFrom != "" {
		t.Error("an extractor that read nothing from this page was adopted anyway")
	}
	if model.calls.Load() == 0 {
		t.Error("the reuse was refused and nothing was generated in its place")
	}
}

// TestReuseSkipsQuarantinedSources: a paused source is one whose extractor
// stopped working and could not be repaired. Whatever is wrong with it is not a
// thing to copy onto a second site because the two look alike.
func TestReuseSkipsQuarantinedSources(t *testing.T) {
	store := newMemory()
	storedSibling(t, store, "sibling-museum", siblingPage, beforeScript)
	if _, err := store.Pause(context.Background(), "sibling-museum", "healed three times in a day"); err != nil {
		t.Fatalf("pause: %v", err)
	}

	model := &fixedModel{script: beforeScript}
	harvester := reuseHarvester(store, model, beforePage)

	artifact, _, err := harvester.Compile(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if artifact.Provenance.ReusedFrom != "" {
		t.Errorf("adopted %q, which is quarantined", artifact.Provenance.ReusedFrom)
	}
}

// TestASecondSiblingInTheSameRunCostsNothing is why a newly compiled artifact
// joins the candidate set immediately. A refresh that meets five sites built
// from one template should generate once, not five times, and waiting for a
// cache to expire would lose exactly that case.
func TestASecondSiblingInTheSameRunCostsNothing(t *testing.T) {
	store := newMemory()
	model := &fixedModel{script: beforeScript}
	harvester := reuseHarvester(store, model, beforePage)

	first := testSource()
	first.Name = "first-museum"
	if _, _, err := harvester.Compile(context.Background(), first); err != nil {
		t.Fatalf("Compile(first) error = %v", err)
	}
	if model.calls.Load() != 1 {
		t.Fatalf("the first site took %d model invocations, want 1", model.calls.Load())
	}

	second := testSource()
	second.Name = "second-museum"
	artifact, _, err := harvester.Compile(context.Background(), second)
	if err != nil {
		t.Fatalf("Compile(second) error = %v", err)
	}

	if model.calls.Load() != 1 {
		t.Errorf("the second site on the same template cost another %d model invocation(s)",
			model.calls.Load()-1)
	}
	if artifact.Provenance.ReusedFrom != "first-museum" {
		t.Errorf("Provenance.ReusedFrom = %q, want the site compiled moments earlier",
			artifact.Provenance.ReusedFrom)
	}
}

// TestReuseSurvivesAStoreThatCannotBeListed: not being able to look for a
// reusable extractor is not a reason to fail a compile. It means generating,
// which is what would have happened before there was anything to reuse.
func TestReuseSurvivesAStoreThatCannotBeListed(t *testing.T) {
	store := &unlistable{newMemory()}
	model := &fixedModel{script: beforeScript}

	harvester := &Harvester{
		Store:     store,
		Fetch:     &pageFetcher{body: beforePage},
		Generator: &extract.Generator{Model: model},
	}

	artifact, _, err := harvester.Compile(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if artifact.Script == "" || model.calls.Load() == 0 {
		t.Error("a store that could not be listed stopped the compile instead of just the reuse")
	}
}

// unlistable is an Archive whose artifact listing fails.
type unlistable struct{ *memory }

func (u *unlistable) CurrentArtifacts(context.Context) ([]extract.Artifact, error) {
	return nil, errUnlistable
}

var errUnlistable = errTest("object storage is unreachable")

type errTest string

func (e errTest) Error() string { return string(e) }

// TestAdoptedArtifactNamesTheModelThatWroteTheScript: provenance has to answer
// "what produced this script", and the answer for an adopted one is the model
// that wrote it for the other site, plus the fact of the adoption. Recording
// this harness as the author would lose the only trail back to it.
func TestAdoptedArtifactNamesTheModelThatWroteTheScript(t *testing.T) {
	store := newMemory()
	storedSibling(t, store, "sibling-museum", siblingPage, beforeScript)

	harvester := reuseHarvester(store, &fixedModel{script: beforeScript}, beforePage)
	artifact, _, err := harvester.Compile(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	if !strings.Contains(artifact.Provenance.Model, "qwen") {
		t.Errorf("Provenance.Model = %q, want the model that wrote the script it adopted",
			artifact.Provenance.Model)
	}
	if artifact.Provenance.PageDigest == "" {
		t.Error("the adopted artifact records no page digest, so what it was checked against is unknown")
	}
}

// TestHealAdoptsARedesignedPageFromASiteAlreadyRunningIt is the fleet-wide
// case, and the one that costs the most model time if it is missed. A CMS
// vendor rolls a new theme out to every site it hosts; every one of them breaks
// in the same way, in the same month — and afterwards they are all identical
// again. The first site healed should pay for the rest.
func TestHealAdoptsARedesignedPageFromASiteAlreadyRunningIt(t *testing.T) {
	store := newMemory()

	source := testSource()
	before, err := extract.ParsePage(source.URL, beforePage)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := store.SaveArtifact(context.Background(), extract.Artifact{
		Source: source.Name, Version: 1, Script: beforeScript,
		Fingerprint: extract.Fingerprint(before), Shape: extract.ShapeOf(before),
	}); err != nil {
		t.Fatalf("save artifact: %v", err)
	}

	// Another site that has already moved to the new theme, with a working
	// extractor for it.
	storedSibling(t, store, "sibling-museum", afterPage, afterScript)

	// This site's page is now the new theme, and its extractor reads nothing.
	model := &fixedModel{script: afterScript}
	harvester := reuseHarvester(store, model, afterPage)

	outcome, err := harvester.Once(context.Background(), source)
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}

	if outcome.Healed == nil {
		t.Fatalf("the source was not healed: verdict %s, %v", outcome.Run.Verdict, outcome.Run.Findings)
	}
	if model.calls.Load() != 0 {
		t.Errorf("the model was invoked %d times to rediscover a page another site already reads",
			model.calls.Load())
	}
	if outcome.Healed.Provenance.ReusedFrom != "sibling-museum" {
		t.Errorf("healed artifact ReusedFrom = %q, want the site already on the new theme",
			outcome.Healed.Provenance.ReusedFrom)
	}
	if outcome.Healed.Version != 2 || outcome.Healed.Parent != 1 {
		t.Errorf("healed to v%d from v%d, want v2 from v1 — an adopted heal is still a heal, "+
			"and rollback has to reach the version it replaced",
			outcome.Healed.Version, outcome.Healed.Parent)
	}
	if outcome.Run.Verdict != extract.Pass {
		t.Errorf("the run after the adopted heal graded %s: %v", outcome.Run.Verdict, outcome.Run.Findings)
	}
}
