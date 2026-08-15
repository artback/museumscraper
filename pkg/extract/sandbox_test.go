package extract

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// listingPage is the shape a museum programme page actually takes: a repeated
// row carrying a title, a relative link, and machine-readable dates in a
// datetime attribute next to human-readable ones in the text.
const listingPage = `<!doctype html>
<html><head><title>What's On — Example Museum</title>
<script>var tracking = {visitors: 12};</script>
<style>.exhibition { color: red }</style>
</head><body>
<nav><a href="/">Home</a><a href="/whats-on">What's On</a></nav>
<main>
  <ul class="exhibitions">
    <li class="exhibition" data-entry-id="a1">
      <h3 class="title">Bronze Age Britain</h3>
      <a class="more" href="/exhibitions/bronze-age">Find out more</a>
      <time class="from" datetime="2026-09-01">1 September</time>
      <time class="to" datetime="2027-01-15">15 January</time>
    </li>
    <li class="exhibition" data-entry-id="a2">
      <h3 class="title">Silk Roads</h3>
      <a class="more" href="/exhibitions/silk-roads">Find out more</a>
      <time class="from" datetime="2026-10-12">12 October</time>
      <time class="to" datetime="2027-02-28">28 February</time>
    </li>
  </ul>
</main>
</body></html>`

// listingScript is what a well-behaved generated artifact looks like.
const listingScript = `function extract(document) {
  return [...document.querySelectorAll('li.exhibition')].map(row => ({
    title: row.querySelector('h3.title').innerText,
    url:   row.querySelector('a.more').href,
    opens: row.querySelector('time.from').getAttribute('datetime'),
    closes: row.querySelector('time.to').getAttribute('datetime'),
  }));
}`

func testPage(t *testing.T, body string) *Page {
	t.Helper()
	page, err := ParsePage("https://example.org/whats-on", body)
	if err != nil {
		t.Fatalf("ParsePage() error = %v", err)
	}
	return page
}

// run executes a script against the standard listing page.
func run(t *testing.T, script string) (Output, error) {
	t.Helper()
	return NewSandbox().Run(context.Background(), script, testPage(t, listingPage))
}

func TestSandboxRun(t *testing.T) {
	out, err := run(t, listingScript)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(out.Records) != 2 {
		t.Fatalf("Run() returned %d records, want 2: %+v", len(out.Records), out.Records)
	}

	first := out.Records[0]
	if got, _ := first.String("title"); got != "Bronze Age Britain" {
		t.Errorf("record[0].title = %q, want %q", got, "Bronze Age Britain")
	}
	// The href in the markup is relative; the DOM resolves it as a browser
	// would, so the artifact never has to know the page's own address.
	if got, _ := first.String("url"); got != "https://example.org/exhibitions/bronze-age" {
		t.Errorf("record[0].url = %q, want the absolute URL", got)
	}
	if got, _ := first.String("opens"); got != "2026-09-01" {
		t.Errorf("record[0].opens = %q, want %q", got, "2026-09-01")
	}
}

// TestSandboxHasNoCapabilities is the test the whole design rests on. The
// isolation claim is that a generated script cannot reach the network, the
// host, or anything that persists — and that this is true because none of it
// was ever installed, not because a filter rejected it.
func TestSandboxHasNoCapabilities(t *testing.T) {
	forbidden := []string{
		"fetch", "XMLHttpRequest", "WebSocket", "require", "process",
		"setTimeout", "setInterval", "globalThis.Go", "importScripts",
		"localStorage", "sessionStorage", "indexedDB", "Worker", "eval_file",
	}

	for _, name := range forbidden {
		t.Run(name, func(t *testing.T) {
			script := `function extract(document) { return [{present: typeof ` + name + ` !== "undefined"}]; }`
			out, err := run(t, script)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if present, _ := out.Records[0]["present"].(bool); present {
				t.Errorf("%s is reachable from a generated artifact; it must not be", name)
			}
		})
	}
}

// TestSandboxDocumentIsReadOnly checks that a script cannot change the page it
// was given. Nothing downstream reads the mutated tree, so a write that
// appeared to succeed would only mislead whoever reviewed the artifact.
func TestSandboxDocumentIsReadOnly(t *testing.T) {
	const script = `function extract(document) {
	  const row = document.querySelector('li.exhibition');
	  let threw = false;
	  try { row.textContent = 'nonsense'; } catch (e) { threw = true; }
	  return [{after: document.querySelector('li.exhibition h3').innerText, threw: threw}];
	}`

	out, err := run(t, script)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, _ := out.Records[0].String("after"); got != "Bronze Age Britain" {
		t.Errorf("after assignment, title = %q, want it unchanged", got)
	}
}

