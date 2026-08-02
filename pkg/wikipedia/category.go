package wikipedia

import (
	"context"
	"log"
	"sort"
	"strings"

	"museum/internal/models"
	"museum/pkg/geo"
)

// CategorySourceName identifies records discovered by the category crawl.
const CategorySourceName = "wikipedia-category"

// CategorySourceNameFor names the source for one edition's category crawl:
// "wikipedia-category" for English, "wikipedia-category-es" for Spanish.
//
// Each edition gets its own name so provenance survives the merge. That is not
// bookkeeping for its own sake — the hall-of-fame contamination was found and
// sized by grouping the catalogue by source, and an edition that turns out to
// be noisy needs to be identifiable the same way. English keeps the bare name
// so existing records, and anything that reads them, are unaffected.
func CategorySourceNameFor(lang string) string {
	if lang == "" || lang == DefaultLanguage {
		return CategorySourceName
	}
	return CategorySourceName + "-" + lang
}

// ListSourceName identifies records read off Wikipedia's "List of museums in …"
// pages. Records from that source carried no provenance at all, so 8,536
// museums in the catalogue could not be attributed to anything — and, more to
// the point, could not be found again when their quality came into question.
const ListSourceName = "wikipedia-list"

// RootMuseumCategory is the top of English Wikipedia's museum category tree.
const RootMuseumCategory = "Category:Museums by country"

// rootMuseumCategories is where the museum tree starts in each edition the
// crawl reads. Every edition names both the namespace and the topic in its own
// language, so none of this can be derived from the English title.
//
// The editions listed are the ones with the most museum articles English lacks:
// Italian, German and Japanese each hold over three thousand museums with no
// English article at all, and Spanish is what makes Latin America visible —
// Spanish Wikipedia has two to four times English's coverage there, and for
// El Salvador the catalogue's thinnest country it is 12 articles against 4.
// Every title here is Wikipedia's own langlink for the English root rather than
// a translation of it, and TestRootCategoriesExist checks they still resolve. A
// guessed title is not a wrong title, it is a silent one: "Category:国別の博物館"
// is a plausible rendering of "Museums by country", it does not exist, and the
// Japanese edition simply returned nothing at all.
var rootMuseumCategories = map[string]string{
	"en": RootMuseumCategory,
	"es": "Categoría:Museos por país",
	"de": "Kategorie:Museum nach Staat",
	"fr": "Catégorie:Musée par pays",
	"it": "Categoria:Musei per stato",
	"pt": "Categoria:Museus por país",
	"nl": "Categorie:Museum naar land",
	"pl": "Kategoria:Muzea według państw",
	"sv": "Kategori:Museer efter land",
	"ru": "Категория:Музеи по странам",
	"ja": "Category:各国の博物館",
	"zh": "Category:各國博物館",
	"uk": "Категорія:Музеї за країною",
	"cs": "Kategorie:Muzea podle zemí",
	"fi": "Luokka:Museot maittain",
	"da": "Kategori:Museer efter land",
	"ko": "분류:나라별 박물관",
	"tr": "Kategori:Ülkelerine göre müzeler",
}

// RootCategoryFor returns the museum category tree's root in one edition, and
// whether that edition is one the crawl knows how to walk.
func RootCategoryFor(lang string) (string, bool) {
	root, ok := rootMuseumCategories[lang]
	return root, ok
}

// Languages returns every edition the crawl can walk, English first and the
// rest in a stable order so a crawl's logs are comparable between runs.
func Languages() []string {
	langs := make([]string, 0, len(rootMuseumCategories))
	for lang := range rootMuseumCategories {
		if lang != DefaultLanguage {
			langs = append(langs, lang)
		}
	}
	sort.Strings(langs)
	return append([]string{DefaultLanguage}, langs...)
}

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

// memberListTerms mark subcategories that list who or what an institution
// honours, rather than institutions.
//
// A hall of fame is a museum, so "Category:Australian Football Hall of Fame
// inductees" sits legitimately inside the museum tree — but its members are
// footballers. Walking those categories put 5,657 records into the catalogue,
// 3% of it: 394 footballers, 307 racehorses, 323 Grammy-winning songs, 89
// ballot articles and 69 cuneiform signs.
//
// Classify now rejects those on their descriptions, which is the safety net and
// catches them whatever category they arrive from. This list is the other half:
// it stops the crawler walking into a category whose members cannot be museums,
// which saves several thousand pointless title resolutions per crawl.
//
// The terms match the category's own wording rather than "hall of fame", which
// must keep working — "Category:Halls of fame in Texas" holds real museums, and
// the Rodeo and Dirt Modified halls of fame in the catalogue are genuine.
var memberListTerms = []string{
	"inductees", "inductee", "honorees", "honourees", "balloting",
	"award recipients", "award winners", "hall of fame members",
	"hall of fame players", "hall of fame horses",
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
	lang      string
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
		lang:      svc.Language(),
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
			// Backed by an article in the edition that was crawled. It used to
			// mean an *English* article specifically, which made the flag a
			// measure of anglophone coverage rather than of confidence: a museum
			// in Italy with a full Italian article was "unverified", and two
			// thirds of the Wikipedia-documented catalogue is in that position.
			Verified: true,
			Sources:  []string{CategorySourceNameFor(c.lang)},
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
	for _, term := range memberListTerms {
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
