package scraper

import (
	"context"
	"log"
	"time"
)

const (
	// maxSubpages is the maximum number of subpages to crawl per museum.
	maxSubpages = 8
	// maxPDFs is the maximum number of PDFs to download per museum.
	maxPDFs = 3
)

// MuseumScraper fetches and parses museum websites to extract exhibitions,
// pricing, opening hours, and other operational data. It performs deep crawling
// across subpages and parses PDF documents.
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

// ScrapeMuseum performs a deep scrape of a museum website:
//  1. Fetch homepage → extract JSON-LD, meta tags, text patterns
//  2. Discover links to exhibition/visit/ticket subpages + PDFs
//  3. Probe common museum URL paths (/exhibitions, /visit, etc.)
//  4. Crawl discovered subpages for additional data
//  5. Download and extract text from PDFs (schedules, brochures)
//  6. Merge all results into a single ParsedWebData
func (s *MuseumScraper) ScrapeMuseum(ctx context.Context, websiteURL string) (*ParsedWebData, error) {
	if websiteURL == "" {
		return nil, errNoURL
	}

	// Phase 1: Fetch and parse homepage
	homepageHTML, err := s.fetcher.Fetch(ctx, websiteURL)
	if err != nil {
		return nil, err
	}

	result := extractPage(homepageHTML)
	if result == nil {
		result = &ParsedWebData{}
	}
	result.URL = websiteURL

	// Phase 2: Discover links from homepage
	discovered := DiscoverLinks(homepageHTML, websiteURL)
	log.Printf("Discovered %d relevant links on %s", len(discovered), websiteURL)

	// Separate PDF links from HTML links
	var htmlLinks, pdfLinks []DiscoveredLink
	for _, link := range discovered {
		if link.IsPDF {
			pdfLinks = append(pdfLinks, link)
		} else {
			htmlLinks = append(htmlLinks, link)
		}
	}

	// Phase 3: Probe common subpage paths if we didn't find enough links
	if len(htmlLinks) < 3 {
		probed := s.probeSubpages(ctx, websiteURL)
		// Deduplicate against already discovered links
		seen := make(map[string]bool)
		for _, l := range htmlLinks {
			seen[l.URL] = true
		}
		for _, p := range probed {
			if !seen[p] {
				seen[p] = true
				htmlLinks = append(htmlLinks, DiscoveredLink{URL: p})
			}
		}
	}

	// Phase 4: Crawl subpages
	crawled := 0
	for _, link := range htmlLinks {
		if ctx.Err() != nil || crawled >= maxSubpages {
			break
		}
		if link.URL == websiteURL {
			continue
		}

		pageData := s.scrapePage(ctx, link.URL)
		if pageData != nil {
			result = mergeWebData(result, pageData)
			crawled++
			log.Printf("Crawled subpage %s: +%d exhibitions, +%d offers",
				link.URL, len(pageData.Exhibitions), len(pageData.Offers))
		}
	}

	// Phase 5: Process PDFs
	pdfCount := 0
	for _, pdf := range pdfLinks {
		if ctx.Err() != nil || pdfCount >= maxPDFs {
			break
		}

		pdfData := s.scrapePDF(ctx, pdf.URL)
		if pdfData != nil {
			result = mergeWebData(result, pdfData)
			pdfCount++
			log.Printf("Extracted PDF %s: +%d offers, hours=%q",
				pdf.URL, len(pdfData.Offers), pdfData.OpeningHours)
		}
	}

	// Also check discovered subpages for PDF links
	if pdfCount < maxPDFs && crawled > 0 {
		// We already crawled some subpages — the PDF links from those
		// were not collected. In a future iteration we could store them.
	}

	log.Printf("Scrape complete for %s: %d exhibitions, %d offers, hours=%q (crawled %d subpages, %d PDFs)",
		websiteURL, len(result.Exhibitions), len(result.Offers), result.OpeningHours, crawled, pdfCount)

	if len(result.Exhibitions) == 0 && len(result.Offers) == 0 &&
		result.OpeningHours == "" && result.Description == "" {
		return nil, errNoData
	}

	return result, nil
}

