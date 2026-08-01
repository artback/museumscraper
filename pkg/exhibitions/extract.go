package exhibitions

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// listingPaths are the paths museum sites conventionally use for their
// programme, tried in order when no link on the home page points at one.
var listingPaths = []string{
	"/whats-on", "/what-s-on", "/exhibitions", "/exhibition",
	"/en/whats-on", "/en/what-s-on", "/en/exhibitions", "/en/exhibition",
	"/calendar/exhibitions", "/visit/exhibitions", "/programme", "/program",
	"/en/programme", "/en/program", "/events", "/en/events",
	"/ausstellungen", "/expositions", "/esposizioni", "/exposiciones",
	"/tentoonstellingen", "/utstillinger", "/wystawy",
}

// listingLinkWords identify a link to the programme, in the languages that
// appear most on European and North American museum sites.
var listingLinkWords = []string{
	"what's on", "whats on", "what’s on", "exhibition", "exhibitions",
	"on view", "on display", "current", "programme", "program", "calendar",
	"ausstellung", "ausstellungen", "veranstaltungen",
	"exposition", "expositions", "agenda",
	"esposizioni", "mostre", "exposiciones", "exposições",
	"tentoonstelling", "tentoonstellingen", "utstilling", "wystawy",
}

// strongPathHints name a URL as an exhibition outright. A site that uses these
// is telling us exactly which of its entries are exhibitions.
var strongPathHints = []string{
	"exhibition", "ausstellung", "exposition", "mostra", "mostre",
	"exposicion", "exposicao", "tentoonstelling", "on-view", "utstilling",
	// Norwegian "utstilling" was here and Swedish "utstallning" was not, so
	// Göteborgs stadsmuseum — which files every exhibition under
	// /utstallningar/ — scored zero and its programme was never read. Both
	// spellings of each word, because a site may or may not fold the accent
	// out of its URLs.
	"utstallning", "utställning", "udstilling", "nayttely", "näyttely",
	"wystawa", "vystava", "výstava", "kiallitas", "kiállítás", "sergi",
	"izlozba", "izložba", "razstava", "naroda", "vystavka", "выставка",
}

// weakPathHints name a URL as some kind of programme entry, which may or may
// not be an exhibition. Sites that separate the two — the Royal Academy files
// exhibitions under /exhibition/ and life-drawing classes under /event/ — make
// these unreliable, so they are only used when a page offers nothing stronger.
var weakPathHints = []string{
	"whats-on", "what-s-on", "display", "event", "programme", "program",
	"calendar", "agenda",
}

// exhibitionPathHints is every hint, used when scoring links to a programme
// index rather than to an individual entry.
var exhibitionPathHints = append(append([]string{}, strongPathHints...), weakPathHints...)

// skipLinkWords mark navigation, commerce and boilerplate that sits alongside
// the listings and would otherwise be read as exhibitions.
var skipLinkWords = []string{
	"cookie", "privacy", "newsletter", "subscribe", "donate", "membership",
	"shop", "tickets", "book now", "buy", "gift", "search", "menu",
	"accessibility", "terms", "contact", "press", "jobs", "careers",
	"view all", "see all", "show all", "load more", "next", "previous",
	"filter", "sort", "sign in", "log in", "register", "français", "deutsch",
}

// whitespaceRe collapses runs of whitespace in extracted text.
var whitespaceRe = regexp.MustCompile(`\s+`)

// Candidate is a possible exhibition pulled out of a listing page.
type Candidate struct {
	Title string
	URL   string
	// Context is the text surrounding the link, which is where the dates
	// usually live.
	Context string
}

// FindListingLinks returns the URLs on a page that look like they lead to the
// museum's programme, most promising first.
func FindListingLinks(pageHTML string, base *url.URL) []string {
	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return nil
	}

	type scored struct {
		url   string
		score int
	}
	var found []scored
	seen := make(map[string]struct{})

	forEachAnchor(doc, func(href, text string) {
		resolved, ok := resolveURL(base, href)
		if !ok {
			return
		}
		if _, dup := seen[resolved]; dup {
			return
		}

		lower := strings.ToLower(text)
		score := 0
		for _, word := range listingLinkWords {
			if strings.Contains(lower, word) {
				score += 2
				break
			}
		}
		parsed, err := url.Parse(resolved)
		if err != nil {
			return
		}
		for _, hint := range exhibitionPathHints {
			if strings.Contains(strings.ToLower(parsed.Path), hint) {
				score++
				break
			}
		}
		if score == 0 {
			return
		}
		// A listing index is shallow; deep paths are individual exhibitions.
		if depth := strings.Count(strings.Trim(parsed.Path, "/"), "/"); depth <= 1 {
			score++
		}

		seen[resolved] = struct{}{}
		found = append(found, scored{url: resolved, score: score})
	})

	for i := 1; i < len(found); i++ {
		for j := i; j > 0 && found[j].score > found[j-1].score; j-- {
			found[j], found[j-1] = found[j-1], found[j]
		}
	}

	urls := make([]string, 0, len(found))
	for _, f := range found {
		urls = append(urls, f.url)
	}
	return urls
}

