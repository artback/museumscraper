package wikipedia

import (
	"context"
	"log"
	"museum/internal/models"
	"museum/pkg/geo"
	"strings"
)

// Stats summarises a crawl, so the operator can see whether the filters are
// discarding a sensible amount rather than silently losing museums.
type Stats struct {
	CategoriesVisited int
	PagesVisited      int
	Candidates        int
	Emitted           int
	Unverified        int
	Duplicates        int
	Rejected          map[Rejection]int
	WithCoordinates   int
}

// CategoryProcessor walks a Wikipedia category tree, extracts museums from
// every list page it finds, resolves each one against the API for its URL and
// coordinates, and streams the results.
type CategoryProcessor struct {
	svc       *CategoryService
	extractor *MuseumExtractor

	visited map[string]struct{}
	// emitted tracks museums already streamed, keyed by canonical article
	// title. Country lists and their city sub-lists overlap heavily, so without
	// this the same museum is emitted once per page that mentions it.
	emitted map[string]struct{}
	// pending holds museum list pages discovered inside other list pages, which
	// the category walk alone would miss. Each carries the country of the page
	// that linked it, because a title like "List of museums in Yerevan" names a
	// city and cannot supply one itself.
	pending []pendingPage
	stats   Stats
}

// pendingPage is a list page queued for processing, with the country inherited
// from wherever it was discovered.
type pendingPage struct {
	title   string
	country string
}

// NewCategoryProcessor returns a processor that pulls pages via svc and parses
// them with extractor.
func NewCategoryProcessor(svc *CategoryService, extractor *MuseumExtractor) *CategoryProcessor {
	return &CategoryProcessor{
		svc:       svc,
		extractor: extractor,
		visited:   make(map[string]struct{}),
		emitted:   make(map[string]struct{}),
		stats:     Stats{Rejected: make(map[Rejection]int)},
	}
}

// Stats returns the counters accumulated so far. It must not be called while a
// ProcessCategoryAsync stream is still running.
func (p *CategoryProcessor) Stats() Stats { return p.stats }

// ProcessCategoryAsync walks categoryTitle and streams every museum it finds.
// The returned channel is closed when the walk finishes or ctx is cancelled.
func (p *CategoryProcessor) ProcessCategoryAsync(ctx context.Context, categoryTitle string) <-chan models.Museum {
	out := make(chan models.Museum)
	go func() {
		defer close(out)
		p.walkCategory(ctx, categoryTitle, "", out)

		// Follow list pages that were linked from other list pages.
		for len(p.pending) > 0 && ctx.Err() == nil {
			page := p.pending[0]
			p.pending = p.pending[1:]
			if p.markVisited(page.title) {
				continue
			}
			p.processPage(ctx, page.title, page.country, out)
		}

		log.Printf("crawl finished: %d categories, %d pages, %d candidates -> %d museums (%d with coordinates, %d without an English article), %d duplicates, %d rejected",
			p.stats.CategoriesVisited, p.stats.PagesVisited, p.stats.Candidates,
			p.stats.Emitted, p.stats.WithCoordinates, p.stats.Unverified,
			p.stats.Duplicates, p.rejectedTotal())
		for reason, n := range p.stats.Rejected {
			log.Printf("  rejected %4d: %s", n, reason)
		}
	}()
	return out
}

// markVisited records title and reports whether it had already been seen.
func (p *CategoryProcessor) markVisited(title string) bool {
	if _, seen := p.visited[title]; seen {
		return true
	}
	p.visited[title] = struct{}{}
	return false
}

func (p *CategoryProcessor) rejectedTotal() int {
	total := 0
	for _, n := range p.stats.Rejected {
		total += n
	}
	return total
}

