package extract_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/artback/museumscraper/extract"
)

// samplePage stands in for something a fetcher returned.
const samplePage = `<!doctype html>
<html><body>
  <ul class="events">
    <li class="event">
      <h3>Bronze Age Britain</h3>
      <a href="/events/bronze-age">Find out more</a>
      <time datetime="2026-09-01">1 September</time>
    </li>
    <li class="event">
      <h3>Silk Roads</h3>
      <a href="/events/silk-roads">Find out more</a>
      <time datetime="2026-10-12">12 October</time>
    </li>
  </ul>
</body></html>`

// storedArtifact is what a model produced for that page on some earlier day.
// Running it costs nothing and involves no model.
const storedArtifact = `function extract(document) {
  return [...document.querySelectorAll('li.event')].map(row => ({
    title: row.querySelector('h3').innerText,
    url:   row.querySelector('a').href,
    start: row.querySelector('time').getAttribute('datetime'),
  }));
}`

// sampleSchema is written by the operator and never by the model. It is what
// makes a wrong extraction detectable: the model decides how to read a page,
// and this decides what a correct reading looks like.
func sampleSchema() extract.Schema {
	return extract.Schema{
		Name:   "events",
		Intent: "the events currently listed on this site",
		Fields: []extract.Field{
			{
				Name: "title", Kind: extract.KindString, Required: true,
				Description: "the event's own name",
				// The exact wrong answers this kind of page invites: the
				// button label sitting next to every title, collected instead
				// of it when a selector is aimed one element too high.
				Rules: extract.Rules{
					MinLength:    2,
					Placeholders: []string{"Find out more", "Read more", "Book now"},
				},
			},
			{Name: "url", Kind: extract.KindURL, Required: true},
			{Name: "start", Kind: extract.KindDate},
		},
	}
}

// Example runs a stored extractor against a freshly fetched page and grades
// what it produced — the steady-state path, which involves no model at all.
func Example() {
	page, err := extract.ParsePage("https://example.org/events", samplePage)
	if err != nil {
		log.Fatal(err)
	}

	output, err := extract.NewSandbox().Run(context.Background(), storedArtifact, page)
	if err != nil {
		log.Fatal(err)
	}

	source := extract.Source{
		Name:   "example",
		URL:    "https://example.org/events",
		Schema: sampleSchema(),
		Expect: extract.Expectation{MinRecords: 1},
	}

	// Complete says the history was read successfully; this source simply has
	// none yet, which is different from a history that could not be read.
	assessment := (&extract.Validator{}).Validate(
		context.Background(), source, output.Records, extract.History{Complete: true})

	fmt.Println("verdict:", assessment.Verdict)
	for _, record := range assessment.Records {
		title, _ := record.String("title")
		url, _ := record.String("url")
		fmt.Printf("%s — %s\n", title, url)
	}

	// Output:
	// verdict: pass
	// Bronze Age Britain — https://example.org/events/bronze-age
	// Silk Roads — https://example.org/events/silk-roads
}

// ExampleValidator_Validate shows the check that gives this package its point.
// Every record below is individually perfect; the page has simply stopped
// listing all but a handful of them, and only a count compared against the
// source's own history catches that.
func ExampleValidator_Validate() {
	source := extract.Source{
		Name:   "example",
		URL:    "https://example.org/events",
		Schema: sampleSchema(),
		Expect: extract.Expectation{MinRecords: 1, Tolerance: 0.5},
	}

	records := []extract.Record{
		{"title": "Bronze Age Britain", "url": "https://example.org/a"},
	}

	// This source has returned about two hundred records on every recent run.
	history := extract.History{Counts: []int{200, 198, 205}, Complete: true}

	assessment := (&extract.Validator{}).Validate(
		context.Background(), source, records, history)

	fmt.Println("verdict:", assessment.Verdict)
	fmt.Println("publishable:", assessment.Publishable())

	// Output:
	// verdict: suspect
	// publishable: false
}

