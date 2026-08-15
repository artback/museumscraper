package harvest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/artback/museumscraper/extract"
)

// memory is an in-memory Archive, so the loop can be exercised against
// histories that would be slow and fiddly to build in a real bucket.
type memory struct {
	// The harvester is shared across the refresh scraper's workers, so a fake
	// standing in for its store has to tolerate the same concurrency the real
	// one does — otherwise the fake, not the code, is what fails the race
	// detector.
	mu        sync.Mutex
	sources   map[string]extract.Source
	artifacts map[string][]extract.Artifact
	runs      map[string][]extract.Run

	paused map[string]string
	// failHistory makes History return an error, to check the loop degrades
	// rather than stops.
	failHistory bool
}

func newMemory() *memory {
	return &memory{
		sources:   make(map[string]extract.Source),
		artifacts: make(map[string][]extract.Artifact),
		runs:      make(map[string][]extract.Run),
		paused:    make(map[string]string),
	}
}

func (m *memory) Sources(context.Context) ([]extract.Source, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var sources []extract.Source
	for _, source := range m.sources {
		sources = append(sources, source)
	}
	return sources, nil
}

func (m *memory) CurrentArtifact(_ context.Context, source string) (extract.Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	versions := m.artifacts[source]
	if len(versions) == 0 {
		return extract.Artifact{}, fmt.Errorf("%w: %s", ErrNoArtifact, source)
	}
	return versions[len(versions)-1], nil
}

func (m *memory) SaveArtifact(_ context.Context, artifact extract.Artifact) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.artifacts[artifact.Source] = append(m.artifacts[artifact.Source], artifact)
	return nil
}

func (m *memory) AppendRun(_ context.Context, run extract.Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.runs[run.Source] = append([]extract.Run{run}, m.runs[run.Source]...)
	return nil
}

func (m *memory) Runs(_ context.Context, source string, limit int) ([]extract.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	runs := m.runs[source]
	if limit > 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

func (m *memory) LastRunAt(_ context.Context, source string) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	runs := m.runs[source]
	if len(runs) == 0 {
		return time.Time{}, false, nil
	}
	newest := runs[0].At
	for _, run := range runs {
		if run.At.After(newest) {
			newest = run.At
		}
	}
	return newest, true, nil
}

func (m *memory) History(_ context.Context, source string) (extract.History, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failHistory {
		return extract.History{}, errors.New("storage unavailable")
	}
	// Mirrors the real store: counts and the newest passing fingerprint come
	// from the same read, and a fake that omitted the fingerprint would make
	// the drift-baseline tests pass for the wrong reason.
	var (
		counts      []int
		fingerprint string
	)
	for _, run := range m.runs[source] {
		if run.Verdict != extract.Pass {
			continue
		}
		if fingerprint == "" {
			fingerprint = run.Fingerprint
		}
		counts = append(counts, run.Records)
	}
	return extract.History{Counts: counts, Fingerprint: fingerprint, Complete: true}, nil
}

func (m *memory) PruneRuns(context.Context, string) (int, error) { return 0, nil }

func (m *memory) Pause(_ context.Context, name, reason string) (extract.Source, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.paused[name] = reason
	source := m.sources[name]
	source.Paused, source.PausedReason = true, reason
	m.sources[name] = source
	return source, nil
}

// pageFetcher serves a fixed body, or an error.
type pageFetcher struct {
	body string
	err  error
	hits atomic.Int64
}

func (f *pageFetcher) Get(context.Context, string) (string, string, error) {
	f.hits.Add(1)
	if f.err != nil {
		return "", "", f.err
	}
	return f.body, "https://example.org/whats-on", nil
}

// recordingSink captures deliveries.
type recordingSink struct {
	mu         sync.Mutex
	deliveries []Delivery
	err        error
}

func (s *recordingSink) Publish(_ context.Context, source extract.Source, run extract.Run, records []extract.Record) error {
	if s.err != nil {
		return s.err
	}
	delivery, err := NewDelivery(source, run, records)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveries = append(s.deliveries, delivery)
	return nil
}

// count reports how many deliveries were recorded.
func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.deliveries)
}

