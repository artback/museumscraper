package wikipedia

import "strings"

// nameColumnHints and localityColumnHints are matched against a wikitable's
// header cells, in order, to work out which column holds the museum name and
// which holds its town. Order matters: "name" is checked before "museum" so a
// table with both columns picks the more specific one.
var (
	nameColumnHints     = []string{"name", "museum", "title", "institution"}
	localityColumnHints = []string{"location", "city", "town", "place", "locality", "municipality", "settlement", "region", "county", "province", "district", "state"}
)

// parseTable reads the wikitable that starts at lines[start] (a "{|" line) and
// returns its data rows, its header cells, and the index of the line holding
// the table's closing "|}".
//
// Cell values are returned as raw wikitext; callers decide what to pull out of
// them. Nested tables are consumed as part of their enclosing cell rather than
// producing rows of their own.
func parseTable(lines []string, start int) (rows [][]string, headers []string, end int) {
	var (
		current    []string
		headerDone bool
		depth      = 1
	)

	flushRow := func() {
		if len(current) == 0 {
			return
		}
		if !headerDone {
			headers = current
			headerDone = true
		} else {
			rows = append(rows, current)
		}
		current = nil
	}

	i := start + 1
	for ; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])

		switch {
		case strings.HasPrefix(trimmed, "{|"):
			// A nested table belongs to the cell that opened it.
			depth++
			if len(current) > 0 {
				current[len(current)-1] += "\n" + trimmed
			}
			continue

		case strings.HasPrefix(trimmed, "|}"):
			depth--
			if depth == 0 {
				flushRow()
				return rows, headers, i
			}
			if len(current) > 0 {
				current[len(current)-1] += "\n" + trimmed
			}
			continue

		case depth > 1:
			if len(current) > 0 {
				current[len(current)-1] += "\n" + trimmed
			}
			continue

		case strings.HasPrefix(trimmed, "|-"):
			flushRow()
			continue

		case strings.HasPrefix(trimmed, "|+"):
			// Table caption; not a data cell.
			continue

		case strings.HasPrefix(trimmed, "|") || strings.HasPrefix(trimmed, "!"):
			current = append(current, splitCells(trimmed)...)
			continue

		case trimmed == "":
			continue

		default:
			// A continuation line belongs to the cell above it.
			if len(current) > 0 {
				current[len(current)-1] += "\n" + trimmed
			}
		}
	}

	flushRow()
	return rows, headers, len(lines) - 1
}

// splitCells splits a single wikitable line into its cells. A line begins with
// "|" or "!" and may pack several cells together with "||" or "!!".
func splitCells(line string) []string {
	sep := "||"
	if strings.HasPrefix(line, "!") {
		sep = "!!"
	}
	body := line[1:]

	var cells []string
	for _, raw := range splitOutsideMarkup(body, sep) {
		cells = append(cells, cellContent(raw))
	}
	return cells
}

// splitOutsideMarkup splits s on sep, ignoring separators that fall inside
// "[[...]]" links or "{{...}}" templates.
func splitOutsideMarkup(s, sep string) []string {
	var (
		parts []string
		last  int
		link  int
		tmpl  int
	)
	for i := 0; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], "[["):
			link++
			i++
		case strings.HasPrefix(s[i:], "]]") && link > 0:
			link--
			i++
		case strings.HasPrefix(s[i:], "{{"):
			tmpl++
			i++
		case strings.HasPrefix(s[i:], "}}") && tmpl > 0:
			tmpl--
			i++
		case link == 0 && tmpl == 0 && strings.HasPrefix(s[i:], sep):
			parts = append(parts, s[last:i])
			i += len(sep) - 1
			last = i + 1
		}
	}
	return append(parts, s[last:])
}

// cellContent strips a cell's HTML attributes, which MediaWiki separates from
// the content with a single "|" — as in `!scope="row" | [[Museum]]`. The pipe
// inside a wiki link must not be mistaken for that separator.
func cellContent(raw string) string {
	depth := 0
	for i := 0; i < len(raw); i++ {
		switch {
		case strings.HasPrefix(raw[i:], "[[") || strings.HasPrefix(raw[i:], "{{"):
			depth++
			i++
		case strings.HasPrefix(raw[i:], "]]") || strings.HasPrefix(raw[i:], "}}"):
			if depth > 0 {
				depth--
			}
			i++
		case depth == 0 && raw[i] == '|':
			// Only an attribute list contains "="; anything else is content.
			if strings.Contains(raw[:i], "=") {
				return strings.TrimSpace(raw[i+1:])
			}
			return strings.TrimSpace(raw)
		}
	}
	return strings.TrimSpace(raw)
}

// classifyColumns locates the name and locality columns from a table's header
// cells. The name column defaults to the first column, which is the convention
// in Wikipedia's museum tables; the locality column is -1 when absent.
func classifyColumns(headers []string) (nameCol, locCol int) {
	nameCol, locCol = 0, -1

	normalized := make([]string, len(headers))
	for i, h := range headers {
		normalized[i] = strings.ToLower(plainText(h))
	}

	for _, hint := range nameColumnHints {
		if idx := indexContaining(normalized, hint); idx != -1 {
			nameCol = idx
			break
		}
	}
	for _, hint := range localityColumnHints {
		if idx := indexContaining(normalized, hint); idx != -1 && idx != nameCol {
			locCol = idx
			break
		}
	}
	return nameCol, locCol
}

// indexContaining returns the index of the first entry containing sub, or -1.
func indexContaining(values []string, sub string) int {
	for i, v := range values {
		if strings.Contains(v, sub) {
			return i
		}
	}
	return -1
}