// ExampleSandbox_Run shows what a generated script cannot do. None of these
// exist in the interpreter, so none of them can be reached — the isolation
// rests on what was never installed rather than on a filter.
func ExampleSandbox_Run() {
	page, err := extract.ParsePage("https://example.org/", "<html><body></body></html>")
	if err != nil {
		log.Fatal(err)
	}

	const probe = `function extract(document) {
	  return ["fetch", "XMLHttpRequest", "require", "process", "setTimeout"]
	    .map(name => ({title: name, reachable: eval("typeof " + name) !== "undefined"}));
	}`

	output, err := extract.NewSandbox().Run(context.Background(), probe, page)
	if err != nil {
		log.Fatal(err)
	}
	for _, record := range output.Records {
		title, _ := record.String("title")
		fmt.Printf("%s reachable: %v\n", title, record["reachable"])
	}

	// Output:
	// fetch reachable: false
	// XMLHttpRequest reachable: false
	// require reachable: false
	// process reachable: false
	// setTimeout reachable: false
}

// ExampleReducer_Reduce shows what a page looks like by the time a model sees
// it. A listing of two hundred identical rows teaches a model nothing the
// first few do not, so the rest are collapsed to a count.
func ExampleReducer_Reduce() {
	var b strings.Builder
	b.WriteString(`<html><head><title>Events</title>`)
	b.WriteString(`<style>` + strings.Repeat(".event{color:red}", 100) + `</style></head>`)
	b.WriteString(`<body><nav><a href="/">Home</a></nav><ul class="events">`)
	for i := range 200 {
		fmt.Fprintf(&b, `<li class="event"><h3>Event %d</h3></li>`, i)
	}
	b.WriteString(`</ul></body></html>`)

	page, err := extract.ParsePage("https://example.org/events", b.String())
	if err != nil {
		log.Fatal(err)
	}

	reduced := extract.NewReducer().Reduce(page)
	fmt.Println(strings.Contains(reduced.Text, "197 more li.event"))
	fmt.Println("kept the stylesheet:", strings.Contains(reduced.Text, "color:red"))
	fmt.Println("kept the navigation:", strings.Contains(reduced.Text, "Home"))

	// Output:
	// true
	// kept the stylesheet: false
	// kept the navigation: false
}

// ExampleSimilarity shows the gate that decides whether one site's extractor
// may be tried on another's page. Validation alone is not a safe gate — an
// extractor can validate on a page it does not understand while missing most
// of it — so reuse is gated on structure instead.
func ExampleSimilarity() {
	listing := func(class, tag string) string {
		var b strings.Builder
		b.WriteString(`<html><body><ul class="` + class + `">`)
		for i := range 50 {
			fmt.Fprintf(&b, `<%s class="row"><h3>Item %d</h3></%s>`, tag, i, tag)
		}
		b.WriteString(`</ul></body></html>`)
		return b.String()
	}

	same, _ := extract.ParsePage("https://example.org/a", listing("events", "li"))
	// The same site with different content: nothing structural changed.
	other, _ := extract.ParsePage("https://example.org/b", listing("events", "li"))

	fmt.Println("same shape:", extract.Similarity(same, other) == 1)

	// Output:
	// same shape: true
}

// ExampleGenerator_Generate shows compilation. The model here is a stub; in
// use it would be a language model, and it is called once per source rather
// than once per run.
func ExampleGenerator_Generate() {
	page, err := extract.ParsePage("https://example.org/events", samplePage)
	if err != nil {
		log.Fatal(err)
	}

	generator := &extract.Generator{
		Model: modelReturning(storedArtifact),
		Now:   func() time.Time { return time.Date(2026, time.August, 15, 0, 0, 0, 0, time.UTC) },
	}

	source := extract.Source{
		Name:   "example",
		URL:    "https://example.org/events",
		Schema: sampleSchema(),
		Expect: extract.Expectation{MinRecords: 1},
	}

	// The returned artifact has already been run against the page that
	// produced it and has passed validation. One that has not is never
	// returned, so a caller cannot forget to trial before storing.
	artifact, report, err := generator.Generate(context.Background(), source, page)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("version:", artifact.Version)
	fmt.Println("attempts:", artifact.Provenance.Attempts)
	fmt.Println("records in trial:", report.Attempts[0].Records)

	// Output:
	// version: 1
	// attempts: 1
	// records in trial: 2
}

// modelReturning is a Model that always answers with the same script, wrapped
// in the JSON envelope the generator requires.
func modelReturning(script string) extract.Model {
	return modelFunc(func(context.Context, string, string) (string, error) {
		encoded, err := json.Marshal(map[string]string{"script": script})
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}

type modelFunc func(ctx context.Context, system, user string) (string, error)

func (f modelFunc) Complete(ctx context.Context, system, user string) (string, error) {
	return f(ctx, system, user)
}
