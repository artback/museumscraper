package exhibitions

import (
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// This file is about displays with no closing date.
//
// The rest of the scraper is built on dates: a listing entry that cannot be
// placed in time is discarded, because that is what separates an exhibition
// from the hundreds of other links a museum's programme page carries. That
// rule is right for temporary shows and wrong for everything a museum keeps
// out permanently, which is most of what most museums have. A small museum
// often runs no temporary programme at all — Radiomuseet in Göteborg has no
// exhibitions page, no calendar and no date anywhere on its site, only rooms
// full of radios — and reading it for dated listings returns nothing, which in
// a result set is indistinguishable from "there is nothing to see here".
//
// So a permanent display is recognised by being named rather than dated, at
// three widths: an entry that says so, a listing page whose entries are all
// permanent, and, for the sites that publish no listing at all, the museum
// itself.

// permanentWords say a display has no closing date.
//
// "permanent" alone carries most of Europe — permanent exhibition, exposition
// permanente, permanente tentoonstelling, colección permanente, esposizione
// permanente, permanent utställning — so the rest of the list is only the
// languages that use a different word entirely.
var permanentWords = []string{
	"permanent",
	"dauerausstellung", "ständige ausstellung", "standige ausstellung",
	"basutställning", "basutstallning", "fast utställning", "fasta utställningar",
	"fast udstilling", "faste udstillinger", "fast utstilling", "faste utstillinger",
	"perusnäyttely", "pysyvä näyttely",
	"vaste collectie", "vaste opstelling",
	"wystawa stała", "stała wystawa",
	"stálá expozice", "stálá výstava",
	"állandó kiállítás",
	"постоянная экспозиция", "постоянная выставка",
}

// closedWords undo a permanence match. "Permanently closed" is the one common
// phrase where the word means the opposite of something being on show.
var closedWords = []string{
	"permanently closed", "permanent closure", "permanently shut",
	"permanent stängt", "dauerhaft geschlossen", "définitivement fermé",
}

// namesPermanent reports whether text says a display is permanent.
func namesPermanent(text string) bool {
	lower := strings.ToLower(text)
	for _, phrase := range closedWords {
		if strings.Contains(lower, phrase) {
			return false
		}
	}
	return containsAny(lower, permanentWords)
}

// permanentPathHints name a URL as a permanent display.
//
// Deliberately narrow. The obvious additions — "collection", "samling",
// "sammlung" — are how museums file their object database, which holds
// hundreds of thousands of items that are in storage rather than on show, and
// treating each as a permanent exhibition would bury the real ones.
var permanentPathHints = []string{
	"permanent", "dauerausstellung", "basutstallning", "basutställning",
	"fasta-utstallningar", "fast-utstallning", "faste-udstillinger",
	"vaste-collectie", "vaste-opstelling", "wystawa-stala", "stala-wystawa",
}

// pathNamesPermanent reports whether a URL path names a permanent display.
func pathNamesPermanent(path string) bool {
	return containsAny(strings.ToLower(path), permanentPathHints)
}

// isPermanentListing reports whether a listing page holds permanent displays
// and nothing else, judged by its URL and its own headings.
//
// This is the signal that matters most, because a page of permanent
// exhibitions labels itself once at the top and then lists entries that say
// nothing about their own permanence — they have no dates and no "permanent"
// badge, since the page already said it. Reading the page's own title is what
// lets those entries through without letting every undated link through.
func isPermanentListing(pageHTML string, pageURL *url.URL) bool {
	if pageURL != nil && pathNamesPermanent(pageURL.Path) {
		return true
	}
	return namesPermanent(pageHeadings(pageHTML))
}

// pageHeadings returns a page's <title> and its first heading, which is where a
// page says what it is.
func pageHeadings(pageHTML string) string {
	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return ""
	}

	var title, heading string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				if title == "" {
					title = textOf(n)
				}
			case "h1", "h2":
				if heading == "" {
					heading = textOf(n)
				}
			}
		}
		if title != "" && heading != "" {
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)

	return title + " " + heading
}

// candidateIsPermanent reports whether an undated listing entry is a permanent
// display rather than one of the many links that are not exhibitions at all.
func candidateIsPermanent(c Candidate, listingIsPermanent bool) bool {
	if listingIsPermanent {
		return true
	}
	if namesPermanent(c.Title) || namesPermanent(c.Context) {
		return true
	}
	parsed, err := url.Parse(c.URL)
	return err == nil && pathNamesPermanent(parsed.Path)
}

// maxPermanentLabelRunes is how long a link's text may be and still be read as
// a label naming a permanent-displays page. Comfortably longer than the
// longest of them — "permanent exhibitions", "fasta utställningar",
// "permanente tentoonstellingen" — and far shorter than a headline.
const maxPermanentLabelRunes = 40

