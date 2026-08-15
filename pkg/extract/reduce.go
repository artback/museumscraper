package extract

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Reducer compresses a fetched page into a structural sketch small enough to
// put in a prompt.
//
// A museum listing page is commonly half a megabyte, of which the part that
// tells a model where the data is amounts to a few hundred bytes. The rest is
// inline scripts, base64 images, styling, tracking, and — most of all — the
// same row repeated two hundred times. A list of two hundred identical rows
// teaches a model nothing the first three do not, and costs context that would
// be better spent on the parts of the page that differ.
//
// What survives is chosen by one rule: keep whatever a selector could be
// written against, discard everything else.
type Reducer struct {
	// MaxTextRunes truncates each text node. Enough to recognise a title and
	// see the shape of a date; not enough to paste in an exhibition's
	// catalogue essay.
	//
	// Text is the single largest category in a reduced page, and the model is
	// being shown where data lives rather than asked to read it: a title is
	// recognisable in a few words and "3 maj 2026 – 12 september 2026" fits
	// twice over. The description a listing carries is pure cost past that.
	MaxTextRunes int

	// MaxRepeats is how many consecutive siblings of the same shape are kept
	// before the rest are summarised as a count.
	MaxRepeats int

	// MaxDepth stops descending. Sites nest wrapper divs a dozen deep around
	// content; past a point the nesting is layout rather than structure.
	MaxDepth int

	// MaxBytes caps the whole reduction, as a last defence for a page that
	// defeats every other rule.
	MaxBytes int

	// MaxClassTokens caps how many class names survive on one element.
	MaxClassTokens int

	// MaxJSONLDRunes caps an embedded JSON-LD block. These are kept where
	// everything else scriptlike is dropped, because a site that publishes
	// schema.org data is handing over exact, language-independent values and
	// an artifact that reads them is far better than one guessing at markup.
	MaxJSONLDRunes int
}

// Reducer defaults, sized for a local model with a modest context window.
const (
	DefaultMaxTextRunes   = 80
	DefaultMaxRepeats     = 3
	DefaultMaxDepth       = 18
	DefaultMaxBytes       = 24000
	DefaultMaxClassTokens = 4
	DefaultMaxJSONLDRunes = 1500
)

// NewReducer returns a Reducer with the default limits.
func NewReducer() *Reducer {
	return &Reducer{
		MaxTextRunes:   DefaultMaxTextRunes,
		MaxRepeats:     DefaultMaxRepeats,
		MaxDepth:       DefaultMaxDepth,
		MaxBytes:       DefaultMaxBytes,
		MaxClassTokens: DefaultMaxClassTokens,
		MaxJSONLDRunes: DefaultMaxJSONLDRunes,
	}
}

// Reduction is a reduced page and what it cost.
type Reduction struct {
	// Text is the sketch, as indented pseudo-HTML.
	Text string
	// OriginalBytes and ReducedBytes are the sizes either side.
	OriginalBytes int
	ReducedBytes  int
	// Truncated reports that MaxBytes was reached, so the sketch is a prefix
	// of the page rather than a summary of all of it.
	Truncated bool
}

// Ratio is how much smaller the reduction is than the page, as a multiple.
//
// It is worth reporting: a page that barely reduces is one with little
// repeated structure and a great deal of unique markup, which is a signal that
// generation will go badly and the source may be unsuitable.
func (r Reduction) Ratio() float64 {
	if r.ReducedBytes == 0 {
		return 0
	}
	return float64(r.OriginalBytes) / float64(r.ReducedBytes)
}

// String renders the ratio for a log line or a CLI.
func (r Reduction) String() string {
	return fmt.Sprintf("%d bytes reduced to %d (%.0f× smaller)",
		r.OriginalBytes, r.ReducedBytes, r.Ratio())
}

// keptAttributes are the attributes a selector can usefully be written
// against. Everything else — style, width, srcset, the dozen framework
// bookkeeping attributes a modern site emits — is noise in a prompt.
var keptAttributes = map[string]bool{
	"id": true, "class": true, "href": true, "datetime": true,
	"itemprop": true, "itemtype": true, "itemscope": true,
	"role": true, "aria-label": true, "lang": true, "type": true, "rel": true,
}

