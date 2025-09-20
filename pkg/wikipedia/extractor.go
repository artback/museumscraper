package wikipedia

import (
	"regexp"
	"strings"
)

var matchSymbol = regexp.MustCompile(":(.*?):")

type MuseumExtractor struct {
	blackListed []string
}

func NewMuseumExtractor(blackListed []string) *MuseumExtractor {
	return &MuseumExtractor{blackListed: blackListed}
}

func (m *MuseumExtractor) ExtractMuseums(content string) []string {
	var museums []string
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "#") {
			start := strings.Index(trimmed, "[[")
			end := strings.Index(trimmed, "]]")
			if start != -1 && end != -1 && end > start {
				link := trimmed[start+2 : end]
				if pipe := strings.Index(link, "|"); pipe != -1 {
					link = link[:pipe]
				}
				link = matchSymbol.ReplaceAllString(link, "")
				if m.include(link) {
					museums = append(museums, link)
				}
			}
		}
	}
	return museums
}

func (m *MuseumExtractor) include(s string) bool {
	for _, bl := range m.blackListed {
		if strings.HasPrefix(s, bl) {
			return false
		}
	}
	return true
}
