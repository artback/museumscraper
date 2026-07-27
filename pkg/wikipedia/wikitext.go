package wikipedia

import (
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	commentRe     = regexp.MustCompile(`(?s)<!--.*?-->`)
	refPairRe     = regexp.MustCompile(`(?is)<ref[^>]*>.*?</ref>`)
	refSelfRe     = regexp.MustCompile(`(?is)<ref[^>]*/\s*>`)
	galleryRe     = regexp.MustCompile(`(?is)<gallery[^>]*>.*?</gallery>`)
	tagRe         = regexp.MustCompile(`(?is)</?(?:small|sub|sup|span|div|br|center|nowiki)[^>]*>`)
	extLinkRe     = regexp.MustCompile(`\[(?:https?:|//)[^\]]*\]`)
	bareLinkRe    = regexp.MustCompile(`\[\[([^\[\]|]+)(?:\|([^\[\]]*))?\]\]`)
	headingRe     = regexp.MustCompile(`^(={2,6})\s*(.*?)\s*={2,6}\s*$`)
	interwikiRe   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	whitespaceRe  = regexp.MustCompile(`\s+`)
	listMarkerRe  = regexp.MustCompile(`^([*#:;]+)\s*(.*)$`)
	numericTitleR = regexp.MustCompile(`^\d{1,4}(s|st|nd|rd|th)?$`)
)

// sectionsToSkip are heading names whose contents never describe a museum but
// are full of link-shaped noise (other list pages, citations, portals).
var sectionsToSkip = map[string]bool{
	"see also":          true,
	"references":        true,
	"external links":    true,
	"further reading":   true,
	"notes":             true,
	"notes and refs":    true,
	"bibliography":      true,
	"sources":           true,
	"citations":         true,
	"footnotes":         true,
	"gallery":           true,
	"navigation":        true,
	"related":           true,
	"further resources": true,
}

// nonArticlePrefixes are MediaWiki namespaces that never contain an article
// about a museum. Compared case-insensitively against a link's prefix.
var nonArticlePrefixes = map[string]bool{
	"file": true, "image": true, "category": true, "template": true,
	"help": true, "portal": true, "wikipedia": true, "wp": true,
	"wikt": true, "wiktionary": true, "commons": true, "media": true,
	"special": true, "talk": true, "user": true, "draft": true,
	"module": true, "book": true, "mediawiki": true, "s": true,
	"q": true, "m": true, "n": true, "v": true, "voy": true, "d": true,
}

// cleanWikitext removes the markup that contributes link-shaped noise but never
// names a museum: HTML comments, <ref> citations (which are full of wiki links
// to publishers and organisations), galleries, inline HTML and bare external
// links. Table and list structure is deliberately preserved.
func cleanWikitext(s string) string {
	s = commentRe.ReplaceAllString(s, "")
	s = refPairRe.ReplaceAllString(s, "")
	s = refSelfRe.ReplaceAllString(s, "")
	s = galleryRe.ReplaceAllString(s, "")
	s = stripTemplates(s)
	s = extLinkRe.ReplaceAllString(s, "")
	s = tagRe.ReplaceAllString(s, "")
	return s
}

// stripTemplates removes {{...}} constructs, honouring nesting so that a
// citation template containing another template is removed as a whole. Table
// syntax ("{|" and "|}") is left untouched because neither brace is doubled.
//
// If the braces turn out to be unbalanced the original string is returned
// rather than a truncated one, so a single malformed template cannot silently
// swallow the rest of a page.
func stripTemplates(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	depth := 0
	for i := 0; i < len(s); i++ {
		switch {
		case i+1 < len(s) && s[i] == '{' && s[i+1] == '{':
			depth++
			i++
		case i+1 < len(s) && s[i] == '}' && s[i+1] == '}' && depth > 0:
			depth--
			i++
		case depth == 0:
			b.WriteByte(s[i])
		}
	}

	if depth != 0 {
		return s
	}
	return b.String()
}

// wikiLink is a parsed [[Target|Display]] construct.
type wikiLink struct {
	// Target is the canonical article title.
	Target string
	// Display is the link's visible text.
	Display string
	// raw is the target exactly as written. Namespace and interwiki detection
	// must run against it: normalising upper-cases the first letter, which
	// would turn "fr:Musée" into "Fr:Musée" and hide the language prefix.
	raw string
}

