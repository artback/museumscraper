package scraper

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// Meta tag patterns
var (
	// Price patterns: "$15", "€20", "£10", "15 EUR", "20 USD", "free admission", etc.
	pricePattern = regexp.MustCompile(`(?i)(?:` +
		`[$€£¥][\s]*\d+(?:[.,]\d{1,2})?` + // $15, €20.00
		`|\d+(?:[.,]\d{1,2})?\s*(?:USD|EUR|GBP|CHF|SEK|NOK|DKK|PLN|CZK|HUF|RON|BGN|HRK|TRY|JPY|CNY|KRW|AUD|CAD|NZD|BRL|MXN|ARS|CLP|COP|PEN)` + // 15 EUR
		`|free\s+(?:admission|entry|entrance)` + // free admission
		`|admission\s+(?:is\s+)?free` + // admission is free
		`|no\s+(?:admission|entry)\s+(?:fee|charge)` + // no admission fee
		`)`)

	// Opening hours patterns
	hoursPattern = regexp.MustCompile(`(?i)(?:` +
		`(?:mon|tue|wed|thu|fri|sat|sun)[a-z]*[\s]*[-–][\s]*(?:mon|tue|wed|thu|fri|sat|sun)[a-z]*[\s:]*\d{1,2}[:.]\d{2}[\s]*[-–][\s]*\d{1,2}[:.]\d{2}` + // Mon-Fri: 10:00-17:00
		`|(?:open|hours)[\s:]*\d{1,2}[:.]\d{2}[\s]*[-–][\s]*\d{1,2}[:.]\d{2}` + // Open: 10:00-17:00
		`|\d{1,2}(?::\d{2})?\s*(?:am|pm)\s*[-–]\s*\d{1,2}(?::\d{2})?\s*(?:am|pm)` + // 10am-5pm
		`)`)

	// Exhibition section indicators
	exhibitionHeaders = regexp.MustCompile(`(?i)<h[1-4][^>]*>[^<]*(?:exhibition|exhibit|on\s+view|current|upcoming|now\s+showing|what'?s\s+on)[^<]*</h[1-4]>`)
)

// ExtractFromHTML parses the HTML document and extracts metadata using
// meta tags, Open Graph, and text pattern matching.
func ExtractFromHTML(rawHTML string) *ParsedWebData {
	data := &ParsedWebData{}

	// Extract meta tags
	extractMetaTags(rawHTML, data)

	// Extract prices from page text
	extractPrices(rawHTML, data)

	// Extract opening hours from page text
	extractHours(rawHTML, data)

	// Extract exhibition-like content from headings and surrounding context
	extractExhibitionContent(rawHTML, data)

	if data.Description == "" && data.Image == "" && len(data.Exhibitions) == 0 &&
		len(data.Offers) == 0 && data.OpeningHours == "" {
		return nil
	}
	return data
}

func extractMetaTags(rawHTML string, data *ParsedWebData) {
	tokenizer := html.NewTokenizer(strings.NewReader(rawHTML))
	for {
		tt := tokenizer.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}

		token := tokenizer.Token()
		if token.Data != "meta" {
			continue
		}

		var name, property, content string
		for _, a := range token.Attr {
			switch a.Key {
			case "name":
				name = strings.ToLower(a.Val)
			case "property":
				property = strings.ToLower(a.Val)
			case "content":
				content = a.Val
			}
		}

		if content == "" {
			continue
		}

		switch {
		case property == "og:description" || name == "description":
			if data.Description == "" {
				data.Description = content
			}
		case property == "og:image":
			if data.Image == "" {
				data.Image = content
			}
		case property == "og:url":
			if data.URL == "" {
				data.URL = content
			}
		}
	}
}

