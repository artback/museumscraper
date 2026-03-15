package scraper

import (
	"context"
	"fmt"
	"log"
	"time"
)

// MuseumScraper fetches and parses museum websites to extract exhibitions,
// pricing, opening hours, and other operational data.
type MuseumScraper struct {
	fetcher *Fetcher
}

// NewMuseumScraper creates a scraper with the given request interval.
func NewMuseumScraper(requestInterval time.Duration) *MuseumScraper {
	return &MuseumScraper{
		fetcher: NewFetcher(requestInterval),
	}
}

// Close releases the scraper's resources.
func (s *MuseumScraper) Close() {
	s.fetcher.Close()
}

// ScrapeMuseum fetches a museum website and extracts all available structured
// data. It tries multiple extraction strategies in order of reliability:
//  1. JSON-LD / Schema.org structured data (most reliable)
//  2. HTML meta tags and Open Graph (metadata)
//  3. Text pattern matching (prices, hours)
func (s *MuseumScraper) ScrapeMuseum(ctx context.Context, websiteURL string) (*ParsedWebData, error) {
	if websiteURL == "" {
		return nil, fmt.Errorf("no website URL provided")
	}

	html, err := s.fetcher.Fetch(ctx, websiteURL)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", websiteURL, err)
	}

	// Strategy 1: JSON-LD structured data (most reliable)
	jsonLDData := ExtractJSONLD(html)

	// Strategy 2+3: HTML meta tags + text patterns
	htmlData := ExtractFromHTML(html)

	// Merge results, preferring JSON-LD data
	result := mergeWebData(jsonLDData, htmlData)
	if result == nil {
		return nil, fmt.Errorf("no structured data found on %s", websiteURL)
	}

	result.URL = websiteURL
	log.Printf("Scraped %s: %d exhibitions, %d offers, hours=%q",
		websiteURL, len(result.Exhibitions), len(result.Offers), result.OpeningHours)

	return result, nil
}

// mergeWebData combines JSON-LD and HTML extraction results.
// JSON-LD data takes precedence when available.
func mergeWebData(jsonLD, htmlMeta *ParsedWebData) *ParsedWebData {
	if jsonLD == nil && htmlMeta == nil {
		return nil
	}
	if jsonLD == nil {
		return htmlMeta
	}
	if htmlMeta == nil {
		return jsonLD
	}

	// JSON-LD is primary; fill gaps from HTML meta
	result := jsonLD

	if result.Description == "" {
		result.Description = htmlMeta.Description
	}
	if result.Image == "" {
		result.Image = htmlMeta.Image
	}
	if result.OpeningHours == "" {
		result.OpeningHours = htmlMeta.OpeningHours
	}

	// Merge offers if JSON-LD didn't have any
	if len(result.Offers) == 0 {
		result.Offers = htmlMeta.Offers
	}

	// Merge exhibitions from HTML extraction that aren't already in JSON-LD
	if len(htmlMeta.Exhibitions) > 0 {
		existing := make(map[string]bool)
		for _, ex := range result.Exhibitions {
			existing[ex.Name] = true
		}
		for _, ex := range htmlMeta.Exhibitions {
			if !existing[ex.Name] {
				result.Exhibitions = append(result.Exhibitions, ex)
			}
		}
	}

	return result
}