// The fixtures. The "before" page and a script that reads it, then a redesign
// that the script no longer matches.
const (
	beforePage = `<html><body><ul class="exhibitions">
	  <li class="exhibition"><h3>Bronze Age Britain</h3>
	    <a href="/exhibitions/bronze-age">more</a></li>
	  <li class="exhibition"><h3>Silk Roads</h3>
	    <a href="/exhibitions/silk-roads">more</a></li>
	  <li class="exhibition"><h3>Ancient Greece</h3>
	    <a href="/exhibitions/greece">more</a></li>
	</ul></body></html>`

	afterPage = `<html><body><div class="programme">
	  <article class="card"><h2>Bronze Age Britain</h2>
	    <a class="card__link" href="/exhibitions/bronze-age">more</a></article>
	  <article class="card"><h2>Silk Roads</h2>
	    <a class="card__link" href="/exhibitions/silk-roads">more</a></article>
	  <article class="card"><h2>Ancient Greece</h2>
	    <a class="card__link" href="/exhibitions/greece">more</a></article>
	</div></body></html>`

	beforeScript = `function extract(document) {
	  return [...document.querySelectorAll('li.exhibition')].map(row => ({
	    title: row.querySelector('h3').innerText,
	    url: row.querySelector('a').href,
	  }));
	}`

	afterScript = `function extract(document) {
	  return [...document.querySelectorAll('article.card')].map(row => ({
	    title: row.querySelector('h2').innerText,
	    url: row.querySelector('a.card__link').href,
	  }));
	}`
)

func testSource() extract.Source {
	return extract.Source{
		Name:   "example-museum",
		URL:    "https://example.org/whats-on",
		Every:  extract.Duration(time.Hour),
		Expect: extract.Expectation{MinRecords: 1},
		Schema: extract.Schema{
			Name: "exhibitions",
			Fields: []extract.Field{
				{Name: "title", Kind: extract.KindString, Required: true, Rules: extract.Rules{MinLength: 2}},
				{Name: "url", Kind: extract.KindURL, Required: true},
			},
		},
	}
}

// fixedModel answers every request with the same script.
type fixedModel struct {
	script string
	err    error
	calls  atomic.Int64
}

func (m *fixedModel) Name() string { return "fixed" }

func (m *fixedModel) Complete(context.Context, string, string) (string, error) {
	m.calls.Add(1)
	if m.err != nil {
		return "", m.err
	}
	encoded, err := json.Marshal(map[string]string{"script": m.script})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// harness builds a harvester wired to fakes.
type harness struct {
	store   *memory
	fetch   *pageFetcher
	sink    *recordingSink
	model   *fixedModel
	harvest *Harvester
}

func newHarness(t *testing.T, page, script string) *harness {
	t.Helper()

	store := newMemory()
	source := testSource()
	store.sources[source.Name] = source
	store.artifacts[source.Name] = []extract.Artifact{{
		Source:      source.Name,
		Version:     1,
		Script:      beforeScript,
		Fingerprint: fingerprintOf(t, beforePage),
	}}

	fetch := &pageFetcher{body: page}
	sink := &recordingSink{}
	model := &fixedModel{script: script}

	return &harness{
		store: store, fetch: fetch, sink: sink, model: model,
		harvest: &Harvester{
			Store:     store,
			Fetch:     fetch,
			Sink:      sink,
			Generator: &extract.Generator{Model: model},
			Now:       func() time.Time { return time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC) },
		},
	}
}

func fingerprintOf(t *testing.T, body string) string {
	t.Helper()
	page, err := extract.ParsePage("https://example.org/whats-on", body)
	if err != nil {
		t.Fatalf("ParsePage() error = %v", err)
	}
	return extract.Fingerprint(page)
}

func TestOncePasses(t *testing.T) {
	h := newHarness(t, beforePage, afterScript)

	outcome, err := h.harvest.Once(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}

	if outcome.Run.Verdict != extract.Pass {
		t.Errorf("Once() verdict = %s, want %s. Findings: %v",
			outcome.Run.Verdict, extract.Pass, outcome.Run.Findings)
	}
	if outcome.Run.Records != 3 {
		t.Errorf("Once() extracted %d records, want 3", outcome.Run.Records)
	}
	if !outcome.Published {
		t.Error("Once() did not publish a passing run")
	}
	if h.model.calls.Load() != 0 {
		t.Errorf("a passing run cost %d model invocations, want 0", h.model.calls.Load())
	}
	if outcome.Run.Drifted {
		t.Error("Once() reported drift against the page the artifact was written from")
	}
}