// walkCategory recursively visits a category's subcategories and processes
// every article it contains.
//
// countryHint carries the country named by an enclosing category
// ("Category:Lists of museums in France"), so that pages whose own title names
// only a city still get the right country.
func (p *CategoryProcessor) walkCategory(ctx context.Context, categoryTitle, countryHint string, out chan<- models.Museum) {
	if ctx.Err() != nil || p.markVisited(categoryTitle) {
		return
	}
	p.stats.CategoriesVisited++

	if country := geo.ExtractCountry(categoryTitle); country != "" && geo.IsCountry(country) {
		countryHint = country
	}

	members, err := p.svc.GetAllCategoryMembers(ctx, categoryTitle)
	if err != nil {
		log.Printf("skipping category %q: %v", categoryTitle, err)
		return
	}

	for _, member := range members {
		if ctx.Err() != nil {
			return
		}
		if member.NS == namespaceCategory {
			p.walkCategory(ctx, member.Title, countryHint, out)
			continue
		}
		if member.NS != namespaceArticle {
			continue
		}
		if p.markVisited(member.Title) {
			continue
		}
		p.processPage(ctx, member.Title, countryHint, out)
	}
}

// namespaceArticle and namespaceCategory are the MediaWiki namespace ids for
// ordinary articles and categories.
const (
	namespaceArticle  = 0
	namespaceCategory = 14
)

// processPage extracts, resolves and emits the museums named by a single list
// page. countryHint is used when the page's own title does not name a country.
func (p *CategoryProcessor) processPage(ctx context.Context, title, countryHint string, out chan<- models.Museum) {
	content, err := p.svc.GetPageContent(ctx, title)
	if err != nil {
		log.Printf("skipping page %q: %v", title, err)
		return
	}
	p.stats.PagesVisited++

	country := countryFor(title, countryHint)

	extraction := p.extractor.Extract(content)
	for _, nested := range extraction.NestedLists {
		p.pending = append(p.pending, pendingPage{title: nested, country: country})
	}
	if len(extraction.Candidates) == 0 {
		return
	}
	p.stats.Candidates += len(extraction.Candidates)

	titles := make([]string, 0, len(extraction.Candidates))
	for _, c := range extraction.Candidates {
		titles = append(titles, c.Title)
	}

	metadata, err := p.svc.ResolveTitles(ctx, titles)
	if err != nil {
		// Partial results are still usable; the unresolved titles simply come
		// back missing and get rejected below.
		log.Printf("partial metadata for %q: %v", title, err)
	}

	log.Printf("%s: %d candidates", title, len(extraction.Candidates))

	for _, candidate := range extraction.Candidates {
		meta, ok := metadata[candidate.Title]
		if !ok {
			// The batch lookup failed for this title; fall back to what the
			// list page gave us rather than dropping the museum.
			meta = PageMetadata{Title: candidate.Title, Missing: true}
		}

		decision, reason := Classify(meta)
		if decision == Reject {
			p.stats.Rejected[reason]++
			continue
		}
		if _, dup := p.emitted[meta.Title]; dup {
			p.stats.Duplicates++
			continue
		}
		p.emitted[meta.Title] = struct{}{}

		museum := models.Museum{
			Name:        meta.Title,
			Country:     country,
			Locality:    candidate.Locality,
			SourcePage:  title,
			Sources:     []string{ListSourceName},
			Verified:    decision == Accept,
			Description: meta.Description,
		}
		if decision == Accept {
			museum.WikipediaURL = meta.URL
			museum.WikidataID = meta.WikidataID
			museum.PageID = meta.PageID
			museum.Latitude = meta.Latitude
			museum.Longitude = meta.Longitude
		} else {
			p.stats.Unverified++
		}
		if meta.HasCoordinates && decision == Accept {
			p.stats.WithCoordinates++
		}
		p.stats.Emitted++

		select {
		case out <- museum:
		case <-ctx.Done():
			return
		}
	}
}

// countryFor derives the country for a list page.
//
// A title like "List of museums in France" names the country outright. A title
// like "List of museums in Yerevan" names a city, so the hint inherited from
// the enclosing category or the linking page is used instead. Only when neither
// yields a recognised country does the page's own place name get used verbatim.
func countryFor(pageTitle, countryHint string) string {
	place := geo.ExtractCountry(pageTitle)
	if canonical, ok := geo.Canonical(place); ok {
		return canonical
	}

	// "Museums of Spain" and similar shapes the preposition scan misses.
	if idx := strings.LastIndex(strings.ToLower(pageTitle), " of "); idx != -1 {
		if canonical, ok := geo.Canonical(strings.TrimSpace(pageTitle[idx+4:])); ok {
			return canonical
		}
	}

	if countryHint != "" {
		return countryHint
	}
	if place != "" {
		return place
	}
	return "unknown"
}