func TestSandboxTimeout(t *testing.T) {
	sandbox := NewSandbox()
	sandbox.Timeout = 100 * time.Millisecond

	const spin = `function extract(document) { for (;;) {} }`
	started := time.Now()
	_, err := sandbox.Run(context.Background(), spin, testPage(t, listingPage))

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Run(infinite loop) error = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("Run(infinite loop) took %s, want it interrupted near the 100ms ceiling", elapsed)
	}
}

func TestSandboxCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewSandbox().Run(ctx, `function extract(document) { for (;;) {} }`, testPage(t, listingPage))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Run(cancelled ctx) error = %v, want the run interrupted", err)
	}
}

func TestSandboxRejectsBadArtifacts(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   error
	}{
		{
			name:   "not javascript",
			script: `function extract( { ??? }`,
			want:   ErrCompile,
		},
		{
			name:   "no entry point",
			script: `function scrape(document) { return []; }`,
			want:   ErrNoEntryPoint,
		},
		{
			name:   "throws",
			script: `function extract(document) { return document.nothing.here; }`,
			want:   ErrScript,
		},
		{
			name:   "returns an object rather than an array",
			script: `function extract(document) { return {title: "one"}; }`,
			want:   ErrShape,
		},
		{
			name:   "returns nothing",
			script: `function extract(document) { }`,
			want:   ErrShape,
		},
		{
			name:   "returns an array of strings",
			script: `function extract(document) { return ["Bronze Age Britain"]; }`,
			want:   ErrShape,
		},
		{
			name:   "invalid selector",
			script: `function extract(document) { return [...document.querySelectorAll('li[[')]; }`,
			want:   ErrScript,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := run(t, tt.script)
			if !errors.Is(err, tt.want) {
				t.Errorf("Run(%s) error = %v, want %v", tt.name, err, tt.want)
			}
		})
	}
}

func TestSandboxCapsRecords(t *testing.T) {
	sandbox := NewSandbox()
	sandbox.MaxRecords = 10

	const flood = `function extract(document) {
	  const out = [];
	  for (let i = 0; i < 500; i++) out.push({title: "row " + i});
	  return out;
	}`

	_, err := sandbox.Run(context.Background(), flood, testPage(t, listingPage))
	if !errors.Is(err, ErrShape) {
		t.Errorf("Run(500 records with a cap of 10) error = %v, want ErrShape", err)
	}
}

// TestSandboxElementIdentity checks that a node reached twice is the same
// object, because scripts deduplicate rows with === and every comparison would
// otherwise be false.
func TestSandboxElementIdentity(t *testing.T) {
	const script = `function extract(document) {
	  const a = document.querySelector('li.exhibition');
	  const b = document.querySelectorAll('li.exhibition')[0];
	  return [{same: a === b, viaParent: a.querySelector('h3').parentElement === a}];
	}`

	out, err := run(t, script)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if same, _ := out.Records[0]["same"].(bool); !same {
		t.Error("the same element reached two ways compared unequal")
	}
	if viaParent, _ := out.Records[0]["viaParent"].(bool); !viaParent {
		t.Error("parentElement did not return the identical object")
	}
}

// TestSandboxText covers the distinction that decides whether extracted titles
// arrive usable: textContent is raw, innerText is collapsed and skips markup
// that is never shown.
func TestSandboxText(t *testing.T) {
	const page = `<html><body><div id="d">
	  <script>var hidden = 1;</script>
	  <style>.x{}</style>
	  <h3>  Bronze   Age
	  Britain  </h3>
	</div></body></html>`

	const script = `function extract(document) {
	  const d = document.getElementById('d');
	  return [{inner: d.innerText, raw: d.textContent}];
	}`

	out, err := NewSandbox().Run(context.Background(), script, testPage(t, page))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if got, _ := out.Records[0].String("inner"); got != "Bronze Age Britain" {
		t.Errorf("innerText = %q, want %q", got, "Bronze Age Britain")
	}
	if raw, _ := out.Records[0].String("raw"); !strings.Contains(raw, "var hidden") {
		t.Errorf("textContent = %q, want it to include script text as the DOM does", raw)
	}
}