// TestOnceHealsAfterRedesign is the whole loop: the site was rebuilt, the
// stored artifact matches nothing, and the harness repairs itself.
func TestOnceHealsAfterRedesign(t *testing.T) {
	h := newHarness(t, afterPage, afterScript)

	outcome, err := h.harvest.Once(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}

	if !outcome.Run.Drifted {
		t.Error("Once() did not notice the page had been rebuilt")
	}
	if outcome.Healed == nil {
		t.Fatalf("Once() did not heal. Verdict %s, findings %v",
			outcome.Run.Verdict, outcome.Run.Findings)
	}

	if outcome.Healed.Version != 2 {
		t.Errorf("healed to v%d, want v2", outcome.Healed.Version)
	}
	if outcome.Healed.Parent != 1 {
		t.Errorf("healed artifact parent = %d, want 1", outcome.Healed.Parent)
	}
	if outcome.Healed.Reason == "" {
		t.Error("healed artifact records no reason, so its diff would not explain itself")
	}

	if outcome.Run.Verdict != extract.Pass || outcome.Run.Records != 3 {
		t.Errorf("after healing, verdict = %s with %d records, want pass with 3",
			outcome.Run.Verdict, outcome.Run.Records)
	}
	if !outcome.Published {
		t.Error("the healed run was not published")
	}

	// The replacement must be stored, or the next run heals again.
	if got := len(h.store.artifacts[testSource().Name]); got != 2 {
		t.Errorf("store holds %d artifact versions, want 2", got)
	}
	// And the old one must still be there to roll back to and to diff against.
	if h.store.artifacts[testSource().Name][0].Script != beforeScript {
		t.Error("healing overwrote the previous version instead of adding one")
	}
}

// TestOnceDoesNotHealWithoutDrift guards the false-heal rate: a source that is
// merely having a quiet week must not cost a model invocation.
func TestOnceDoesNotHealWithoutDrift(t *testing.T) {
	// A page with the same structure but only one entry, against a history of
	// twenty.
	const quiet = `<html><body><ul class="exhibitions">
	  <li class="exhibition"><h3>Bronze Age Britain</h3>
	    <a href="/exhibitions/bronze-age">more</a></li>
	</ul></body></html>`

	h := newHarness(t, quiet, afterScript)
	for range 4 {
		h.store.runs["example-museum"] = append(h.store.runs["example-museum"],
			extract.Run{Source: "example-museum", Verdict: extract.Pass, Records: 20})
	}

	outcome, err := h.harvest.Once(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}

	if outcome.Run.Verdict != extract.Suspect {
		t.Errorf("Once() verdict = %s, want %s for a collapse from 20 to 1",
			outcome.Run.Verdict, extract.Suspect)
	}
	if outcome.Healed != nil {
		t.Error("Once() healed a source whose page structure had not changed")
	}
	if h.model.calls.Load() != 0 {
		t.Errorf("a suspect run without drift cost %d model invocations, want 0", h.model.calls.Load())
	}
	if outcome.Published {
		t.Error("a suspect result was published; only pass may be")
	}
}

// TestOnceDoesNotHealOnFetchFailure keeps a network blip from costing a model
// invocation.
func TestOnceDoesNotHealOnFetchFailure(t *testing.T) {
	h := newHarness(t, beforePage, afterScript)
	h.fetch.err = errors.New("dial tcp: connection refused")

	outcome, err := h.harvest.Once(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}

	if outcome.Run.Verdict != extract.Fail {
		t.Errorf("Once() verdict = %s, want %s", outcome.Run.Verdict, extract.Fail)
	}
	if outcome.Healed != nil || h.model.calls.Load() != 0 {
		t.Errorf("a fetch failure triggered healing (%d model invocations)", h.model.calls.Load())
	}
	if outcome.Run.Err == "" {
		t.Error("the run does not record what went wrong")
	}
	if outcome.Published {
		t.Error("a failed fetch published something")
	}
}

