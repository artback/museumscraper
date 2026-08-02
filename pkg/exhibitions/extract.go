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
	// Swedish and Danish were missing while Norwegian was here, so a nav link
	// reading "Utställningar" — which is how a Swedish museum labels the way
	// to its programme — scored nothing from its text. World of Volvo and the
	// Medicinhistoriska museet both list their exhibitions behind exactly that
	// word. The same gap was found and fixed in strongPathHints below and not
	// carried across to here, so both spellings of each go in this time.
	"utställning", "utställningar", "utstallning", "utstallningar",
	"udstilling", "udstillinger", "näyttely", "nayttely", "näyttelyt",
	// Words for permanent displays are deliberately absent: this list decides
	// which page is read for the programme, and the loop that uses it stops at
	// the first page that yields anything. Scoring a permanent link here put
	// Ateneum's permanent collection ahead of "/nayttelyt/" and lost all three
	// of its temporary exhibitions. Permanent pages are found separately, by
	// FindPermanentLinks, so they add to the programme instead of replacing it.
}

// pastListingWords name a page as the archive of what has already closed.
//
// Never worth one of the three page reads a museum gets: every entry behind it
// is a finished run, and every one is discarded for being finished. Hasselblad
// Center offers "Past Exhibitions" and "Calendar" alongside "Current
// Exhibitions", all three scoring alike, and the archive came first in the
// page — so the budget was spent before the current programme was reached.
var pastListingWords = []string{
	"past exhibition", "past show", "previous exhibition", "archive", "arkiv",
	"tidigare", "förra", "vergangene", "vroegere", "passate", "anteriores",
	"archiv", "ausstellungsarchiv",
	"passées", "passees", "aiemmat", "tidligere",
}

// currentListingWords name a page as what is on right now, which is what a
// visitor is asking about and so what the budget should be spent on first.
var currentListingWords = []string{
	"current", "now on", "on now", "on view now", "aktuell", "aktuella",
	"pågående", "pagaende", "nu visas", "actueel", "en cours", "attuale",
	"actual", "nykyiset", "jetzt", "heute",
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
	// A permanent display is as plainly an exhibition as a temporary one, and
	// the sites that separate the two say which is which in the path.
	"permanent", "dauerausstellung", "basutstallning", "basutställning",
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
	"biljett", "biljetter", "köp biljett", "entré", "öppettider", "kontakt",
	// Archives of what has already been on. Harmless while every entry needed
	// a date — a closed run was dropped for being closed — but the undated
	// rule has no dates to judge by, so the index of past shows has to be
	// named. The Medicinhistoriska museet lists "Tidigare utställningar"
	// beside its current ones.
	"tidigare", "past exhibition", "previous exhibition", "archive", "arkiv",
	"archiv", "sarchiv",
	// Staff pages. A museum names them after the exhibition the team works on
	// — "Team Dauerausstellung" — so the path and the words both look right.
	"team ", "unser team", "mitarbeiter", "staff", "medarbetare", "personal",
	"vergangene", "vroegere", "mostre passate", "exposiciones anteriores",
	"expositions passées", "aiemmat",
	// And the index of what has not opened yet, for the same reason: Ateneum
	// lists "Tulevat näyttelyt" beside its current ones.
	"tulevat", "upcoming exhibition", "kommande", "kommende", "à venir",
	"proximas", "próximas", "prossime",
}

// whitespaceRe collapses runs of whitespace in extracted text.
var whitespaceRe = regexp.MustCompile(`\s+`)

// invisibleText strips the characters a page inserts to control line breaking
// and nothing else.
//
// German sites use the soft hyphen heavily — the Jewish Museum Berlin writes
// "Aus­stel­lun­gen" with three of them — and every word list in this package
// then fails to match, silently. Its archive link, "Alle ver­gangenen
// Aus­stel­lun­gen", was read as a permanent exhibition for exactly that
// reason. They are invisible to a reader and should be invisible here.
var invisibleText = strings.NewReplacer(
	"\u00ad", "", // soft hyphen
	"\u200b", "", // zero-width space
	"\u200c", "", // zero-width non-joiner
	"\u200d", "", // zero-width joiner
	"\ufeff", "", // byte-order mark
)