func TestSandboxNormalisesNumbers(t *testing.T) {
	const script = `function extract(document) {
	  return [{count: 3, ratio: 1.5, flag: true, missing: null}];
	}`

	out, err := run(t, script)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// Whole numbers arrive from goja as int64. Records round-trip through JSON
	// and are compared against numeric rules, both of which want one type.
	record := out.Records[0]
	if _, ok := record["count"].(float64); !ok {
		t.Errorf("count is %T, want float64", record["count"])
	}
	if _, ok := record["ratio"].(float64); !ok {
		t.Errorf("ratio is %T, want float64", record["ratio"])
	}
	if flag, ok := record["flag"].(bool); !ok || !flag {
		t.Errorf("flag = %v (%T), want true", record["flag"], record["flag"])
	}
	if record["missing"] != nil {
		t.Errorf("missing = %v, want nil", record["missing"])
	}
}

func TestSandboxCapturesConsole(t *testing.T) {
	const script = `function extract(document) {
	  console.log("rows", document.querySelectorAll('li.exhibition').length);
	  return [{title: "one"}];
	}`

	out, err := run(t, script)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(out.Console, "rows 2") {
		t.Errorf("Console = %q, want it to contain %q", out.Console, "rows 2")
	}
}

func TestCompile(t *testing.T) {
	if err := Compile(listingScript); err != nil {
		t.Errorf("Compile(valid script) error = %v, want nil", err)
	}
	if err := Compile(`function extract( {`); !errors.Is(err, ErrCompile) {
		t.Errorf("Compile(invalid script) error = %v, want ErrCompile", err)
	}
}

// TestSandboxContainsPanicsFromReturnedValues covers the defect that a
// returned value is not inert: goja signals a JS throw by panicking, and an
// accessor property on a returned object runs when that value is exported —
// outside any goja frame, where goja does not recover it.
//
// The trigger is not an attack. `this.el` being undefined inside a getter is an
// ordinary mistake for a model to make, and the runner is called from a bare
// goroutine in the scheduler, where a panic takes the whole process with it.
func TestSandboxContainsPanicsFromReturnedValues(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   error
	}{
		{
			name:   "getter throws a TypeError",
			script: `function extract(document) { return [{get title() { return this.el.textContent; }}]; }`,
			want:   ErrScript,
		},
		{
			name:   "getter throws explicitly",
			script: `function extract(document) { return [{get title() { throw new Error("boom"); }}]; }`,
			want:   ErrScript,
		},
		{
			name:   "getter on a nested object",
			script: `function extract(document) { return [{title: {get x() { throw new Error("boom"); }}}]; }`,
			want:   ErrScript,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A panic here fails the test by killing the process, which is
			// exactly the behaviour being guarded against.
			_, err := run(t, tt.script)
			if !errors.Is(err, tt.want) {
				t.Errorf("Run(%s) error = %v, want %v", tt.name, err, tt.want)
			}
		})
	}
}

// TestSandboxRejectsHugeArrayWithoutAllocating checks that MaxRecords is
// enforced against the array's declared length rather than against a Go slice
// that has already been built from it.
//
// `new Array(1e9)` is sparse: goja builds it in microseconds, so the timeout
// has nothing to interrupt. Exporting it first and counting afterwards asked
// for tens of gigabytes before rejecting them.
func TestSandboxRejectsHugeArrayWithoutAllocating(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	started := time.Now()
	_, err := NewSandbox().Run(context.Background(),
		`function extract(document) { return new Array(200000000); }`, testPage(t, listingPage))
	elapsed := time.Since(started)

	runtime.ReadMemStats(&after)

	if !errors.Is(err, ErrShape) {
		t.Fatalf("Run(new Array(2e8)) error = %v, want ErrShape", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run(new Array(2e8)) took %s, want it rejected without materialising the array", elapsed)
	}

	// Exporting two hundred million elements cost gigabytes. Rejecting on the
	// declared length costs nothing, so the allocation should be negligible.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 256<<20 {
		t.Errorf("Run(new Array(2e8)) allocated %d MiB, want it rejected before materialising",
			grew>>20)
	}
}

// TestSandboxTimeoutDuringExport checks that an interrupt landing while the
// result is being exported is still reported as a timeout. goja's own
// exception machinery does not match an interrupt, so it escapes vm.Try and is
// caught by the recover — and it must be classified, or a timeout is read as
// the artifact's fault and drives a heal that fixes nothing.
func TestSandboxTimeoutDuringExport(t *testing.T) {
	sandbox := NewSandbox()
	sandbox.Timeout = 150 * time.Millisecond

	// The getter spins, so the interrupt fires while the value is exporting
	// rather than while extract is running.
	const script = `function extract(document) {
	  return [{get title() { for (;;) {} }}];
	}`

	_, err := sandbox.Run(context.Background(), script, testPage(t, listingPage))
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("Run(spinning getter) error = %v, want ErrTimeout", err)
	}
}