// ExtractCandidates pulls possible exhibitions out of a listing page.
//
// Museum sites share no markup conventions and almost none publish schema.org
// event data, so the signal used here is structural: a link that goes deeper
// into the site on an exhibition-shaped path, whose own text is long enough to
// be a title, with date text on or near it.
// viewSegments name a way of looking at a listing rather than a thing in it.
//
// Short and mostly language-neutral by design. The alternative — rejecting
// titles like "Nästa Evenemang", "Föregående Evenemang", "Evenemang in Lista
// View" — is a list of phrases that has to grow with every country the
// catalogue covers and is always behind. These are the paths calendar plugins
// use, and they are far fewer than the words their buttons are labelled with.
var viewSegments = map[string]bool{
	"list": true, "lista": true, "liste": true, "listing": true,
	"page": true, "sida": true, "seite": true, "pagina": true, "paged": true,
	"month": true, "week": true, "day": true, "today": true,
	"upcoming": true, "past": true, "archive": true, "all": true,
	"calendar": true, "kalender": true, "grid": true, "map": true, "photo": true,
	"elenco": true, "liste-view": true, "agenda": true,
}

// containerSegments name the section a programme lives in, rather than
// anything in it. A path made only of these and viewSegments is a listing;
// one that adds a name is an entry — "/events/list/" against
// "/events/women-behind-the-camera/".
var containerSegments = map[string]bool{
	"event": true, "events": true, "evenemang": true, "eventi": true,
	"veranstaltungen": true, "evenementen": true, "evenements": true,
	"exhibition": true, "exhibitions": true, "utstallningar": true,
	"ausstellungen": true, "mostre": true, "expositions": true,
	"whats-on": true, "what-s-on": true, "programme": true, "program": true,
	"programs": true, "programmes": true, "kalendarium": true,
}

// IsNavigationLink reports whether a link is a way of paging through a listing
// rather than an entry in it.
//
// A real exhibition link goes deeper than the page it sits on: it adds a
// segment naming the exhibition. Paging and view-switching links add nothing
// but a page number or a view name, or only a query string. Testing that shape
// catches "Nästa Evenemang", "/event/lista/", "/events/lista/sida/2/" and
// "?eventDisplay=list" together, in any language, without knowing what any of
// the words mean.
func IsNavigationLink(candidate string, base *url.URL) bool {
	if base == nil {
		return false
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return false
	}
	if parsed.Host != "" && base.Host != "" && !strings.EqualFold(parsed.Host, base.Host) {
		return false
	}

	basePath := strings.Trim(base.Path, "/")
	linkPath := strings.Trim(parsed.Path, "/")

	// The same page, differing only by query or fragment.
	if linkPath == basePath {
		return true
	}
	// Somewhere else entirely: not this listing's navigation.
	if basePath != "" && !strings.HasPrefix(linkPath, basePath+"/") {
		return false
	}

	rest := linkPath
	if basePath != "" && strings.HasPrefix(linkPath, basePath+"/") {
		rest = strings.TrimPrefix(linkPath, basePath)
	} else if basePath != "" {
		// A different section of the same site. Judged on its own path below:
		// museums commonly link "/whats-on/" to a calendar living at
		// "/events/list/", which is still a listing and still not an entry.
		rest = linkPath
	}

	segments := strings.FieldsFunc(rest, func(r rune) bool { return r == '/' })
	if len(segments) == 0 {
		return true
	}
	for _, segment := range segments {
		lower := strings.ToLower(segment)
		if viewSegments[lower] || containerSegments[lower] {
			continue
		}
		if _, err := strconv.Atoi(segment); err == nil {
			continue // a page number
		}
		return false // names something, so it is a candidate
	}
	return true
}

