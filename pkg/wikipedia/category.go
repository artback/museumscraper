package wikipedia

import (
	"context"
	"log"
	"strings"

	"museum/internal/models"
	"museum/pkg/geo"
)

// CategorySourceName identifies records discovered by the category crawl.
const CategorySourceName = "wikipedia-category"

// ListSourceName identifies records read off Wikipedia's "List of museums in …"
// pages. Records from that source carried no provenance at all, so 8,536
// museums in the catalogue could not be attributed to anything — and, more to
// the point, could not be found again when their quality came into question.
const ListSourceName = "wikipedia-list"

// RootMuseumCategory is the top of Wikipedia's museum category tree.
const RootMuseumCategory = "Category:Museums by country"

// defaultMaxDepth bounds the recursion. Wikipedia's category graph is not a
// tree — it has cycles and drifts into loosely related topics after a few
// levels — so the walk needs a hard floor as well as the skip rules below.
const defaultMaxDepth = 8

// skippedCategoryTerms mark subcategories that hold things *about* museums
// rather than museums: their staff, their holdings, the artworks inside them.
//
// The terms are deliberately specific. A blunter list would do real damage,
// because museums are named after the very things that look like noise:
// "print" appears in "Printing museums", "film" in "Film museums", "book" in
// "Book museums", "media" in "Media museums". Plural and phrase forms
// distinguish "Museum directors" from "Film museums".
var skippedCategoryTerms = []string{
	"curators", "directors", "founders", "employees", "staff",
	"people", "biographies", "architects", "collectors", "donors",
	"stubs", "templates", "logos", "images of", "photographs of",
	"lists of", "list of", "index of", "awards", "wikipedia",
	"navigational boxes", "redirects",
}

// skippedCategoryPrefixes mark subcategories whose members are holdings and
// works rather than the institutions themselves.
var skippedCategoryPrefixes = []string{
	"collections of", "collection of", "artworks", "paintings", "sculptures",
	"drawings", "engravings", "manuscripts", "works ", "exhibitions",
	"publications", "acquisitions",
}

// CategoryCrawler discovers museums by walking Wikipedia's museum category
// tree.
//
// This finds far more than the "List of museums in X" articles do: a museum
// only appears in a list if somebody added it there, whereas nearly every
// museum article is filed under a museum category. The two sources overlap
// heavily but neither is a superset of the other, so both are worth running.
type CategoryCrawler struct {
	svc       *CategoryService
	maxDepth  int
	visited   map[string]struct{}
	emitted   map[string]struct{}
	stats     Stats
	batchSize int
}

// NewCategoryCrawler returns a crawler backed by svc.
func NewCategoryCrawler(svc *CategoryService) *CategoryCrawler {
	return &CategoryCrawler{
		svc:       svc,
		maxDepth:  defaultMaxDepth,
		visited:   make(map[string]struct{}),
		emitted:   make(map[string]struct{}),
		stats:     Stats{Rejected: make(map[Rejection]int)},
		batchSize: maxTitlesPerQuery,
	}
}

// Stats returns the counters accumulated so far. It must not be called while a
// Museums stream is still running.
func (c *CategoryCrawler) Stats() Stats { return c.stats }

// Museums walks the category tree from root and streams every museum article
// it finds. The channel closes when the walk finishes or ctx is cancelled.
func (c *CategoryCrawler) Museums(ctx context.Context, root string) <-chan models.Museum {
	out := make(chan models.Museum)

	go func() {
		defer close(out)
		c.walk(ctx, root, "", 0, out)

		log.Printf("category crawl finished: %d categories, %d candidates -> %d museums (%d with coordinates), %d duplicates, %d rejected",
			c.stats.CategoriesVisited, c.stats.Candidates, c.stats.Emitted,
			c.stats.WithCoordinates, c.stats.Duplicates, c.rejectedTotal())
		for reason, n := range c.stats.Rejected {
			log.Printf("  rejected %5d: %s", n, reason)
		}
	}()

	return out
}