// TestSandboxScalesLinearly locks in the two fixes that made the DOM affordable
// on the deployment target.
//
// Before them, a 2,000-row listing page cost 105 MiB to walk, and the
// childElementCount loop below — the shape a model reaches for by reflex — was
// quadratic at 364 MiB. Both are now a few MiB. This test fails on the shape of
// the growth rather than on an absolute number, so it does not become a
// benchmark that breaks on a faster machine.
func TestSandboxScalesLinearly(t *testing.T) {
	rows := func(n int) string {
		var b strings.Builder
		b.WriteString("<html><body><div id='c'>")
		for i := range n {
			fmt.Fprintf(&b, `<div class="row"><h2>Row %d</h2></div>`, i)
		}
		b.WriteString("</div></body></html>")
		return b.String()
	}

	// Bounded by childElementCount and indexing children — the quadratic shape.
	const script = `function extract(document) {
	  const c = document.getElementById('c');
	  const out = [];
	  for (let i = 0; i < c.childElementCount; i++) {
	    out.push({title: c.children[i].querySelector('h2').innerText});
	  }
	  return out;
	}`

	measure := func(n int) uint64 {
		t.Helper()
		page := testPage(t, rows(n))

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)

		out, err := NewSandbox().Run(context.Background(), script, page)
		runtime.ReadMemStats(&after)

		if err != nil {
			t.Fatalf("Run(%d rows) error = %v", n, err)
		}
		if len(out.Records) != n {
			t.Fatalf("Run(%d rows) returned %d records, want %d", n, len(out.Records), n)
		}
		return after.TotalAlloc - before.TotalAlloc
	}

	small := measure(500)
	large := measure(2000)

	// Four times the rows should cost about four times the memory. Quadratic
	// growth would be sixteen; anything past eight is the regression.
	if ratio := float64(large) / float64(small); ratio > 8 {
		t.Errorf("4x the rows cost %.1fx the memory (%d KiB -> %d KiB), want roughly linear",
			ratio, small>>10, large>>10)
	}
}

// TestSandboxCapsEveryQuery covers the ceiling that getElementsByTagName was
// written without: it and querySelectorAll now share one helper, so the cap
// cannot be present on one and missing on the other.
func TestSandboxCapsEveryQuery(t *testing.T) {
	var b strings.Builder
	b.WriteString("<html><body>")
	for range maxQueryResults + 10 {
		b.WriteString("<span>x</span>")
	}
	b.WriteString("</body></html>")
	page := testPage(t, b.String())

	for _, method := range []string{"querySelectorAll('span')", "getElementsByTagName('span')"} {
		t.Run(method, func(t *testing.T) {
			script := `function extract(document) { return [...document.` + method + `]; }`
			if _, err := NewSandbox().Run(context.Background(), script, page); !errors.Is(err, ErrScript) {
				t.Errorf("Run(%s over the cap) error = %v, want it refused", method, err)
			}
		})
	}
}

// TestParsePageRejectsPathologicalNesting covers CPU that no timeout can reach.
//
// html.Parse is quadratic in nesting depth and takes no context, so a deeply
// nested page holds the calling goroutine for as long as it likes. In the
// scheduler that goroutine owns the source's in-flight claim and a concurrency
// slot, neither of which is ever released — one wedged source, silently.
func TestParsePageRejectsPathologicalNesting(t *testing.T) {
	deep := strings.Repeat("<div>", 20000) + "x" + strings.Repeat("</div>", 20000)

	started := time.Now()
	_, err := ParsePage("https://example.org/", deep)
	elapsed := time.Since(started)

	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("ParsePage(20000 nested divs) error = %v, want ErrTooDeep", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("ParsePage() took %s to refuse; the guard must be cheaper than the parse", elapsed)
	}

	// Ordinary pages, including quite deeply structured ones, must be fine.
	ordinary := strings.Repeat("<div>", 60) + "x" + strings.Repeat("</div>", 60)
	if _, err := ParsePage("https://example.org/", ordinary); err != nil {
		t.Errorf("ParsePage(60 nested divs) error = %v, want it accepted", err)
	}
	if _, err := ParsePage("https://example.org/", listingPage); err != nil {
		t.Errorf("ParsePage(ordinary listing) error = %v", err)
	}
}