// TestOnceQuarantinesAfterRepeatedHeals is the cap that stops a dead source
// burning the budget on every tick.
func TestOnceQuarantinesAfterRepeatedHeals(t *testing.T) {
	h := newHarness(t, afterPage, afterScript)

	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	for i := range extract.DefaultHealLimit {
		h.store.runs["example-museum"] = append(h.store.runs["example-museum"], extract.Run{
			Source: "example-museum",
			At:     now.Add(-time.Duration(i+1) * time.Hour),
			Healed: true,
		})
	}

	outcome, err := h.harvest.Once(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}

	if !outcome.Quarantined {
		t.Fatalf("Once() did not quarantine after %d heals in the window", extract.DefaultHealLimit)
	}
	if h.model.calls.Load() != 0 {
		t.Errorf("a quarantined source still cost %d model invocations, want 0", h.model.calls.Load())
	}
	if reason := h.store.paused["example-museum"]; reason == "" {
		t.Error("the source was reported quarantined but not actually paused")
	}
}

// TestOnceQuarantinesWhenHealingCannotFix covers the lifecycle's last branch:
// heal ran, failed, and the source is escalated rather than left to retry.
func TestOnceQuarantinesWhenHealingFails(t *testing.T) {
	// The model keeps returning the script that already does not work.
	h := newHarness(t, afterPage, beforeScript)

	outcome, err := h.harvest.Once(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}

	if !outcome.Quarantined {
		t.Errorf("Once() did not quarantine after healing failed. Alert: %q", outcome.Alert)
	}
	if outcome.Healed != nil {
		t.Error("Once() stored an artifact that could not extract the page")
	}
	if len(h.store.artifacts["example-museum"]) != 1 {
		t.Error("a failed heal added a version to the store")
	}
	if outcome.Published {
		t.Error("a quarantined run published something")
	}
}

func TestOnceWithoutGeneratorStillRuns(t *testing.T) {
	// A runner deployed with no model executes and grades, it just cannot
	// repair. That has to work rather than crash.
	h := newHarness(t, afterPage, afterScript)
	h.harvest.Generator = nil

	outcome, err := h.harvest.Once(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}
	if outcome.Run.Verdict != extract.Fail {
		t.Errorf("Once() verdict = %s, want %s", outcome.Run.Verdict, extract.Fail)
	}
	if outcome.Healed != nil {
		t.Error("Once() healed without a generator")
	}
}

func TestOnceSinkFailureDoesNotFailTheRun(t *testing.T) {
	h := newHarness(t, beforePage, afterScript)
	h.sink.err = errors.New("webhook returned 503")

	outcome, err := h.harvest.Once(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}

	// A broken sink must not make a healthy source look broken, or the next
	// run would heal an artifact that is working perfectly.
	if outcome.Run.Verdict != extract.Pass {
		t.Errorf("Once() verdict = %s, want %s when only delivery failed",
			outcome.Run.Verdict, extract.Pass)
	}
	if outcome.Published {
		t.Error("Once() reported publishing despite the sink failing")
	}
}

func TestOnceRecordsHistory(t *testing.T) {
	h := newHarness(t, beforePage, afterScript)

	if _, err := h.harvest.Once(context.Background(), testSource()); err != nil {
		t.Fatalf("Once() error = %v", err)
	}

	runs := h.store.runs["example-museum"]
	if len(runs) != 1 {
		t.Fatalf("store holds %d runs, want 1", len(runs))
	}
	switch run := runs[0]; {
	case run.Fingerprint == "":
		t.Error("the recorded run carries no fingerprint, so the next run cannot detect drift")
	case run.Version != 1:
		t.Errorf("recorded version = %d, want 1", run.Version)
	case run.At.IsZero():
		t.Error("the recorded run has no timestamp")
	}
}

func TestCompile(t *testing.T) {
	store := newMemory()
	source := testSource()
	store.sources[source.Name] = source

	model := &fixedModel{script: beforeScript}
	harvester := &Harvester{
		Store:     store,
		Fetch:     &pageFetcher{body: beforePage},
		Generator: &extract.Generator{Model: model},
	}

	artifact, report, err := harvester.Compile(context.Background(), source)
	if err != nil {
		t.Fatalf("Compile() error = %v (attempts %+v)", err, report.Attempts)
	}
	if artifact.Version != 1 {
		t.Errorf("Compile() version = %d, want 1", artifact.Version)
	}
	if len(store.artifacts[source.Name]) != 1 {
		t.Error("Compile() did not store the artifact")
	}
	if artifact.Fingerprint == "" {
		t.Error("Compile() stored no fingerprint")
	}
}