func extractPrices(rawHTML string, data *ParsedWebData) {
	// Strip HTML tags for text extraction
	text := stripTags(rawHTML)

	matches := pricePattern.FindAllString(text, 10)
	seen := make(map[string]bool)
	for _, m := range matches {
		m = strings.TrimSpace(m)
		lower := strings.ToLower(m)
		if seen[lower] {
			continue
		}
		seen[lower] = true

		if strings.Contains(lower, "free") {
			data.Offers = append(data.Offers, Offer{
				Name:  "General Admission",
				Price: "0",
			})
		} else {
			data.Offers = append(data.Offers, Offer{
				Price: m,
			})
		}
	}
}

func extractHours(rawHTML string, data *ParsedWebData) {
	if data.OpeningHours != "" {
		return
	}
	text := stripTags(rawHTML)
	matches := hoursPattern.FindAllString(text, 5)
	if len(matches) > 0 {
		data.OpeningHours = strings.Join(matches, "; ")
	}
}

func extractExhibitionContent(rawHTML string, data *ParsedWebData) {
	// Find exhibition section headers and extract nearby links/titles
	headerMatches := exhibitionHeaders.FindAllStringIndex(rawHTML, 5)
	for _, loc := range headerMatches {
		// Look at content after the header (up to 2000 chars or next h1-h4)
		start := loc[1]
		end := start + 2000
		if end > len(rawHTML) {
			end = len(rawHTML)
		}
		section := rawHTML[start:end]

		// Find exhibition titles in links within this section
		exhibitions := extractExhibitionsFromSection(section)
		data.Exhibitions = append(data.Exhibitions, exhibitions...)
	}
}

// extractExhibitionsFromSection pulls exhibition names from links and headings
// within an HTML section.
func extractExhibitionsFromSection(section string) []Exhibition {
	var exhibitions []Exhibition
	seen := make(map[string]bool)

	tokenizer := html.NewTokenizer(strings.NewReader(section))
	var inLink bool
	var currentURL string
	depth := 0

	for {
		tt := tokenizer.Next()
		if tt == html.ErrorToken {
			break
		}

		switch tt {
		case html.StartTagToken:
			token := tokenizer.Token()
			depth++

			// Stop at next major heading (new section)
			if depth > 0 && (token.Data == "h1" || token.Data == "h2" || token.Data == "h3") {
				if depth > 1 { // Don't stop at the first heading
					return exhibitions
				}
			}

			if token.Data == "a" {
				inLink = true
				for _, a := range token.Attr {
					if a.Key == "href" {
						currentURL = a.Val
					}
				}
			}

		case html.TextToken:
			if inLink {
				text := strings.TrimSpace(tokenizer.Token().Data)
				if text != "" && len(text) > 3 && len(text) < 200 && !seen[text] {
					seen[text] = true
					// Filter out generic navigation links
					lower := strings.ToLower(text)
					if !isNavText(lower) {
						exhibitions = append(exhibitions, Exhibition{
							Name: text,
							URL:  currentURL,
						})
					}
				}
			}

		case html.EndTagToken:
			token := tokenizer.Token()
			if token.Data == "a" {
				inLink = false
				currentURL = ""
			}
			depth--
		}
	}

	return exhibitions
}

// isNavText returns true for generic navigation text that shouldn't be
// treated as exhibition names.
func isNavText(text string) bool {
	navWords := []string{
		"read more", "learn more", "see more", "view all", "buy tickets",
		"book now", "get tickets", "plan your visit", "back to top",
		"home", "contact", "about", "menu", "search", "close",
		"sign up", "subscribe", "newsletter", "cookie", "privacy",
	}
	for _, nav := range navWords {
		if strings.Contains(text, nav) {
			return true
		}
	}
	return false
}

// stripTags removes HTML tags and returns plain text.
func stripTags(s string) string {
	var b strings.Builder
	tokenizer := html.NewTokenizer(strings.NewReader(s))
	for {
		tt := tokenizer.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt == html.TextToken {
			b.WriteString(tokenizer.Token().Data)
			b.WriteByte(' ')
		}
	}
	return b.String()
}
