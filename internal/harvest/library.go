package harvest

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/artback/museumscraper/extract"
	"museum/pkg/exhibitions"
)

// ExhibitionLibrary is the standard library generated museum extractors get.
//
// It lives here rather than in pkg/extract because every function in it is
// domain knowledge: how museum listings write dates, what a navigation link
// looks like on a museum site. pkg/extract knows how to run a script safely and
// grade what it returns, and should not also know that Swedish for May is
// "maj". The harness is where the two are joined.
//
// The functions are the ones six live generations each rewrote for themselves.
// Sharing them means the model stops paying for that, the operator stops
// reviewing six versions of it, and — the part that compounds — a fix to any of
// them reaches every extractor already in the store without regenerating one.
func ExhibitionLibrary() *extract.Library {
	return &extract.Library{
		Global: "museum",
		Helpers: []extract.Helper{
			{
				Name:      "dates",
				Signature: `dates(text) -> {start, end, permanent}`,
				Doc: "Reads a listing's run dates out of human-readable text in any of a " +
					"dozen languages: \"12 mars – 7 september 2026\", \"Until 3 Jan 2027\", " +
					"\"t.o.m. 15 januari\", \"Ongoing\". Returns ISO dates, or nulls where the " +
					"text gives none, and permanent:true for an always-on display. " +
					"Prefer a datetime attribute or JSON-LD where the page offers one; use " +
					"this for prose, and do not write your own month table.",
				Bind: bindDates,
			},
			{
				Name:      "clean",
				Signature: `clean(text) -> string`,
				Doc: "Collapses whitespace, including non-breaking spaces, and trims. " +
					"What innerText already does, for strings you have built yourself.",
				Bind: bindClean,
			},
			{
				Name:      "jsonld",
				Signature: `jsonld() -> array`,
				Doc: "Every schema.org block on this page, JSON-parsed, with @graph and " +
					"nested arrays flattened. Where a site publishes these they are exact " +
					"and language-independent, and reading them beats guessing at markup.",
				Bind: bindJSONLD,
			},
			{
				Name:      "isNavigation",
				Signature: `isNavigation(url) -> boolean`,
				Doc: "True when a link points back at the listing itself, or at a view or " +
					"page of it, rather than at an entry. Use it to drop the menu, the " +
					"pagination and the \"view all\" link.",
				Bind: bindIsNavigation,
			},
		},
	}
}

// bindDates exposes the catalogue's own multilingual date reader.
//
// This is the helper that earns the whole idea. It is 340 lines of accumulated
// knowledge about how museums write dates — ordinals, en-dashes, "jusqu'au",
// the year appearing on only one end of a range — that every generated
// extractor was otherwise reinventing badly in twenty.
func bindDates(_ *extract.Page, now time.Time) any {
	return func(text string) map[string]any {
		parsed := exhibitions.ParseDateRange(text, now)

		result := map[string]any{
			"start":     nil,
			"end":       nil,
			"permanent": parsed.Permanent,
		}
		if parsed.Start != nil {
			result["start"] = parsed.Start.Format(time.DateOnly)
		}
		if parsed.End != nil {
			result["end"] = parsed.End.Format(time.DateOnly)
		}
		return result
	}
}

func bindClean(_ *extract.Page, _ time.Time) any {
	return func(text string) string { return strings.Join(strings.Fields(text), " ") }
}

// bindJSONLD parses the page's schema.org blocks once.
//
// Flattened, because a site's declaration is as likely to be a @graph, an
// array, or a single object, and a script that has to handle all three spends
// its complexity on the wrong problem.
func bindJSONLD(page *extract.Page, _ time.Time) any {
	return func() []any {
		var flat []any

		for _, block := range jsonLDBlocks(page.HTML) {
			var decoded any
			if err := json.Unmarshal([]byte(block), &decoded); err != nil {
				// Real sites ship these with trailing commas and stray
				// comments. One unparseable block is not a reason to withhold
				// the others.
				continue
			}
			flat = append(flat, flatten(decoded)...)
		}
		return flat
	}
}

// jsonLDBlocks pulls the raw contents of every schema.org script tag.
func jsonLDBlocks(pageHTML string) []string {
	var blocks []string
	rest := pageHTML

	for {
		open := strings.Index(strings.ToLower(rest), "<script")
		if open < 0 {
			return blocks
		}
		rest = rest[open:]

		gt := strings.Index(rest, ">")
		if gt < 0 {
			return blocks
		}
		tag := strings.ToLower(rest[:gt])

		end := strings.Index(strings.ToLower(rest), "</script")
		if end < 0 {
			return blocks
		}

		if strings.Contains(tag, "ld+json") {
			blocks = append(blocks, strings.TrimSpace(rest[gt+1:end]))
		}
		rest = rest[end+len("</script"):]
	}
}

// flatten unwraps arrays and @graph containers into a flat list of objects.
func flatten(node any) []any {
	switch value := node.(type) {
	case []any:
		var out []any
		for _, item := range value {
			out = append(out, flatten(item)...)
		}
		return out

	case map[string]any:
		if graph, ok := value["@graph"]; ok {
			return flatten(graph)
		}
		return []any{value}

	default:
		return nil
	}
}

// bindIsNavigation exposes the catalogue's navigation heuristic, judged
// against the page being read.
func bindIsNavigation(page *extract.Page, _ time.Time) any {
	base, err := url.Parse(page.URL)
	if err != nil {
		base = nil
	}
	return func(link string) bool {
		if base == nil {
			return false
		}
		return exhibitions.IsNavigationLink(link, base)
	}
}