// probeSubpages tries common museum URL paths and returns those that respond
// with 200 OK.
func (s *MuseumScraper) probeSubpages(ctx context.Context, baseURL string) []string {
	candidates := ProbeSubpages(baseURL)
	var found []string
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		if len(found) >= 4 {
			break
		}
		// Just try to fetch — if it 404s, the fetcher returns an error
		_, err := s.fetcher.Fetch(ctx, candidate)
		if err == nil {
			found = append(found, candidate)
		}
	}
	return found
}

// scrapePage fetches a single page and extracts data.
func (s *MuseumScraper) scrapePage(ctx context.Context, pageURL string) *ParsedWebData {
	html, err := s.fetcher.Fetch(ctx, pageURL)
	if err != nil {
		return nil
	}
	return extractPage(html)
}

// scrapePDF fetches and extracts text from a PDF, then runs text-pattern
// extraction on the result.
func (s *MuseumScraper) scrapePDF(ctx context.Context, pdfURL string) *ParsedWebData {
	pdfText, err := s.fetcher.FetchAndExtractPDF(ctx, pdfURL)
	if err != nil {
		log.Printf("PDF extraction failed for %s: %v", pdfURL, err)
		return nil
	}

	// Run the same text-pattern extractors on the PDF text
	return extractFromPlainText(pdfText.Text)
}

// extractPage runs all HTML extraction strategies on a page.
func extractPage(html string) *ParsedWebData {
	jsonLDData := ExtractJSONLD(html)
	htmlData := ExtractFromHTML(html)
	return mergeWebData(jsonLDData, htmlData)
}

// extractFromPlainText runs text-pattern extractors on plain text
// (e.g. from a PDF or plain text page).
func extractFromPlainText(text string) *ParsedWebData {
	if text == "" {
		return nil
	}

	data := &ParsedWebData{}

	// Extract prices
	matches := pricePattern.FindAllString(text, 10)
	seen := make(map[string]bool)
	for _, m := range matches {
		lower := toLower(m)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		if containsAny(lower, "free") {
			data.Offers = append(data.Offers, Offer{Name: "General Admission", Price: "0"})
		} else {
			data.Offers = append(data.Offers, Offer{Price: m})
		}
	}

	// Extract hours
	hourMatches := hoursPattern.FindAllString(text, 5)
	if len(hourMatches) > 0 {
		data.OpeningHours = joinStrings(hourMatches, "; ")
	}

	if len(data.Offers) == 0 && data.OpeningHours == "" {
		return nil
	}
	return data
}

// mergeWebData combines two ParsedWebData results.
// The first argument (primary) takes precedence when both have data.
func mergeWebData(primary, secondary *ParsedWebData) *ParsedWebData {
	if primary == nil && secondary == nil {
		return nil
	}
	if primary == nil {
		return secondary
	}
	if secondary == nil {
		return primary
	}

	if primary.Description == "" {
		primary.Description = secondary.Description
	}
	if primary.Image == "" {
		primary.Image = secondary.Image
	}
	if primary.OpeningHours == "" {
		primary.OpeningHours = secondary.OpeningHours
	}

	// Merge offers (dedup by price+name)
	if len(secondary.Offers) > 0 {
		existingOffers := make(map[string]bool)
		for _, o := range primary.Offers {
			existingOffers[o.Price+"|"+o.Name] = true
		}
		for _, o := range secondary.Offers {
			if !existingOffers[o.Price+"|"+o.Name] {
				primary.Offers = append(primary.Offers, o)
				existingOffers[o.Price+"|"+o.Name] = true
			}
		}
	}

	// Merge exhibitions (dedup by name)
	if len(secondary.Exhibitions) > 0 {
		existing := make(map[string]bool)
		for _, ex := range primary.Exhibitions {
			existing[ex.Name] = true
		}
		for _, ex := range secondary.Exhibitions {
			if !existing[ex.Name] {
				primary.Exhibitions = append(primary.Exhibitions, ex)
				existing[ex.Name] = true
			}
		}
	}

	return primary
}

// Small helpers to avoid importing strings in this file
func toLower(s string) string {
	b := make([]byte, len(s))
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func containsAny(s string, substr string) bool {
	return len(s) >= len(substr) && findSubstring(s, substr) >= 0
}

func findSubstring(s, sub string) int {
	for i := range len(s) - len(sub) + 1 {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func joinStrings(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	result := ss[0]
	for _, s := range ss[1:] {
		result += sep + s
	}
	return result
}