func TestCompileWithoutGenerator(t *testing.T) {
	harvester := &Harvester{Store: newMemory(), Fetch: &pageFetcher{body: beforePage}}

	if _, _, err := harvester.Compile(context.Background(), testSource()); !errors.Is(err, ErrNoGenerator) {
		t.Errorf("Compile() without a model error = %v, want ErrNoGenerator", err)
	}
}

func TestDeliveryKeyIsStable(t *testing.T) {
	source := testSource()
	records := []extract.Record{{"title": "Bronze Age Britain", "url": "https://example.org/a"}}

	first, err := NewDelivery(source, extract.Run{Version: 1, At: time.Now()}, records)
	if err != nil {
		t.Fatalf("NewDelivery() error = %v", err)
	}
	// Same records, a later run, a healed artifact: downstream this is not new
	// data, and the key has to say so.
	second, err := NewDelivery(source, extract.Run{Version: 7, At: time.Now().Add(time.Hour)}, records)
	if err != nil {
		t.Fatalf("NewDelivery() error = %v", err)
	}

	if first.Key != second.Key {
		t.Errorf("delivery keys differ for identical records: %s vs %s", first.Key, second.Key)
	}

	changed, err := NewDelivery(source, extract.Run{Version: 1}, []extract.Record{
		{"title": "Silk Roads", "url": "https://example.org/b"},
	})
	if err != nil {
		t.Fatalf("NewDelivery() error = %v", err)
	}
	if changed.Key == first.Key {
		t.Error("delivery key did not change when the records did")
	}
}

func TestSchedulerSkipsPausedAndUndueSources(t *testing.T) {
	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	store := newMemory()

	hourly := func(name string) extract.Source {
		s := testSource()
		s.Name = name
		return s
	}

	store.sources["due"] = hourly("due")
	store.runs["due"] = []extract.Run{{Source: "due", At: now.Add(-2 * time.Hour)}}

	store.sources["recent"] = hourly("recent")
	store.runs["recent"] = []extract.Run{{Source: "recent", At: now.Add(-10 * time.Minute)}}

	paused := hourly("paused")
	paused.Paused = true
	store.sources["paused"] = paused

	manual := hourly("manual")
	manual.Every = 0
	store.sources["manual"] = manual

	store.sources["never-run"] = hourly("never-run")

	scheduler := &Scheduler{Store: store, Now: func() time.Time { return now }}
	due, err := scheduler.due(context.Background())
	if err != nil {
		t.Fatalf("due() error = %v", err)
	}

	got := make(map[string]bool, len(due))
	for _, source := range due {
		got[source.Name] = true
	}

	for name, want := range map[string]bool{
		"due":       true,
		"never-run": true,
		"recent":    false,
		"paused":    false,
		"manual":    false,
	} {
		if got[name] != want {
			t.Errorf("due() included %q = %t, want %t (got %v)", name, got[name], want, keys(got))
		}
	}
}

