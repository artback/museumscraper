package wikipedia

import (
	"regexp"
	"strings"
)

var matchSymbol = regexp.MustCompile(":(.*?):")

type MuseumExtractor struct {
	blocklisted []string
}

func NewMuseumExtractor(blocklisted []string) *MuseumExtractor {
	return &MuseumExtractor{blocklisted: blocklisted}
}

var linkRe = regexp.MustCompile(`\[\[(.+?)]\]`)

func (m *MuseumExtractor) ExtractMuseums(content string) []string {
	var museums []string
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "#") {
			matches := linkRe.FindAllStringSubmatch(trimmed, -1)
			for _, match := range matches {
				if len(match) < 2 {
					continue
				}
				link := match[1]
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
	for _, bl := range m.blocklisted {
		if strings.HasPrefix(s, bl) {
			return false
		}
	}
	return true
}
