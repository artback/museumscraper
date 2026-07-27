package wikipedia

import (
	"strings"
)

// Candidate is a museum-like article title discovered on a list page, together
// with the locality the page grouped it under.
type Candidate struct {
	// Title is the canonical Wikipedia article title, taken from the link
	// target rather than its display text so it can be resolved via the API.
	Title string
	// Locality is the city or town the list page filed the museum under, taken
	// from the parent list item or the table's location column. May be empty.
	Locality string
}

// Extraction is everything a single list page yielded.
type Extraction struct {
	// Candidates are the museum-like articles linked from the page.
	Candidates []Candidate
	// NestedLists are other museum list pages linked from the page ("List of
	// museums in Montevideo"), worth following so their museums are not missed.
	NestedLists []string
}

// MuseumExtractor pulls museum articles out of Wikipedia list pages.
//
// Wikipedia writes these lists in two very different shapes, and both are
// handled:
//
//   - Bullet lists, often nested so that "* [[City]]" groups a set of
//     "** [[Museum]]" children. Group headers are localities, not museums, so
//     they are recorded as the children's locality instead of being emitted.
//   - Sortable wikitables, where only the "Name" column holds museums while the
//     other columns hold towns, dates, photos and citations.
//
// Sections that only ever hold link-shaped noise ("See also", "References")
// are skipped entirely.
type MuseumExtractor struct {
	blocklisted []string
}

// NewMuseumExtractor returns an extractor that drops any article whose title
// starts with one of the blocklisted prefixes.
func NewMuseumExtractor(blocklisted []string) *MuseumExtractor {
	return &MuseumExtractor{blocklisted: blocklisted}
}

// ExtractMuseums returns just the museum titles found in content. It is a thin
// convenience wrapper around Extract.
func (m *MuseumExtractor) ExtractMuseums(content string) []string {
	extraction := m.Extract(content)
	titles := make([]string, 0, len(extraction.Candidates))
	for _, c := range extraction.Candidates {
		titles = append(titles, c.Title)
	}
	return titles
}

// Extract parses a list page's wikitext and returns the museums it names along
// with any nested museum-list pages worth following.
func (m *MuseumExtractor) Extract(content string) Extraction {
	lines := strings.Split(cleanWikitext(content), "\n")

	var (
		result      Extraction
		seen        = make(map[string]struct{})
		seenLists   = make(map[string]struct{})
		skipToLevel = 0 // >0 while inside a skipped section
		// headingScope is only set for headings that were wiki links, so a
		// thematic heading never masquerades as a place.
		headingScope string
		localities   = make(map[int]string)
	)

	noteNestedList := func(title string) {
		if !isMuseumListTitle(title) {
			return
		}
		if _, dup := seenLists[title]; dup {
			return
		}
		seenLists[title] = struct{}{}
		result.NestedLists = append(result.NestedLists, title)
	}

	addCandidate := func(title, locality string) {
		if !m.include(title) {
			return
		}
		if isListTitle(title) {
			noteNestedList(title)
			return
		}
		if _, dup := seen[title]; dup {
			return
		}
		seen[title] = struct{}{}
		result.Candidates = append(result.Candidates, Candidate{Title: title, Locality: locality})
	}

	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])

		if heading, ok := parseHeading(trimmed); ok {
			clear(localities)
			headingScope = ""
			switch {
			case sectionsToSkip[strings.ToLower(heading.name)]:
				skipToLevel = heading.level
			case skipToLevel > 0 && heading.level <= skipToLevel:
				// A sibling or shallower heading ends the skipped section.
				skipToLevel = 0
				if heading.linked {
					headingScope = heading.name
				}
			case skipToLevel == 0:
				if heading.linked {
					headingScope = heading.name
				}
			}
			continue
		}
		if skipToLevel > 0 {
			// "See also" holds no museums, but it is where sub-lists such as
			// "List of museums in Montevideo" are linked, and those must still
			// be followed or their museums are lost.
			for _, link := range parseLinks(trimmed) {
				if isArticleLink(link.raw) {
					noteNestedList(link.Target)
				}
			}
			continue
		}

		if strings.HasPrefix(trimmed, "{|") {
			rows, headers, end := parseTable(lines, i)
			nameCol, locCol := classifyColumns(headers)
			for _, row := range rows {
				title := firstArticleLink(cellAt(row, nameCol))
				if title == "" {
					continue
				}
				locality := linkOrText(cellAt(row, locCol))
				if locality == "" {
					locality = headingScope
				}
				addCandidate(title, locality)
			}
			i = end
			continue
		}

		marker := listMarkerRe.FindStringSubmatch(trimmed)
		if marker == nil {
			continue
		}
		depth := len(marker[1])
		body := marker[2]

		// Localities recorded at this depth or deeper belonged to a previous
		// sibling and no longer apply.
		for d := range localities {
			if d >= depth {
				delete(localities, d)
			}
		}

		title := firstArticleLink(body)

		if isGroupHeader(lines, i, depth) {
			// "* [[Bourg-en-Bresse]]" heading a set of "** [[Museum]]" children
			// is a locality, not a museum.
			label := title
			if label == "" {
				label = plainText(body)
			}
			localities[depth] = label
			continue
		}

		if title == "" {
			continue
		}

		locality := headingScope
		for d := depth - 1; d >= 1; d-- {
			if v, ok := localities[d]; ok && v != "" {
				locality = v
				break
			}
		}
		addCandidate(title, locality)
	}

	return result
}

// include reports whether a title survives the configured blocklist.
func (m *MuseumExtractor) include(title string) bool {
	for _, bl := range m.blocklisted {
		if strings.HasPrefix(title, bl) {
			return false
		}
	}
	return true
}

// isGroupHeader reports whether the list item at lines[i] with the given depth
// is a grouping header, i.e. whether the next list item nests beneath it.
func isGroupHeader(lines []string, i, depth int) bool {
	for j := i + 1; j < len(lines); j++ {
		trimmed := strings.TrimSpace(lines[j])
		if trimmed == "" {
			continue
		}
		marker := listMarkerRe.FindStringSubmatch(trimmed)
		if marker == nil {
			// Any non-list content ends the list.
			return false
		}
		return len(marker[1]) > depth
	}
	return false
}

// firstArticleLink returns the first link in s that points at a normal article,
// skipping files, categories and interwiki links. Taking only the first such
// link keeps trailing context links ("[[Museum]] in [[City]]") out of the
// results.
func firstArticleLink(s string) string {
	for _, link := range parseLinks(s) {
		if isArticleLink(link.raw) {
			return link.Target
		}
	}
	return ""
}

// linkOrText returns the first article link in s, falling back to the cell's
// plain text when it holds no link.
func linkOrText(s string) string {
	if title := firstArticleLink(s); title != "" {
		return title
	}
	return plainText(s)
}

// plainText strips the remaining wiki markup from s, leaving display text.
func plainText(s string) string {
	out := s
	if links := parseLinks(s); len(links) > 0 {
		out = links[0].Display
	}
	out = strings.NewReplacer("'''", "", "''", "", "[[", "", "]]", "").Replace(out)
	return strings.TrimSpace(whitespaceRe.ReplaceAllString(out, " "))
}

// cellAt returns the cell at index idx, or "" when the row is shorter.
func cellAt(row []string, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	return row[idx]
}