func (c *CategoryCrawler) rejectedTotal() int {
	total := 0
	for _, n := range c.stats.Rejected {
		total += n
	}
	return total
}

// walk visits one category: it emits the museums among its articles and
// recurses into the subcategories worth following.
func (c *CategoryCrawler) walk(ctx context.Context, category, countryHint string, depth int, out chan<- models.Museum) {
	if ctx.Err() != nil || depth > c.maxDepth {
		return
	}
	if _, seen := c.visited[category]; seen {
		return
	}
	c.visited[category] = struct{}{}
	c.stats.CategoriesVisited++

	if country := categoryCountry(category); country != "" {
		countryHint = country
	}

	members, err := c.svc.GetAllCategoryMembers(ctx, category)
	if err != nil {
		log.Printf("category crawl: skipping %q: %v", category, err)
		return
	}

	var articles []string
	for _, member := range members {
		switch member.NS {
		case namespaceArticle:
			articles = append(articles, member.Title)
		case namespaceCategory:
			if skipCategory(member.Title) {
				continue
			}
			c.walk(ctx, member.Title, countryHint, depth+1, out)
		}
	}

	c.emitArticles(ctx, articles, countryHint, category, out)
}

// emitArticles resolves a category's articles in batches and emits the museums.
func (c *CategoryCrawler) emitArticles(ctx context.Context, titles []string, country, sourceCategory string, out chan<- models.Museum) {
	fresh := titles[:0:0]
	for _, title := range titles {
		if _, done := c.emitted[title]; done {
			c.stats.Duplicates++
			continue
		}
		fresh = append(fresh, title)
	}
	if len(fresh) == 0 {
		return
	}
	c.stats.Candidates += len(fresh)

	metadata, err := c.svc.ResolveTitles(ctx, fresh)
	if err != nil {
		log.Printf("category crawl: partial metadata under %q: %v", sourceCategory, err)
	}

	for _, title := range fresh {
		meta, ok := metadata[title]
		if !ok {
			meta = PageMetadata{Title: title, Missing: true}
		}

		decision, reason := Classify(meta)
		if decision == Reject {
			c.stats.Rejected[reason]++
			continue
		}
		// A category member that resolves to nothing is a redirect gone stale,
		// not a museum somebody forgot to write up.
		if decision == AcceptUnverified {
			c.stats.Rejected[RejectUnresolvable]++
			continue
		}
		if _, dup := c.emitted[meta.Title]; dup {
			c.stats.Duplicates++
			continue
		}
		c.emitted[meta.Title] = struct{}{}
		c.emitted[title] = struct{}{}

		museum := models.Museum{
			Name:         meta.Title,
			Country:      orUnknown(country),
			Description:  meta.Description,
			WikipediaURL: meta.URL,
			WikidataID:   meta.WikidataID,
			PageID:       meta.PageID,
			Latitude:     meta.Latitude,
			Longitude:    meta.Longitude,
			SourcePage:   sourceCategory,
			Verified:     true,
			Sources:      []string{CategorySourceName},
		}
		if meta.HasCoordinates {
			c.stats.WithCoordinates++
		}
		c.stats.Emitted++

		select {
		case out <- museum:
		case <-ctx.Done():
			return
		}
	}
}

// skipCategory reports whether a subcategory holds something other than
// museums and should not be walked.
func skipCategory(title string) bool {
	name := strings.ToLower(strings.TrimPrefix(title, "Category:"))
	for _, term := range skippedCategoryTerms {
		if strings.Contains(name, term) {
			return true
		}
	}
	for _, prefix := range skippedCategoryPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// categoryCountry pulls a country out of a category title such as
// "Category:Museums in France".
func categoryCountry(title string) string {
	name := strings.TrimPrefix(title, "Category:")
	if canonical, ok := geo.Canonical(geo.ExtractCountry(name)); ok {
		return canonical
	}
	return ""
}

// orUnknown replaces an empty country with the placeholder used in object keys.
func orUnknown(country string) string {
	if country == "" {
		return "unknown"
	}
	return country
}