func TestSchedulerClaimIsExclusive(t *testing.T) {
	scheduler := &Scheduler{}

	if !scheduler.claim("a") {
		t.Fatal("claim() refused the first claim")
	}
	if scheduler.claim("a") {
		t.Error("claim() allowed a source to run twice at once; it must be skipped, not queued")
	}

	scheduler.release("a")
	if !scheduler.claim("a") {
		t.Error("claim() refused after release")
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	return out
}

// TestOnceDegradesWithoutHistory checks the loop still grades when the store
// cannot answer, rather than failing the run.
func TestOnceDegradesWithoutHistory(t *testing.T) {
	h := newHarness(t, beforePage, afterScript)
	h.store.failHistory = true

	outcome, err := h.harvest.Once(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}
	if outcome.Run.Verdict != extract.Pass {
		t.Errorf("Once() verdict = %s, want %s when only the baseline was unavailable",
			outcome.Run.Verdict, extract.Pass)
	}
}

func TestSinksReportEveryFailure(t *testing.T) {
	failing := &recordingSink{err: errors.New("first sink down")}
	working := &recordingSink{}

	err := Sinks{failing, working}.Publish(context.Background(),
		testSource(), extract.Run{}, []extract.Record{{"title": "a"}})

	if err == nil {
		t.Fatal("Sinks.Publish() reported success despite a sink failing")
	}
	if !strings.Contains(err.Error(), "first sink down") {
		t.Errorf("Sinks.Publish() error = %v, want it to name the failure", err)
	}
	// One sink failing must not stop the others.
	if working.count() != 1 {
		t.Error("Sinks.Publish() stopped at the first failure instead of delivering to the rest")
	}
}

// TestDriftBaselineFollowsPassingRuns covers a failure that would have
// quarantined healthy sources months after deployment.
//
// An artifact records the page it was generated from and is then immutable. If
// drift is measured against that, the first cosmetic change a still-working
// extractor survives makes every later run report drift forever — and drift is
// what turns an ordinary seasonal dip into grounds for healing. The baseline
// has to follow the last passing run instead.
func TestDriftBaselineFollowsPassingRuns(t *testing.T) {
	h := newHarness(t, beforePage, afterScript)

	// The site was tweaked cosmetically after the artifact was written: a new
	// wrapper class the extractor does not care about. Extraction still works.
	tweaked := strings.Replace(beforePage,
		`<ul class="exhibitions">`, `<div class="wrapper"><ul class="exhibitions">`, 1)
	tweaked = strings.Replace(tweaked, `</ul>`, `</ul></div>`, 1)
	h.fetch.body = tweaked

	first, err := h.harvest.Once(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}
	if first.Run.Verdict != extract.Pass {
		t.Fatalf("Once() verdict = %s, want %s — the tweak should not break extraction",
			first.Run.Verdict, first.Run.Findings)
	}
	if !first.Run.Drifted {
		t.Fatal("the first run after a real markup change should report drift")
	}

	// The passing run is now the baseline, so the same page is no longer drift.
	second, err := h.harvest.Once(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}
	if second.Run.Drifted {
		t.Error("drift was still reported against an unchanged page; the baseline did not move " +
			"to the last passing run, so a seasonal dip would later be read as a partial break")
	}
	if h.model.calls.Load() != 0 {
		t.Errorf("a passing source cost %d model invocations, want 0", h.model.calls.Load())
	}
}

// TestHealDoesNotQuarantineForAnUnchangedVerdict covers the other half of the
// same failure: a replacement that grades no worse than what it replaced has
// not failed to fix anything.
func TestHealDoesNotQuarantineForAnUnchangedVerdict(t *testing.T) {
	// One entry against a history of twenty, on a page whose structure has
	// changed — so the run is suspect AND drifted, which authorises a heal.
	const quiet = `<html><body><div class="programme">
	  <article class="card"><h2>Bronze Age Britain</h2>
	    <a class="card__link" href="/exhibitions/bronze-age">more</a></article>
	</div></body></html>`

	h := newHarness(t, quiet, afterScript)
	for range 4 {
		h.store.runs["example-museum"] = append(h.store.runs["example-museum"],
			extract.Run{Source: "example-museum", Verdict: extract.Pass, Records: 20})
	}

	outcome, err := h.harvest.Once(context.Background(), testSource())
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}

	if outcome.Healed == nil {
		t.Fatalf("Once() did not heal a suspect-and-drifted run: %v", outcome.Run.Findings)
	}
	// The museum genuinely has one show on. The healed extractor reads it
	// correctly and is still graded suspect for the count — that is not a
	// reason to quarantine a source that is working.
	if outcome.Quarantined {
		t.Errorf("a healthy source was quarantined because its healed extractor graded "+
			"the same as before: %s", outcome.Alert)
	}
}

// TestHarvesterIsSafeForConcurrentUse covers the way this is actually driven
// in production: the exhibitions fallback shares one Harvester across the
// refresh scraper's workers, eight by default.
func TestHarvesterIsSafeForConcurrentUse(t *testing.T) {
	// The page and the script the model returns have to match, or every call
	// fails generation and the test proves nothing about concurrency.
	h := newHarness(t, beforePage, beforeScript)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Draft and Once between them touch every lazily initialised
			// field on the harvester.
			if _, _, err := h.harvest.Draft(context.Background(), testSource()); err != nil {
				t.Errorf("Draft() error = %v", err)
			}
			if _, err := h.harvest.Once(context.Background(), testSource()); err != nil {
				t.Errorf("Once() error = %v", err)
			}
		}()
	}
	wg.Wait()
}