// repeatedLinkTexts returns the anchor texts a page uses more than once.
func repeatedLinkTexts(root *html.Node) map[string]bool {
	counts := make(map[string]int)
	forEachAnchor(root, func(_, text string) {
		cleaned := strings.ToLower(strings.TrimSpace(cleanTitle(text)))
		if cleaned != "" {
			counts[cleaned]++
		}
	})

	repeated := make(map[string]bool, len(counts))
	for text, n := range counts {
		if n > 1 {
			repeated[text] = true
		}
	}
	return repeated
}

func ExtractCandidates(pageHTML string, base *url.URL) []Candidate {
	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return nil
	}

	repeated := repeatedLinkTexts(doc)

	var strong, weak []Candidate
	seen := make(map[string]struct{})

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			if candidate, isStrong, ok := candidateFrom(n, base, repeated); ok {
				// Paging and view-switching links are calendar chrome,
				// whatever their text says.
				if IsNavigationLink(candidate.URL, base) {
					return
				}
				if _, dup := seen[candidate.URL]; !dup {
					seen[candidate.URL] = struct{}{}
					if isStrong {
						strong = append(strong, candidate)
					} else {
						weak = append(weak, candidate)
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)

	// When a site labels its exhibitions in the URL, trust that labelling and
	// drop everything filed under its generic programme paths — those are the
	// talks, classes and late openings that sit beside the exhibitions.
	if len(strong) > 0 {
		return strong
	}
	return weak
}

// candidateFrom decides whether an anchor names an exhibition.
func candidateFrom(anchor *html.Node, base *url.URL, repeated map[string]bool) (Candidate, bool, bool) {
	href := attr(anchor, "href")
	if href == "" {
		return Candidate{}, false, false
	}

	resolved, ok := resolveURL(base, href)
	if !ok {
		return Candidate{}, false, false
	}
	parsed, err := url.Parse(resolved)
	if err != nil || parsed.Host != base.Host {
		// Off-site links are sponsors, socials and ticketing partners.
		return Candidate{}, false, false
	}

	// The type is carried by the path *segments*, never by the final slug: the
	// Royal Academy files a DJ night at /event/summer-exhibition-friday-lates,
	// whose slug contains "exhibition" while the entry plainly is not one.
	sections := pathSections(parsed.Path)
	if len(sections) == 0 {
		// A listing index links to its entries, so an entry is deeper than the
		// index itself.
		return Candidate{}, false, false
	}
	isStrong := containsAny(strings.Join(sections, "/"), strongPathHints)
	if !isStrong && !containsAny(strings.Join(sections, "/"), weakPathHints) {
		return Candidate{}, false, false
	}

	text := titleOf(anchor)
	if len([]rune(text)) < 4 || len(text) > 400 {
		return Candidate{}, false, false
	}

	linkText := textOf(anchor)
	// The type check runs against the link's own text, never against the
	// surrounding context: sibling cards share a container, and judging by the
	// container would let one "Guided tour" entry disqualify every exhibition
	// listed beside it.
	if isNonExhibition(text + " " + linkText) {
		return Candidate{}, false, false
	}

	lower := strings.ToLower(text)
	for _, word := range skipLinkWords {
		if strings.Contains(lower, word) {
			return Candidate{}, false, false
		}
	}

	title := cleanTitle(text)
	// Some cards link out through a "Find out more" button rather than from the
	// title itself, so the anchor carries no title at all. The URL slug does.
	// When the link's text is a button rather than a name, the name is
	// elsewhere in the card.
	//
	// A button's label cannot be recognised from a list of phrases: there is
	// one per language and per site — "Läs mer", "Upptäck mer", "En savoir
	// plus", "詳細を見る" — and the list is never finished. What gives it away
	// without knowing any of them is that a listing page repeats it on every
	// card, while an exhibition's name appears once.
	//
	// The URL slug is what the title falls back to.
	isButton := title == "" ||
		isCallToAction(title) ||
		repeated[strings.ToLower(strings.TrimSpace(title))]

	if isButton {
		// The slug, not the card's heading: a card's heading is as often the
		// section it sits in ("Flowers for you") as the thing it names, and the
		// slug names the entry by construction.
		title = titleFromSlug(parsed.Path)
	}
	if title == "" {
		return Candidate{}, false, false
	}
	return Candidate{Title: title, URL: resolved, Context: cardContext(anchor, linkText)}, isStrong, true
}

// callsToAction are the button labels that stand in for a title on cards whose
// link is a "read more" control.
var callsToAction = []string{
	"find out more", "read more", "learn more", "see more", "more info",
	"more information", "discover", "explore", "view", "details",
	"en savoir plus", "découvrir", "decouvrir", "plus d'infos",
	"mehr erfahren", "weiterlesen", "meer info", "lees meer",
	"saber más", "scopri", "ver más", "läs mer",
}

// isCallToAction reports whether a title is really a button label.
func isCallToAction(title string) bool {
	normalized := strings.ToLower(strings.Trim(strings.TrimSpace(title), ".>»→ "))
	for _, phrase := range callsToAction {
		if normalized == phrase {
			return true
		}
	}
	return false
}

// titleFromSlug builds a readable title from a URL's final segment. It is a
// fallback, so the result is approximate — "we-are-still-here" becomes "We Are
// Still Here" — but it beats labelling every entry "Find out more".
func titleFromSlug(path string) string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return ""
	}
	slug := trimmed[strings.LastIndex(trimmed, "/")+1:]

	// Drop a file extension and any numeric id suffix.
	if idx := strings.LastIndex(slug, "."); idx > 0 {
		slug = slug[:idx]
	}
	slug = strings.NewReplacer("-", " ", "_", " ").Replace(slug)
	slug = strings.TrimSpace(whitespaceRe.ReplaceAllString(slug, " "))
	if len([]rune(slug)) < 3 {
		return ""
	}

	words := strings.Fields(slug)
	for i, word := range words {
		runes := []rune(word)
		words[i] = strings.ToUpper(string(runes[:1])) + string(runes[1:])
	}
	return strings.Join(words, " ")
}