// FindPermanentLinks returns the URLs on a page that lead to displays which
// are always on, most promising first.
//
// It matches on the link's text as well as its path, because the two disagree
// often enough to matter: plenty of sites label a link "Fasta utställningar"
// and point it at "/utstallningar/1/", where nothing in the URL says the
// entries behind it have no closing date.
//
// Text only counts when it is short enough to be a label. A sentence that
// happens to contain the word is a headline about one, not a way to it — the
// Technisches Museum Wien's front page carries "Eröffnung der Dauerausstellung
// „Wissenschaft im Wandel"", which leads to a magazine article whose every
// link was then recorded as a permanent display.
func FindPermanentLinks(pageHTML string, base *url.URL) []string {
	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return nil
	}

	var urls []string
	seen := make(map[string]struct{})

	forEachAnchor(doc, func(href, text string) {
		isLabel := len([]rune(strings.TrimSpace(text))) <= maxPermanentLabelRunes
		if !namesPermanent(href) && !(isLabel && namesPermanent(text)) {
			return
		}
		resolved, ok := resolveURL(base, href)
		if !ok {
			return
		}
		parsed, err := url.Parse(resolved)
		if err != nil || !strings.EqualFold(parsed.Host, base.Host) {
			return
		}
		if _, dup := seen[resolved]; dup {
			return
		}
		seen[resolved] = struct{}{}
		urls = append(urls, resolved)
	})

	return urls
}

// infoLinkWords identify the page a museum uses to describe itself and what it
// holds, which is where a site with no programme says what is on show.
var infoLinkWords = []string{
	"museum information", "museiinformation", "museumsinformation",
	"about the museum", "about us", "about", "visit", "plan your visit",
	"what we offer", "what to see", "see and do", "things to see",
	"om museet", "om oss", "besök", "besok", "besøk", "vieraile",
	"besuch", "besuchen", "über uns", "uber uns", "informationen",
	"visiter", "visite", "à propos", "a propos", "informations",
	"visita", "visitar", "informazioni", "información", "informacion",
	"bezoek", "over het museum", "over ons",
}

// infoPathHints are the same idea in a URL, used when a link's text is an icon
// or a phrase no list would hold.
//
// The collection words are deliberately absent from both lists. "/collection"
// is where a museum puts its object database — a search box over a hundred
// thousand catalogue records, most of them in storage — and pointing a visitor
// at that instead of at the building is worse than pointing them nowhere.
var infoPathHints = []string{
	"museiinformation", "museum-information", "about", "visit", "besok",
	"besök", "besuch", "om-museet", "om-oss", "ueber-uns", "uber-uns",
	"visite", "informations", "informazioni", "informacion", "bezoek",
}

// infoPaths are the conventional locations of that page, tried when the home
// page offers no link to one.
var infoPaths = []string{
	"/visit", "/en/visit", "/plan-your-visit", "/about", "/en/about",
	"/museiinformation", "/om-museet", "/besok", "/besuch", "/visite",
	"/bezoek", "/visita", "/informazioni",
}

// FindInfoLinks returns the URLs on a page that lead to the museum's own
// description of itself, most promising first.
func FindInfoLinks(pageHTML string, base *url.URL) []string {
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
		parsed, err := url.Parse(resolved)
		if err != nil || !strings.EqualFold(parsed.Host, base.Host) {
			return
		}
		if _, dup := seen[resolved]; dup {
			return
		}

		// The page describing the museum sits at the top of the site. Depth is a
		// requirement rather than a preference because these words turn up deep
		// in paths that are not it: Moderna Museet's front page links to
		// "/en/stockholm/events/the-moderna-museet-collection-guided-tour/",
		// which matched on a word and would have been recorded as the museum's
		// permanent display.
		depth := strings.Count(strings.Trim(parsed.Path, "/"), "/")
		if depth > 1 {
			return
		}

		score := 0
		if containsAny(strings.ToLower(text), infoLinkWords) {
			score += 2
		}
		if containsAny(strings.ToLower(parsed.Path), infoPathHints) {
			score++
		}
		if score == 0 {
			return
		}
		if depth == 0 {
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

// infoURLs returns the conventional description pages for a site.
func infoURLs(base *url.URL) []string {
	urls := make([]string, 0, len(infoPaths))
	for _, path := range infoPaths {
		candidate := *base
		candidate.Path = path
		candidate.RawQuery = ""
		candidate.Fragment = ""
		urls = append(urls, candidate.String())
	}
	return urls
}

// displayWords say a page is describing things on show, rather than an
// organisation, its opening hours or its membership scheme.
//
// Swedish "visas" and German "gezeigt" are here alongside the nouns because
// small museums often never write the word "exhibition" at all: Radiomuseet's
// museum information page says "Här visas teknikhistorien" — here is shown the
// history of the technology — and then lists what is in the rooms.
var displayWords = []string{
	"exhibit", "exhibition", "on display", "on show", "on view",
	"gallery", "galleries", "collection", "collections", "displayed",
	"utställning", "utstallning", "visas", "visar", "samling", "samlingar",
	"udstilling", "utstilling", "näyttely", "nayttely",
	"ausstellung", "sammlung", "gezeigt", "zeigt", "exponate",
	"exposition", "collections", "exposé", "expose", "montre", "présente",
	"esposizione", "mostra", "collezione", "esposti",
	"exposición", "exposicion", "colección", "coleccion", "muestra",
	"tentoonstelling", "collectie", "getoond", "te zien",
	"wystawa", "ekspozycja", "экспозиция", "выставка",
}

// describesDisplay reports whether a page describes something on show.
//
// A loose test, and meant to be: it guards against recording a permanent
// display for a page that turned out to be a login form or a parked domain,
// not against a museum that describes its rooms in words this list happens to
// miss.
func describesDisplay(pageHTML string) bool {
	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return false
	}
	return containsAny(strings.ToLower(textOf(doc)), displayWords)
}