// Candidate is a possible exhibition pulled out of a listing page.
type Candidate struct {
	Title string
	URL   string
	// Context is the text surrounding the link, which is where the dates
	// usually live.
	Context string
	// Dates are the run dates when the page gave them in a machine-readable
	// form — a <time datetime> attribute, or a schema.org event. Both are
	// ISO-8601 whatever language the page is written in, so where they exist
	// they are used in place of reading the text.
	Dates DateRange

	// Vouched marks an entry admitted only because the page it sits on calls
	// itself an exhibitions listing — its own path says nothing. The caller
	// holds these to a higher standard, because the path test they skipped is
	// what normally keeps a listing page's navigation out.
	Vouched bool
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

		parsed, err := url.Parse(resolved)
		if err != nil {
			return
		}
		// Only this site's own pages. A listing page is read for the links it
		// carries, and ExtractCandidates discards everything off-host, so an
		// off-site page can never yield an exhibition — but it still costs one
		// of the three pages a museum gets. World of Volvo's front page offers
		// four ticketing links on a different host, which filled the budget
		// before its exhibitions page was ever tried.
		if base.Host != "" && !strings.EqualFold(parsed.Host, base.Host) {
			return
		}

		lower := strings.ToLower(text)
		path := strings.ToLower(parsed.Path)
		if containsAny(lower, pastListingWords) || containsAny(path, pastListingWords) {
			return
		}

		score := 0
		for _, word := range listingLinkWords {
			if strings.Contains(lower, word) {
				score += 2
				break
			}
		}
		// What is on now beats what was on, and beats an undifferentiated
		// calendar of everything the museum does.
		if containsAny(lower, currentListingWords) || containsAny(path, currentListingWords) {
			score += 2
		}
		for _, hint := range exhibitionPathHints {
			if strings.Contains(path, hint) {
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
	"permanent": true, "permanent-exhibitions": true, "permanent-exhibition": true,
	"dauerausstellung": true, "fasta-utstallningar": true, "basutstallningar": true,
	"vaste-collectie": true,
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
	// A page that calls itself the exhibitions page is believed about the
	// links it carries, even when their paths say nothing.
	//
	// The path test is the main defence against reading a listing page's
	// navigation as exhibitions, and it holds wherever a site files
	// exhibitions somewhere named. Plenty do not. The Hasselblad Foundation
	// publishes every exhibition under "/en/portfolio_page/439876/" — a
	// WordPress grid plugin's own path, carrying an post id and no word about
	// what it is — so "Women Behind the Camera" was rejected along with the
	// rest, and a museum with a full programme looked empty.
	//
	// Relaxing it here is safe because the date rule is still in force. An
	// entry on this page has to be datable to be kept, and the navigation this
	// would otherwise let in — the opening hours, the library, the shop —
	// carries no dates.
	trustPage := namesExhibitions(pageHTML, base)

	var strong, weak []Candidate
	seen := make(map[string]struct{})

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			if candidate, isStrong, ok := candidateFrom(n, base, repeated, trustPage); ok {
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
func candidateFrom(anchor *html.Node, base *url.URL, repeated map[string]bool, trustPage bool) (Candidate, bool, bool) {
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
	joined := strings.Join(sections, "/")
	if len(sections) == 0 {
		// No sections means a flat path — the whole of it is the entry's own
		// slug, with no directory above to classify it by.
		//
		// Rejecting those outright lost every exhibition on any site that does
		// not nest. The Jewish Museum Berlin publishes "/dauerausstellung" and
		// "/ausstellung-gegenteil-von-jetzt" at the root, so it had no
		// extractable exhibitions at all and fell back to a single entry for
		// the museum.
		//
		// Judged by the slug, which the nested case deliberately ignores, and
		// only on a strong word. The danger the nested rule guards against is
		// the Royal Academy's "/event/summer-exhibition-friday-lates", where a
		// directory says "event" and the slug says "exhibition" — there the
		// directory is believed. Here there is no directory to disagree with.
		slug := strings.Trim(strings.ToLower(parsed.Path), "/")
		if slug == "" || !containsAny(slug, strongPathHints) {
			return Candidate{}, false, false
		}
		joined = slug
	}
	isStrong := containsAny(joined, strongPathHints)
	pathNamed := isStrong || containsAny(joined, weakPathHints)
	// trustPage stands in for a path hint the site does not give. It never
	// makes an entry strong: a page that does name its exhibitions in the path
	// still outranks one that does not.
	if !pathNamed && !trustPage {
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

	context, card := cardContext(anchor, linkText)
	return Candidate{
		Title:   title,
		URL:     resolved,
		Context: context,
		Dates:   machineDates(card),
		Vouched: !isStrong && !pathNamed,
	}, isStrong, true
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

	// Drop a file extension.
	if idx := strings.LastIndex(slug, "."); idx > 0 {
		slug = slug[:idx]
	}
	slug = trimOccurrenceSuffix(slug)
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

// trimOccurrenceSuffix removes the counter a site appends to a slug when it
// publishes the same event again.
//
// This is what makes the recurring-event merge work at all. Hasselblad Center
// publishes one weekly introduction to its current exhibition as seven pages,
// "…-women-behind-the-camera-1848-1968-3" through "-9". The merge keys on the
// title, and where the title comes from the slug, the counter made seven
// different titles out of one name — so nothing merged and the museum showed
// the same event seven times.
//
// Only a short number, and only when a real name is left without it. A year is
// four digits and stays; "documenta-14" keeps its number because "documenta"
// alone is not a name the rest of the slug supports.
func trimOccurrenceSuffix(slug string) string {
	idx := strings.LastIndex(slug, "-")
	if idx <= 0 {
		return slug
	}
	suffix := slug[idx+1:]
	if len(suffix) == 0 || len(suffix) > 2 {
		return slug
	}
	if _, err := strconv.Atoi(suffix); err != nil {
		return slug
	}
	if strings.Count(slug[:idx], "-") < 2 {
		return slug
	}
	return slug[:idx]
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
// the smallest enclosing element that adds anything. It also returns the
// element that text came from, which is the card, so its markup can be read
// for dates the text does not spell out.
//
// The bound matters. Climbing to any ancestor would eventually reach the page
// body, whose text holds every other listing's dates; a card is only modestly
// larger than the link it wraps, so growth is capped.
func cardContext(anchor *html.Node, linkText string) (string, *html.Node) {
	context, card := linkText, anchor
	limit := max(3*len(linkText), 300)

	for parent := anchor.Parent; parent != nil; parent = parent.Parent {
		surrounding := textOf(parent)
		if len(surrounding) > limit {
			break
		}
		if len(surrounding) > len(context) {
			context, card = surrounding, parent
		}
	}
	return context, card
}

// maxCardTimes bounds how many <time> elements are read from one card. A card
// carries an opening and a closing date; a page whose markup put a hundred
// inside one is not a listing card and should not be read as though the first
// and last of them were an exhibition's run.
const maxCardTimes = 8

// machineDates reads the ISO dates a card states in <time datetime="…">
// attributes.
//
// This is the cheapest language-independence available. A site writes "12.
// marts – 7. september 2026" or "12 марта" in its own language and its own
// order, and then marks the same two dates up as datetime="2026-03-12" for
// browsers and search engines. The date table in dates.go knows ten languages
// and will never know all of them; this knows the format every site agrees on.
func machineDates(card *html.Node) DateRange {
	var stamps []time.Time

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(stamps) >= maxCardTimes {
			return
		}
		if n.Type == html.ElementNode && n.Data == "time" {
			if when, ok := parseISODate(attr(n, "datetime")); ok {
				stamps = append(stamps, when)
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(card)

	switch len(stamps) {
	case 0:
		return DateRange{}
	case 1:
		// One stamp is a single-day entry as far as the markup says. The text
		// around it decides whether it is really an opening or a closing date,
		// which is what the text reader is for, so this is left for it.
		return DateRange{}
	default:
		start, end := stamps[0], stamps[len(stamps)-1]
		for _, when := range stamps {
			if when.Before(start) {
				start = when
			}
			if when.After(end) {
				end = when
			}
		}
		return DateRange{Start: &start, End: &end}
	}
}

// parseISODate reads the date out of an ISO-8601 date or date-time attribute.
func parseISODate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 10 {
		return time.Time{}, false
	}
	when, err := time.Parse("2006-01-02", value[:10])
	if err != nil {
		return time.Time{}, false
	}
	return when, true
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
	// An introduction, a walkthrough or a curator's talk is an event about an
	// exhibition rather than the exhibition, and museums schedule them weekly.
	// Hasselblad Center's calendar carries seven introductions to one show and
	// no entry for the show itself.
	"introduction to the exhibition", "introduktion till utställningen",
	"curator's tour", "curator tour", "artist talk", "gallery talk",
	"einführung in die ausstellung", "visite commentée",
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

	// The label is two words when the entry is a permanent one — "Exposition
	// permanente", "Ausstellung dauerhaft" — and only the first is a type. The
	// Mucem's "Exposition permanente Méditerranées" came out titled
	// "permanente Méditerranées" until the adjective went too.
	title = trimLeadingWord(title, permanenceAdjectives)

	// A link's accessible label often leads with what the link does rather than
	// what it names — "Gå till sidan Kungens skepp", "Read about Bärgningen".
	// titleOf prefers the aria-label because it is usually the cleanest title
	// on the card, so the affordance has to come off the front of it or every
	// exhibition at the Vasamuseet is called "Gå till sidan ...".
	for _, prefix := range linkAffordances {
		if len(title) > len(prefix) && strings.EqualFold(title[:len(prefix)], prefix) {
			if rest := strings.TrimSpace(title[len(prefix):]); len([]rune(rest)) >= 4 {
				title = rest
			}
			break
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

// linkAffordances are the phrases an accessible label puts in front of the
// thing it names, saying what following the link does.
var linkAffordances = []string{
	"gå till sidan ", "ga till sidan ", "gå till ", "läs om ", "las om ",
	"go to page ", "go to ", "read about ", "read more about ", "more about ",
	"link to ", "navigate to ", "visit page ",
	"zur seite ", "mehr über ", "mehr ueber ",
	"aller à la page ", "en savoir plus sur ",
	"ga naar ", "lees over ",
	"siirry sivulle ", "gå til siden ",
}

// markupRe matches anything tag-shaped left in extracted text.
var markupRe = regexp.MustCompile(`<[^>]*>?`)

// typeLabels are the words listings put in front of a title to say what kind
// of entry it is.
var typeLabels = []string{
	"Youth exhibition", "Exhibition", "Ausstellung", "Exposition",
	"Mostra", "Tentoonstelling", "Display", "Event", "Teens",
}

// permanenceAdjectives are the words that follow a type label to say the entry
// has no closing date, in the languages that put the adjective after the noun.
var permanenceAdjectives = []string{
	"permanente", "permanenta", "permanentes", "permanenti", "permanent",
	"dauerhaft", "stała", "stálá",
}

// trimLeadingWord removes a leading word from title when it is one of words,
// leaving the title untouched when nothing readable would remain.
func trimLeadingWord(title string, words []string) string {
	first, rest, found := strings.Cut(strings.TrimSpace(title), " ")
	if !found {
		return title
	}
	lower := strings.ToLower(first)
	for _, word := range words {
		if lower != word {
			continue
		}
		if trimmed := strings.TrimLeft(rest, " :–—-"); len([]rune(trimmed)) >= 4 {
			return trimmed
		}
		return title
	}
	return title
}

// lastTypeLabel returns the offset just past the last type label in title, or
// -1 when there is none.
func lastTypeLabel(title string) int {
	best := -1
	for _, label := range typeLabels {
		idx := strings.LastIndex(title, label)
		if idx == -1 {
			continue
		}
		end := idx + len(label)
		// A whole word, not the start of a longer one. Cutting inside a word
		// turned "Ausstellungsarchiv" into "sarchiv": the label "Ausstellung"
		// is a prefix of it, and everything up to the end of the label was
		// taken for scaffolding.
		if end < len(title) {
			if r := rune(title[end]); r == '\'' || (r|0x20 >= 'a' && r|0x20 <= 'z') || r >= 0x80 {
				continue
			}
		}
		if end > best {
			best = end
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
			return strings.TrimSpace(invisibleText.Replace(a.Val))
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

	return strings.TrimSpace(whitespaceRe.ReplaceAllString(invisibleText.Replace(b.String()), " "))
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

// datesFor reads the run dates for a candidate: what the markup stated
// outright first, then the text inside the link, then its surroundings.
func datesFor(c Candidate, now time.Time) DateRange {
	if c.Dates.Known() {
		return c.Dates.resolveOpenEnd(now)
	}
	if dates := ParseDateRange(c.Title, now); dates.Known() {
		return dates.resolveOpenEnd(now)
	}
	return ParseDateRange(c.Context, now).resolveOpenEnd(now)
}