// pathSections returns a URL path's segments with the final one — the entry's
// own slug — removed, so classification looks only at the site's structure.
// It returns nothing for a path with no depth.
func pathSections(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(strings.ToLower(trimmed), "/")
	if len(parts) < 2 {
		return nil
	}
	return parts[:len(parts)-1]
}

// containsAny reports whether s contains any of the substrings.
func containsAny(s string, substrings []string) bool {
	for _, sub := range substrings {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// cardContext returns the text to search for dates: the link's own text plus
// the smallest enclosing element that adds anything.
//
// The bound matters. Climbing to any ancestor would eventually reach the page
// body, whose text holds every other listing's dates; a card is only modestly
// larger than the link it wraps, so growth is capped.
func cardContext(anchor *html.Node, linkText string) string {
	context := linkText
	limit := max(3*len(linkText), 300)

	for parent := anchor.Parent; parent != nil; parent = parent.Parent {
		surrounding := textOf(parent)
		if len(surrounding) > limit {
			break
		}
		if len(surrounding) > len(context) {
			context = surrounding
		}
	}
	return context
}

// titleOf picks the cleanest title an anchor offers.
//
// Listing cards wrap an image, a type label, the title, the venue and the dates
// in one link, so the link's full text is a poor title. Sites that build such
// cards almost always label the link for screen readers, or mark the title up
// as a heading, and either is far cleaner than the flattened text.
func titleOf(anchor *html.Node) string {
	if label := attr(anchor, "aria-label"); len(label) >= 4 {
		return label
	}
	if label := attr(anchor, "title"); len(label) >= 4 {
		return label
	}
	if heading := findHeading(anchor); heading != "" {
		return heading
	}
	return textOf(anchor)
}

// findHeading returns the text of the first heading element inside n.
func findHeading(n *html.Node) string {
	var found string

	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if found != "" {
			return
		}
		if node.Type == html.ElementNode {
			switch node.Data {
			case "h1", "h2", "h3", "h4", "h5", "h6":
				if text := textOf(node); len([]rune(text)) >= 4 {
					found = text
					return
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)

	return found
}

// nonExhibitionTypes are programme entries that are events rather than
// exhibitions. Listings mix them together, and the caller asked for what is on
// show, not for the talks and tours around it.
var nonExhibitionTypes = []string{
	"guided tour", "guided visit", "workshop", "concert", "lecture",
	"screening", "conference", "performance", "reading", "masterclass",
	"symposium", "seminar", "webinar", "course", "family activity",
	"visite guidée", "atelier", "rencontre", "projection", "spectacle",
	"führung", "vortrag", "werkstatt", "rondleiding", "lezing",
}

// isNonExhibition reports whether a listing card labels itself as some other
// kind of event.
func isNonExhibition(context string) bool {
	lower := strings.ToLower(context)
	for _, kind := range nonExhibitionTypes {
		if strings.Contains(lower, kind) {
			return true
		}
	}
	return false
}

// cleanTitle strips the labels listings prepend to a title ("Exhibition",
// "Free", "Members only") and trims the result to something displayable.
func cleanTitle(text string) string {
	// Escaped markup occasionally survives into text nodes; a title containing
	// a tag is never usable.
	title := markupRe.ReplaceAllString(text, " ")
	title = whitespaceRe.ReplaceAllString(strings.TrimSpace(title), " ")

	// Cards frequently lead with the venue and a type label before the title,
	// as in "Grand Palais, Paris Exhibition Hilma af Klint". Everything up to
	// and including the last type label is scaffolding.
	if idx := lastTypeLabel(title); idx != -1 {
		if rest := strings.TrimLeft(title[idx:], " :–—-"); len([]rune(rest)) >= 4 {
			title = rest
		}
	}

	// Listing cards run the title into the dates and the "now on view" badge.
	// Cutting at the first date keeps the title and drops the rest.
	if idx := firstDateIndex(title); idx > 3 {
		title = title[:idx]
	}
	for _, tail := range []string{
		" Until ", " From ", " Now on", " Nu te zien", " Verwacht", " FREE",
		": Plan your visit", " Plan your visit", " Book now", " Book tickets",
	} {
		if idx := strings.Index(title, tail); idx > 3 {
			title = title[:idx]
		}
	}

	return strings.Trim(strings.TrimSpace(title), " ,;:–—-")
}

// markupRe matches anything tag-shaped left in extracted text.
var markupRe = regexp.MustCompile(`<[^>]*>?`)

// typeLabels are the words listings put in front of a title to say what kind
// of entry it is.
var typeLabels = []string{
	"Youth exhibition", "Exhibition", "Ausstellung", "Exposition",
	"Mostra", "Tentoonstelling", "Display", "Event", "Teens",
}

// lastTypeLabel returns the offset just past the last type label in title, or
// -1 when there is none.
func lastTypeLabel(title string) int {
	best := -1
	for _, label := range typeLabels {
		if idx := strings.LastIndex(title, label); idx != -1 && idx+len(label) > best {
			best = idx + len(label)
		}
	}
	return best
}

// forEachAnchor invokes fn for every anchor with an href.
func forEachAnchor(n *html.Node, fn func(href, text string)) {
	if n.Type == html.ElementNode && n.Data == "a" {
		if href := attr(n, "href"); href != "" {
			fn(href, textOf(n))
		}
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		forEachAnchor(child, fn)
	}
}

// skippedElements never contribute visible text. noscript matters as much as
// script here: lazy-loading image markup lives inside it, and its contents are
// escaped, so they surface as literal "<img src=..." text if not excluded.
var skippedElements = map[string]bool{
	"script": true, "style": true, "noscript": true,
	"template": true, "svg": true, "head": true,
}

// attr returns an element's attribute value.
func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return strings.TrimSpace(a.Val)
		}
	}
	return ""
}

// textOf returns the visible text of a node, with script and style contents
// excluded.
func textOf(n *html.Node) string {
	var b strings.Builder

	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && skippedElements[node.Data] {
			return
		}
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
			b.WriteByte(' ')
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)

	return strings.TrimSpace(whitespaceRe.ReplaceAllString(b.String(), " "))
}

