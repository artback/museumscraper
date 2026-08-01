package exhibitions

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Reading schema.org data is the one part of this scraper that does not have to
// guess.
//
// Everything else here infers an exhibition from markup written for people: a
// link's depth, the words around it, the shape of its path. That inference is
// what the rest of the package is, and it is language-bound at every step — the
// month table knows ten languages, the type labels a dozen, and a museum
// writing in Czech or Turkish falls outside all of them.
//
// A site that publishes JSON-LD has already answered the questions. It says
// this is an ExhibitionEvent, its name is this, it runs from this date to that
// one, in ISO-8601. None of that changes with the language the page is written
// in or the way the page is laid out, so where it exists it is believed over
// anything read off the page.
//
// Not many sites publish it — in a survey of thirty-four museums across nine
// countries, three did — but where they do it is exact, and it costs nothing:
// the page has already been fetched for the other reader.

// eventTypes are the schema.org types that describe something on show.
//
// ExhibitionEvent and VisualArtsEvent name an exhibition outright and are
// trusted on their own. Plain Event does not — it is what a site uses for
// concerts, tours and late openings as well — so it is accepted only when the
// URL agrees, which is the same test the HTML reader applies.
var eventTypes = map[string]bool{
	"exhibitionevent": true,
	"visualartsevent": true,
	"event":           false,
}

// ExtractJSONLDCandidates returns the exhibitions a page declares in
// schema.org JSON-LD.
//
// Malformed blocks are skipped rather than reported: a site that publishes
// broken JSON-LD is still readable by the HTML path, and one bad block should
// not cost the good ones on the same page.
func ExtractJSONLDCandidates(pageHTML string, base *url.URL) []Candidate {
	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return nil
	}

	var found []Candidate
	seen := make(map[string]struct{})

	for _, block := range jsonLDBlocks(doc) {
		var parsed any
		if err := json.Unmarshal([]byte(block), &parsed); err != nil {
			continue
		}
		walkJSONLD(parsed, func(node map[string]any) {
			candidate, ok := candidateFromJSONLD(node, base)
			if !ok {
				return
			}
			if _, dup := seen[candidate.URL]; dup {
				return
			}
			seen[candidate.URL] = struct{}{}
			found = append(found, candidate)
		})
	}
	return found
}

// candidatesOn returns everything a listing page offers, from both readers.
//
// The declared events come first and win ties, because a site that publishes
// them has stated what the HTML reader can only infer. The HTML reader still
// runs on the same page: sites routinely declare a handful of events and list
// thirty, and reading only the declaration would silently lose the rest.
func candidatesOn(pageHTML string, base *url.URL, section string) []Candidate {
	declared := ExtractJSONLDCandidates(pageHTML, base)

	seen := make(map[string]struct{}, len(declared))
	for _, candidate := range declared {
		seen[candidate.URL] = struct{}{}
	}

	found := declared
	for _, candidate := range ExtractCandidatesUnder(pageHTML, base, section) {
		if _, dup := seen[candidate.URL]; dup {
			continue
		}
		found = append(found, candidate)
	}
	return found
}

// jsonLDBlocks returns the contents of every JSON-LD script on the page.
func jsonLDBlocks(root *html.Node) []string {
	var blocks []string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "script" &&
			strings.Contains(strings.ToLower(attr(n, "type")), "ld+json") {
			var b strings.Builder
			for child := n.FirstChild; child != nil; child = child.NextSibling {
				if child.Type == html.TextNode {
					b.WriteString(child.Data)
				}
			}
			if text := strings.TrimSpace(b.String()); text != "" {
				blocks = append(blocks, text)
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)

	return blocks
}

// walkJSONLD calls fn for every object in a decoded document.
//
// It descends into everything rather than following the specific containers,
// because sites nest events differently — inside "@graph", inside an ItemList's
// "itemListElement", inside a "subEvent", or at the top level as a bare array —
// and the shape that matters is the object itself, wherever it sits.
func walkJSONLD(node any, fn func(map[string]any)) {
	switch value := node.(type) {
	case map[string]any:
		fn(value)
		for _, child := range value {
			walkJSONLD(child, fn)
		}
	case []any:
		for _, child := range value {
			walkJSONLD(child, fn)
		}
	}
}

// candidateFromJSONLD reads one schema.org object, if it is an exhibition.
func candidateFromJSONLD(node map[string]any, base *url.URL) (Candidate, bool) {
	trusted, isEvent := eventType(node["@type"])
	if !isEvent {
		return Candidate{}, false
	}

	// Unescaped here and not in cleanTitle, because every other title reaches
	// it through the HTML parser, which has already done this. A JSON-LD name
	// has not been through it, and sites escape the value anyway: the V&A's
	// declared name is "Wallace &amp; Gromit", which left a second copy of the
	// exhibition in the results because it no longer matched the title read
	// from the page.
	title := cleanTitle(html.UnescapeString(jsonLDString(node["name"])))
	if len([]rune(title)) < 4 || len(title) > 400 {
		return Candidate{}, false
	}

	link := jsonLDString(node["url"])
	if link == "" {
		link = jsonLDString(node["@id"])
	}
	resolved, ok := resolveURL(base, link)
	if !ok {
		return Candidate{}, false
	}
	parsed, err := url.Parse(resolved)
	if err != nil || !strings.EqualFold(parsed.Host, base.Host) {
		return Candidate{}, false
	}

	if !trusted && !containsAny(strings.ToLower(parsed.Path), exhibitionPathHints) {
		return Candidate{}, false
	}
	// The talks and tours are declared with the same markup as the
	// exhibitions, and the name is where a listing says which it is.
	if isNonExhibition(title) {
		return Candidate{}, false
	}

	dates := DateRange{
		Start: jsonLDDate(node["startDate"]),
		End:   jsonLDDate(node["endDate"]),
	}
	if description := jsonLDString(node["description"]); dates.IsZero() && namesPermanent(description) {
		dates.Permanent = true
	}

	return Candidate{
		Title:   title,
		URL:     resolved,
		Context: title,
		Dates:   dates,
	}, true
}

// eventType reports whether a "@type" value names something on show, and
// whether the type is specific enough to be trusted without corroboration.
// The value may be a single string or a list, since an object may declare
// several types.
func eventType(value any) (trusted, isEvent bool) {
	switch typed := value.(type) {
	case string:
		specific, known := eventTypes[strings.ToLower(strings.TrimSpace(typed))]
		return specific, known
	case []any:
		for _, entry := range typed {
			if specific, known := eventType(entry); known {
				// Any specific type on the object settles it.
				if specific {
					return true, true
				}
				trusted, isEvent = false, true
			}
		}
		return trusted, isEvent
	}
	return false, false
}

// jsonLDString reads a value that may be a plain string, a language-tagged
// object, or a list of either. Language tagging is common on multilingual
// museum sites, which is much of the point of reading this at all.
func jsonLDString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		for _, key := range []string{"@value", "name", "@id"} {
			if text := jsonLDString(typed[key]); text != "" {
				return text
			}
		}
	case []any:
		for _, entry := range typed {
			if text := jsonLDString(entry); text != "" {
				return text
			}
		}
	}
	return ""
}

// jsonLDDate reads an ISO-8601 date or date-time, which is the only form
// schema.org permits — so this is where the language-independence comes from.
func jsonLDDate(value any) *time.Time {
	text := jsonLDString(value)
	if len(text) < 10 {
		return nil
	}
	when, err := time.Parse("2006-01-02", text[:10])
	if err != nil {
		return nil
	}
	return &when
}