// droppedElements never survive reduction. Their content cannot be extracted
// from and their bulk is what makes pages large in the first place — an inline
// SVG icon set alone routinely outweighs a page's entire listing markup.
//
// Keyed by tag name rather than by atom because the foreign-content elements
// here, svg and its children, have no atom constants.
var droppedElements = map[string]bool{
	"style": true, "noscript": true, "svg": true, "path": true, "defs": true,
	"iframe": true, "canvas": true, "video": true, "audio": true,
	"source": true, "track": true, "link": true, "br": true, "symbol": true,

	// Chrome. A site's menu is the largest repeated structure on most pages
	// and contains no data: on one museum's listing page the reduction spent
	// its first 160 lines on head metadata and navigation, reached the actual
	// listing at line 164, and was then truncated part-way through a second
	// menu — so the model paid the full token budget to see the page's
	// furniture and only part of its content.
	//
	// A nav or a footer inside a listing entry would be unusual and would
	// carry nothing a selector needs. header is deliberately NOT here: cards
	// really do use it, and one of the live extractors keys on .card__head.
	"nav": true, "footer": true, "aside": true,
	"form": true, "select": true, "option": true, "button": true,
	"meta": true, "input": true, "label": true, "template": true,
}

// Reduce compresses a parsed page.
func (r *Reducer) Reduce(page *Page) Reduction {
	w := &sketch{
		reducer: r,
		limit:   r.MaxBytes,
	}
	w.writeNode(page.doc, 0)

	text := strings.TrimRight(w.b.String(), "\n")
	return Reduction{
		Text:          text,
		OriginalBytes: len(page.HTML),
		ReducedBytes:  len(text),
		Truncated:     w.truncated,
	}
}

// sketch accumulates the reduced rendering.
type sketch struct {
	reducer   *Reducer
	b         strings.Builder
	limit     int
	truncated bool
}

// full reports whether the byte cap has been reached, and records that it was.
func (s *sketch) full() bool {
	if s.limit > 0 && s.b.Len() >= s.limit {
		s.truncated = true
		return true
	}
	return false
}

func (s *sketch) line(depth int, format string, args ...any) {
	if s.full() {
		return
	}
	s.b.WriteString(strings.Repeat("  ", min(depth, 12)))
	fmt.Fprintf(&s.b, format, args...)
	s.b.WriteByte('\n')
}

func (s *sketch) writeNode(node *html.Node, depth int) {
	if node == nil || s.full() {
		return
	}

	switch node.Type {
	case html.DocumentNode:
		s.writeChildren(node, depth)
		return

	case html.TextNode:
		if text := collapse(node.Data); text != "" {
			s.line(depth, "%q", truncateRunes(text, s.reducer.MaxTextRunes))
		}
		return

	case html.ElementNode:
		// fall through

	default:
		// Comments, doctypes and the rest carry nothing extractable.
		return
	}

	if droppedElements[node.Data] {
		return
	}
	if node.DataAtom == atom.Script {
		s.writeScript(node, depth)
		return
	}
	// The head carries a title and, on a site that publishes it, schema.org
	// data. Everything else in it is metadata for other machines.
	if node.DataAtom == atom.Head {
		s.writeHead(node, depth)
		return
	}
	if depth > s.reducer.MaxDepth {
		s.line(depth, "<%s …>", node.Data)
		return
	}

	attributes := s.attributes(node)

	// A wrapper carrying nothing is worth nothing.
	//
	// Modern pages nest a dozen anonymous divs around every piece of content
	// for layout. Each costs a line, an indent and a tag name, and tells a
	// model nothing it can select on — one museum's listing page spent 139
	// lines on them. Where such an element has no attributes worth keeping and
	// exactly one element child, it is elided and the child takes its place.
	if attributes == "" && onlyChild(node) != nil {
		s.writeNode(onlyChild(node), depth)
		return
	}

	open := "<" + node.Data + attributes + ">"
	if node.FirstChild == nil {
		s.line(depth, "%s", open)
		return
	}
	s.line(depth, "%s", open)
	s.writeChildren(node, depth+1)
}

// writeHead keeps only the parts of a head worth extracting from.
func (s *sketch) writeHead(node *html.Node, depth int) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != html.ElementNode {
			continue
		}
		if child.DataAtom == atom.Title || child.DataAtom == atom.Script {
			s.writeNode(child, depth)
		}
	}
}