// resolveURL turns a possibly relative href into an absolute http(s) URL.
func resolveURL(base *url.URL, href string) (string, bool) {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") ||
		strings.HasPrefix(strings.ToLower(href), "javascript:") ||
		strings.HasPrefix(strings.ToLower(href), "mailto:") ||
		strings.HasPrefix(strings.ToLower(href), "tel:") {
		return "", false
	}

	parsed, err := url.Parse(href)
	if err != nil {
		return "", false
	}
	resolved := base.ResolveReference(parsed)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return "", false
	}
	resolved.Fragment = ""
	return resolved.String(), true
}

// candidateListingURLs returns the conventional programme URLs for a site, used
// when the home page offers no obvious link.
func candidateListingURLs(base *url.URL) []string {
	urls := make([]string, 0, len(listingPaths))
	for _, path := range listingPaths {
		candidate := *base
		candidate.Path = path
		candidate.RawQuery = ""
		candidate.Fragment = ""
		urls = append(urls, candidate.String())
	}
	return urls
}

// datesFor reads the run dates for a candidate, preferring text inside the link
// and falling back to its surroundings.
func datesFor(c Candidate, now time.Time) DateRange {
	if dates := ParseDateRange(c.Title, now); !dates.IsZero() {
		return dates
	}
	return ParseDateRange(c.Context, now)
}
