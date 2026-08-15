# extract

Compile a web page and a declared schema into a durable, executable extractor —
then keep it working as the page changes.

A language model reads a page **once** and writes a JavaScript extractor for it.
Every run after that executes the stored script with no model involved. The
model is a compiler, not a runtime, and is re-invoked only when a run has
produced evidence the script has stopped working.

```bash
go get github.com/artback/museumscraper/extract
```

## Why

Hand-written extraction is tedious to write, worse to maintain, and fails
silently: a site rearranges its markup, the selectors stop matching, and an
empty result is indistinguishable from a page that genuinely has nothing on it.
Running a model against every page on every run fixes the brittleness but is
slow, expensive, non-deterministic and unauditable.

Separating *designing* an extractor from *running* one gets both: minutes of
model time once per source, microseconds per run thereafter, and a stored
artifact a person can read and diff.

## The steady-state path

```go
page, err := extract.ParsePage(url, body)
output, err := extract.NewSandbox().Run(ctx, artifact.Script, page)

assessment := (&extract.Validator{}).Validate(ctx, source, output.Records, history)
if assessment.Publishable() {
    publish(assessment.Records)
}
```

No model, no network, single-digit milliseconds. See the
[examples](https://pkg.go.dev/github.com/artback/museumscraper/extract#pkg-examples)
for compilation, validation and reuse.

## Generated code cannot do anything but read the page

Scripts run in a bare [goja](https://github.com/dop251/goja) interpreter holding
a read-only DOM and nothing else. There is no `fetch`, no `XMLHttpRequest`, no
`require`, no `process`, no timers and no storage — not filtered out, but never
installed. `TestSandboxHasNoCapabilities` asserts each is absent.

Execution is bounded by a wall-clock interrupt, a cap on elements reached, and a
cap on records returned. No panic escapes `Sandbox.Run`, including one raised by
an accessor property on a returned object — which is an ordinary mistake for a
model to make and, unguarded, kills the host process.

## Nothing is published unless it is trustworthy

Every run is graded `pass`, `suspect` or `fail`, cheapest check first:

| Rung | Catches |
| --- | --- |
| Structural | Output missing, malformed, or with required fields empty |
| Volumetric | A count out of character with this source's own history |
| Semantic | Values that parse but are implausible — placeholders, dates far out |
| Model-judged | Optional; the only rung that can cost an invocation |

The volumetric rung is the one that earns its keep. A page that drops from 200
rows to 3 returns three perfectly well-formed records, and structural validity
alone calls that a success.

Rules are declared by the operator, never by the model. A model that both
extracted the data and defined what counted as plausible would grade its own
homework.

## Self-healing

A failing run alone does not authorise regeneration. `Fingerprint` hashes the
*set* of structural paths in a page, so it is insensitive to how much content is
on it; `ShouldHeal` reads a failure against that signal:

| Verdict | Structure | Response |
| --- | --- | --- |
| `fail` | either | Regenerate once, then re-validate |
| `suspect` | changed | Regenerate once — a partial break |
| `suspect` | unchanged | Hold and report; more likely a quiet week |
| `pass` | either | Publish |

`HealPolicy` caps regeneration so a permanently dead source cannot burn a model
invocation on every schedule tick.

## Extending it

`Library` adds pure functions for generated scripts to call — date parsing, text
cleaning, whatever a domain repeatedly needs. Written once and improved once: a
fix reaches every artifact already generated, without regenerating any.

Everything in a `Library` must be a pure function of its arguments and the page.
A helper that fetched or wrote anything would put back what the sandbox exists
to keep out.

## What this does not do

- **No I/O.** Fetching pages, storing artifacts and calling a model are yours.
  `Model` is a two-method interface you implement against your own client.
- **No JavaScript rendering.** A listing that exists only after client-side
  rendering is invisible. goja is not a browser.
- **No pagination or multi-page traversal.** An artifact sees one page.
- **A catastrophically backtracking regular expression can outrun the timeout**,
  because goja routes some patterns to an engine that does not poll its
  interrupt flag. Bound the work you give it.
- **Cross-site reuse is mostly unsafe.** Measured across six real sites, an
  extractor written for one validated on another while extracting a quarter of
  the records. `Similarity` exists to gate that; validation alone does not.

## Dependencies

`goja`, `cascadia`, `golang.org/x/net/html`. Nothing else.
