package harvest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/artback/museumscraper/extract"
)

// libraryRun executes a script with the exhibitions library installed, at a
// fixed instant so date helpers are reproducible.
func libraryRun(t *testing.T, page, script string) extract.Output {
	t.Helper()

	parsed, err := extract.ParsePage("https://example.org/utstallningar/", page)
	if err != nil {
		t.Fatalf("ParsePage() error = %v", err)
	}

	sandbox := extract.NewSandbox()
	sandbox.Library = ExhibitionLibrary()
	sandbox.Now = func() time.Time { return time.Date(2026, time.August, 15, 0, 0, 0, 0, time.UTC) }

	out, err := sandbox.Run(context.Background(), script, parsed)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return out
}

const emptyPage = `<html><body><p>nothing here</p></body></html>`

// TestLibraryGrantsNoCapability is the test that keeps the standard library
// from quietly undoing the sandbox.
//
// The isolation argument is that a generated script has no way to reach the
// network, the host or anything that persists. Adding Go functions to the
// interpreter is exactly how that argument gets lost, so every helper must be a
// pure function of its arguments and the page — and nothing reachable through
// the library object may be otherwise.
func TestLibraryGrantsNoCapability(t *testing.T) {
	forbidden := []string{
		"fetch", "XMLHttpRequest", "require", "process", "setTimeout",
		"localStorage", "child_process", "eval_file", "Deno", "Bun",
	}
	for _, name := range forbidden {
		t.Run(name, func(t *testing.T) {
			script := `function extract(document) { return [{present: typeof ` + name + ` !== "undefined"}]; }`
			out := libraryRun(t, emptyPage, script)
			if present, _ := out.Records[0]["present"].(bool); present {
				t.Errorf("%s became reachable once the library was installed", name)
			}
		})
	}

	// The library object itself must expose only the declared helpers, and
	// none of them may hand back something with further reach.
	const inspect = `function extract(document) {
	  const names = Object.keys(museum).sort().join(",");
	  const kinds = Object.keys(museum).map(k => typeof museum[k]).join(",");
	  return [{names: names, kinds: kinds}];
	}`

	out := libraryRun(t, emptyPage, inspect)
	names, _ := out.Records[0].String("names")
	kinds, _ := out.Records[0].String("kinds")

	if names != "clean,dates,isNavigation,jsonld" {
		t.Errorf("museum exposes %q, want exactly the declared helpers", names)
	}
	if strings.Trim(strings.ReplaceAll(kinds, "function", ""), ",") != "" {
		t.Errorf("museum exposes non-function members: %q", kinds)
	}
}

// TestLibraryHelperFailureIsCatchable checks that a helper given nonsense
// throws in JavaScript rather than panicking out of the sandbox. The helpers
// are Go, and Go panics on things JavaScript shrugs at.
func TestLibraryHelperFailureIsCatchable(t *testing.T) {
	const script = `function extract(document) {
	  let threw = false;
	  try { museum.dates(); } catch (e) { threw = true; }
	  try { museum.clean(null, 1, 2, 3); } catch (e) { threw = true; }
	  try { museum.isNavigation({}); } catch (e) { threw = true; }
	  return [{survived: true, threw: threw}];
	}`

	out := libraryRun(t, emptyPage, script)
	if survived, _ := out.Records[0]["survived"].(bool); !survived {
		t.Error("the script did not survive calling helpers with bad arguments")
	}
}

func TestLibraryDates(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		start     string
		end       string
		permanent bool
	}{
		{
			// The Swedish case every generated extractor hand-rolled, and the
			// one the shared parser used to fail: "maj" was not in its table.
			name: "Swedish range", text: "3 maj 2026 – 12 september 2026",
			start: "2026-05-03", end: "2026-09-12",
		},
		{name: "Swedish open end", text: "t.o.m. 15 januari 2027", end: "2027-01-15"},
		{name: "English", text: "12 March – 7 September 2026", start: "2026-03-12", end: "2026-09-07"},
		{name: "English until", text: "Until 3 Jan 2027", end: "2027-01-03"},
		{name: "German", text: "bis 3. Januar 2027", end: "2027-01-03"},
		{name: "permanent", text: "Permanent utställning", permanent: true},
		{name: "ongoing", text: "Ongoing", permanent: true},
		{name: "nothing", text: "Read more about this show"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := `function extract(document) {
			  const d = museum.dates(` + quoteJS(tt.text) + `);
			  return [{start: d.start, end: d.end, permanent: d.permanent}];
			}`

			record := libraryRun(t, emptyPage, script).Records[0]
			start, _ := record["start"].(string)
			end, _ := record["end"].(string)
			permanent, _ := record["permanent"].(bool)

			if start != tt.start || end != tt.end || permanent != tt.permanent {
				t.Errorf("museum.dates(%q) = {%q, %q, %t}, want {%q, %q, %t}",
					tt.text, start, end, permanent, tt.start, tt.end, tt.permanent)
			}
		})
	}
}

func TestLibraryJSONLD(t *testing.T) {
	const page = `<html><head>
	<script type="application/ld+json">
	{"@context":"https://schema.org","@graph":[
	  {"@type":"ExhibitionEvent","name":"Silk Roads","startDate":"2026-10-12"},
	  {"@type":"Organization","name":"Example Museum"}]}
	</script>
	<script type="application/ld+json">{"@type":"ExhibitionEvent","name":"Bronze Age"}</script>
	<script type="application/ld+json">{ not json at all, </script>
	<script>var tracking = 1;</script>
	</head><body></body></html>`

	const script = `function extract(document) {
	  return museum.jsonld()
	    .filter(n => n['@type'] === 'ExhibitionEvent')
	    .map(n => ({title: n.name, url: 'https://example.org/' + n.name}));
	}`

	out := libraryRun(t, page, script)
	if len(out.Records) != 2 {
		t.Fatalf("museum.jsonld() yielded %d exhibition events, want 2: %+v", len(out.Records), out.Records)
	}
	// A @graph, a bare object and an unparseable block, all handled: the
	// script did not have to know which shape the site used.
	got, _ := out.Records[0].String("title")
	if got != "Silk Roads" {
		t.Errorf("first event = %q, want %q", got, "Silk Roads")
	}
}

func TestLibraryIsNavigation(t *testing.T) {
	const script = `function extract(document) {
	  return [{
	    self: museum.isNavigation("https://example.org/utstallningar/"),
	    paged: museum.isNavigation("https://example.org/utstallningar/page/2/"),
	    entry: museum.isNavigation("https://example.org/utstallningar/silk-roads/"),
	  }];
	}`

	record := libraryRun(t, emptyPage, script).Records[0]
	if self, _ := record["self"].(bool); !self {
		t.Error("the listing page itself was not recognised as navigation")
	}
	if entry, _ := record["entry"].(bool); entry {
		t.Error("an entry was wrongly called navigation")
	}
}

// quoteJS renders a Go string as a JavaScript literal.
func quoteJS(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