// parseLinks returns every internal wiki link in s, in order of appearance.
// Fragments ("Article#Section") are trimmed to the article title.
func parseLinks(s string) []wikiLink {
	matches := bareLinkRe.FindAllStringSubmatch(s, -1)
	links := make([]wikiLink, 0, len(matches))
	for _, m := range matches {
		target := normalizeTitle(m[1])
		if target == "" {
			continue
		}
		display := target
		if len(m) > 2 && strings.TrimSpace(m[2]) != "" {
			display = strings.TrimSpace(m[2])
		}
		links = append(links, wikiLink{Target: target, Display: display, raw: strings.TrimSpace(m[1])})
	}
	return links
}

// normalizeTitle trims a link target down to a canonical article title:
// whitespace collapsed, any "#fragment" removed, and a leading ":" (the
// interwiki escape) dropped.
func normalizeTitle(raw string) string {
	// Wikitext escapes ampersands and dashes as HTML entities; the API expects
	// the decoded characters.
	title := html.UnescapeString(strings.TrimSpace(raw))
	title = strings.TrimPrefix(title, ":")
	if idx := strings.Index(title, "#"); idx != -1 {
		title = title[:idx]
	}
	title = whitespaceRe.ReplaceAllString(strings.TrimSpace(title), " ")
	if title == "" {
		return ""
	}
	// MediaWiki treats underscores and spaces as equivalent and upper-cases the
	// first letter of a title. The first letter must be taken as a rune, not a
	// byte: slicing "Übersee Museum Bremen" at [:1] splits the two-byte "Ü" and
	// produces a title the API cannot find.
	title = strings.ReplaceAll(title, "_", " ")
	first, size := utf8.DecodeRuneInString(title)
	if first == utf8.RuneError {
		return title
	}
	return string(unicode.ToUpper(first)) + title[size:]
}

// isArticleLink reports whether a link target refers to a normal article rather
// than a file, category, template or another language's wiki.
//
// It must be given the target as written, not the normalised title: the leading
// letter of a normalised title is upper-cased, which would disguise the "fr:"
// in "fr:Musée français" as part of an article name.
func isArticleLink(rawTarget string) bool {
	title := strings.TrimPrefix(strings.TrimSpace(rawTarget), ":")
	if title == "" {
		return false
	}
	if idx := strings.Index(title, ":"); idx > 0 {
		prefix := strings.TrimSpace(title[:idx])
		if nonArticlePrefixes[strings.ToLower(prefix)] {
			return false
		}
		// A short all-lowercase prefix is an interwiki language code
		// ("fr:Louvre"); real article titles capitalise the word before a colon.
		if len(prefix) <= 3 && interwikiRe.MatchString(prefix) {
			return false
		}
	}
	// Bare years and numbers show up in date columns and "established" cells.
	return !numericTitleR.MatchString(title)
}

// isListTitle reports whether title is itself an index page ("List of museums
// in Spain") rather than a museum.
func isListTitle(title string) bool {
	lower := strings.ToLower(title)
	return strings.HasPrefix(lower, "list of ") ||
		strings.HasPrefix(lower, "lists of ") ||
		strings.HasPrefix(lower, "index of ") ||
		strings.HasPrefix(lower, "outline of ")
}

// isMuseumListTitle reports whether title is an index page that is worth
// following because it should itself contain museums.
func isMuseumListTitle(title string) bool {
	lower := strings.ToLower(title)
	if !isListTitle(lower) {
		return false
	}
	return strings.Contains(lower, "museum") ||
		strings.Contains(lower, "galler") ||
		strings.Contains(lower, "aquari") ||
		strings.Contains(lower, "planetari")
}

// section describes a heading encountered while scanning a page.
type section struct {
	level int
	name  string
	// linked is true when the heading text was a wiki link, as in
	// "===01 - [[Ain]]===". Museum lists link their headings when the heading
	// names a place and leave them as plain text when it names a theme
	// ("House-museums, biography"), which is what makes the flag a usable
	// signal for whether the heading can stand in as a locality.
	linked bool
}

// parseHeading returns the heading described by line, and whether line was a
// heading at all.
func parseHeading(line string) (section, bool) {
	m := headingRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return section{}, false
	}

	name := m[2]
	linked := false
	if links := parseLinks(name); len(links) > 0 && isArticleLink(links[0].raw) {
		name = links[0].Target
		linked = true
	}
	return section{level: len(m[1]), name: strings.TrimSpace(name), linked: linked}, true
}