// boilerplateTypes are the schema.org types every SEO plugin emits and no
// extraction wants.
//
// A WordPress site with Yoast declares a WebSite, a CollectionPage, a
// BreadcrumbList and an ImageObject on every page. On one museum that
// boilerplate filled the entire JSON-LD budget and was truncated before
// reaching anything else — so a signal the reducer keeps precisely because it
// is exact was, in practice, pure cost.
var boilerplateTypes = map[string]bool{
	"website": true, "webpage": true, "collectionpage": true,
	"breadcrumblist": true, "listitem": true, "imageobject": true,
	"searchaction": true, "sitenavigationelement": true,
	"organization": true, "person": true, "readaction": true,
	"entrypoint": true, "webpageelement": true, "contactpoint": true,
}

// writeScript keeps schema.org declarations and drops all other script.
func (s *sketch) writeScript(node *html.Node, depth int) {
	kind, _ := attribute(node, "type")
	if !strings.Contains(strings.ToLower(kind), "ld+json") {
		return
	}
	body := strings.TrimSpace(textOf(node))
	if body == "" {
		return
	}

	// Re-marshalled where it parses, rather than quoted.
	//
	// Quoting a JSON document escapes every slash and quote inside it, which
	// roughly doubled its size — a page's own data paying twice for the
	// privilege of being unreadable. Re-marshalling also lets the boilerplate
	// be dropped, which is most of what is usually there.
	if compact, ok := usefulJSONLD(body); ok {
		if compact == "" {
			return
		}
		s.line(depth, `<script type="application/ld+json">`)
		s.line(depth+1, "%s", truncateRunes(compact, s.reducer.MaxJSONLDRunes))
		return
	}

	// Unparseable — real sites ship these with trailing commas and comments.
	// Quoted, because it is then arbitrary text rather than a document.
	s.line(depth, `<script type="application/ld+json">`)
	s.line(depth+1, "%q", truncateRunes(collapse(body), s.reducer.MaxJSONLDRunes))
}

// usefulJSONLD re-encodes a schema.org block with the SEO boilerplate removed,
// reporting whether it parsed at all.
func usefulJSONLD(body string) (string, bool) {
	var decoded any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		return "", false
	}

	kept := make([]any, 0)
	for _, node := range flattenJSONLD(decoded) {
		object, ok := node.(map[string]any)
		if !ok {
			continue
		}
		if boilerplateTypes[strings.ToLower(typeOfJSONLD(object))] {
			continue
		}
		kept = append(kept, object)
	}
	if len(kept) == 0 {
		return "", true
	}

	encoded, err := json.Marshal(kept)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// typeOfJSONLD reads a node's @type, which may be a string or a list.
func typeOfJSONLD(object map[string]any) string {
	switch value := object["@type"].(type) {
	case string:
		return value
	case []any:
		if len(value) > 0 {
			first, _ := value[0].(string)
			return first
		}
	}
	return ""
}

// flattenJSONLD unwraps arrays and @graph containers.
func flattenJSONLD(node any) []any {
	switch value := node.(type) {
	case []any:
		var out []any
		for _, item := range value {
			out = append(out, flattenJSONLD(item)...)
		}
		return out
	case map[string]any:
		if graph, ok := value["@graph"]; ok {
			return flattenJSONLD(graph)
		}
		return []any{value}
	default:
		return nil
	}
}

// writeChildren writes a node's children, collapsing runs of siblings that
// share a shape.
func (s *sketch) writeChildren(node *html.Node, depth int) {
	var (
		lastShape string
		seen      int
		skipped   int
	)

	flush := func() {
		if skipped > 0 {
			s.line(depth, "<!-- %d more %s -->", skipped, lastShape)
			skipped = 0
		}
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if s.full() {
			break
		}

		shape := shapeOf(child)
		if shape == "" {
			// Text and other unshaped nodes interrupt a run: two rows either
			// side of a heading are not two consecutive rows.
			flush()
			lastShape, seen = "", 0
			s.writeNode(child, depth)
			continue
		}

		if shape != lastShape {
			flush()
			lastShape, seen = shape, 0
		}

		seen++
		if seen <= s.reducer.MaxRepeats {
			s.writeNode(child, depth)
			continue
		}
		skipped++
	}
	flush()
}

// onlyChild returns a node's single element child when that is all it has —
// no text of its own, no siblings for the child. Anything else returns nil,
// because eliding it would lose structure.
func onlyChild(node *html.Node) *html.Node {
	var found *html.Node
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		switch child.Type {
		case html.ElementNode:
			if found != nil {
				return nil
			}
			found = child
		case html.TextNode:
			if collapse(child.Data) != "" {
				return nil
			}
		}
	}
	if found != nil && droppedElements[found.Data] {
		return nil
	}
	return found
}

// shapeOf is the signature siblings are grouped by: tag and classes, which is
// what a selector would be written against, and nothing that varies per row.
//
// It deliberately ignores the subtree. Two listing entries differing only in
// whether one carries a "sold out" badge are the same shape for a model's
// purposes, and treating them as different would defeat the collapsing on
// exactly the pages that most need it.
func shapeOf(node *html.Node) string {
	if node.Type != html.ElementNode {
		return ""
	}
	classes, _ := attribute(node, "class")
	fields := strings.Fields(classes)
	slices.Sort(fields)

	shape := node.Data
	if len(fields) > 0 {
		shape += "." + strings.Join(fields, ".")
	}
	return shape
}

// attributes renders the attributes worth keeping.
func (s *sketch) attributes(node *html.Node) string {
	var b strings.Builder
	for _, a := range node.Attr {
		key := strings.ToLower(a.Key)
		if !keptAttributes[key] && !strings.HasPrefix(key, "data-") {
			continue
		}

		value := collapse(a.Val)
		switch key {
		case "class":
			// Framework class soup is most of a modern page's markup and none
			// of its meaning. A breakpoint helper, a spacing utility and a
			// build-generated identifier are all things no selector would ever
			// be written against, so they are cost without signal.
			value = keepUsefulClasses(value, s.reducer.MaxClassTokens)
			if value == "" {
				continue
			}
		case "id":
			// Generated ids — svid12_18df5c9518529a08514622 and its cousins —
			// change on every publish and cannot be selected on.
			if volatileClass(value) {
				continue
			}
		case "href":
			// A path says what a link is for; a hundred-character query
			// string of tracking parameters does not.
			if cut, _, found := strings.Cut(value, "?"); found {
				value = cut + "?…"
			}
			value = truncateRunes(value, 100)
		default:
			value = truncateRunes(value, 80)
		}

		if value == "" {
			fmt.Fprintf(&b, " %s", key)
			continue
		}
		fmt.Fprintf(&b, " %s=%q", key, value)
	}
	return b.String()
}

// keepUsefulClasses drops the class tokens a selector would never use and caps
// how many survive.
func keepUsefulClasses(classes string, limit int) string {
	if limit <= 0 {
		limit = DefaultMaxClassTokens
	}

	kept := make([]string, 0, limit)
	for _, token := range strings.Fields(classes) {
		if volatileClass(token) || utilityClass(token) {
			continue
		}
		kept = append(kept, token)
		if len(kept) >= limit {
			break
		}
	}
	return strings.Join(kept, " ")
}

// utilityClass matches the layout and visibility helpers frameworks emit by
// the dozen: grid columns, spacing, breakpoint visibility, alignment. They
// describe how something looks, never what it is.
var utilityClass = func() func(string) bool {
	pattern := regexp.MustCompile(`^(?:` +
		`(?:col|row|grid|flex|order|offset|push|pull)(?:-[a-z0-9]+)*|` +
		`(?:m|p)[trblxy]?-\d|` +
		`(?:text|bg|border|rounded|shadow|opacity|z)-[a-z0-9-]+|` +
		`(?:w|h|min|max)-[a-z0-9-]+|` +
		`(?:d|display)-[a-z0-9-]+|` +
		`(?:sr-only|clearfix|container|wrapper|inner|outer|content|wrap)|` +
		`[a-z]+-(?:hide|show|skip|spacer|template|portlet|breakpoint|bp)[a-z0-9-]*|` +
		`[a-z]+-(?:xs|sm|md|lg|xl|xxl)(?:-[a-z0-9]+)*` +
		`)$`)
	return func(token string) bool { return pattern.MatchString(strings.ToLower(token)) }
}()

// truncateRunes shortens a string to at most n runes, marking that it was cut.
// It counts runes rather than bytes because a cut mid-rune would put invalid
// UTF-8 into a prompt, and museum listings are full of multi-byte characters.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
