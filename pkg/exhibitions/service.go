// Package exhibitions scrapes museum websites for what is currently on show.
//
// This exists because no open catalogue carries current exhibitions. Wikidata
// holds hundreds of thousands of exhibition items, but they are a historical
// record — a count of those with an end date in the future returns a few dozen
// worldwide. The only place a museum reliably publishes its current programme
// is its own website, so that is what this reads.
//
// Museum sites share no markup conventions and very few publish schema.org
// event data, so extraction is heuristic and its output should be treated as a
// lead rather than as a fact: every exhibition carries the URL it came from so
// a caller can link out to the museum's own page.
package exhibitions

import (
	"context"
	"errors"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"museum/internal/models"
)

// Exhibition is something on show at a museum.
type Exhibition struct {
	Title  string `json:"title"`
	URL    string `json:"url"`
	Museum string `json:"museum"`

	// Start and End are nil when the listing gave only one bound, or none.
	Start *time.Time `json:"start,omitempty"`
	End   *time.Time `json:"end,omitempty"`

	// Running is true when the dates cover the day the scrape ran.
	Running bool `json:"running"`
	// Upcoming is true when the exhibition opens later.
	Upcoming bool `json:"upcoming"`

	// SourcePage is the listing page the entry was read from, so a surprising
	// result can be traced back.
	SourcePage string `json:"source_page"`

	// MuseumWikidataID identifies the venue, where the museum record had one.
	MuseumWikidataID string `json:"museum_wikidata_id,omitempty"`
	// Latitude and Longitude are the venue's position, copied onto the
	// exhibition so it can be indexed and searched by location directly.
	Latitude  float64 `json:"latitude,omitempty"`
	Longitude float64 `json:"longitude,omitempty"`

	// ScrapedAt records when the listing was read, so a consumer can tell how
	// stale the answer is.
	ScrapedAt time.Time `json:"scraped_at"`
}

// Position reports the exhibition's location, satisfying geoindex.Located.
func (e Exhibition) Position() (lat, lon float64, ok bool) {
	if e.Latitude == 0 && e.Longitude == 0 {
		return 0, 0, false
	}
	return e.Latitude, e.Longitude, true
}

// maxListingPages bounds how many pages are tried per museum. The programme is
// almost always on the first or second candidate; more than this is a crawl.
const maxListingPages = 3

// Scraper reads exhibition listings from museum websites.
type Scraper struct {
	fetcher *Fetcher
	now     func() time.Time
}

// NewScraper returns a Scraper using a polite fetcher.
func NewScraper() *Scraper {
	return &Scraper{fetcher: NewFetcher(), now: time.Now}
}

// ErrNoWebsite means the museum record carries no site to scrape.
var ErrNoWebsite = errors.New("museum has no website")

// ForMuseum returns the exhibitions listed on a museum's own website.
//
// It looks for the programme page in two ways: the links the home page offers,
// and the conventional paths sites use ("/whats-on", "/exhibitions", and the
// non-English equivalents). Pages that fail, are disallowed by robots.txt, or
// yield nothing are skipped quietly — across thousands of sites, some fraction
// will always be unreachable.
func (s *Scraper) ForMuseum(ctx context.Context, museum models.Museum) ([]Exhibition, error) {
	if strings.TrimSpace(museum.Website) == "" {
		return nil, ErrNoWebsite
	}

	base, err := url.Parse(strings.TrimSpace(museum.Website))
	if err != nil || base.Host == "" {
		return nil, errors.New("museum website is not a usable URL")
	}
	if base.Scheme == "" {
		base.Scheme = "https"
	}

	now := s.now()
	var (
		found   []Exhibition
		visited = make(map[string]struct{})
	)

	for _, listingURL := range s.listingURLs(ctx, base) {
		if ctx.Err() != nil {
			return found, ctx.Err()
		}
		if len(visited) >= maxListingPages {
			break
		}
		if _, seen := visited[listingURL]; seen {
			continue
		}
		visited[listingURL] = struct{}{}

		body, finalURL, err := s.fetcher.Get(ctx, listingURL)
		if err != nil {
			continue
		}

		pageBase, err := url.Parse(finalURL)
		if err != nil {
			pageBase = base
		}

		for _, candidate := range ExtractCandidates(body, pageBase) {
			dates := datesFor(candidate, now)
			// An entry with no readable dates cannot be placed in time, and
			// listing pages are full of links that are not exhibitions at all;
			// requiring a date is what separates the two.
			if dates.IsZero() {
				continue
			}
			running, upcoming := dates.Runs(now), dates.Upcoming(now)
			if !running && !upcoming {
				continue // already closed
			}

			found = append(found, Exhibition{
				Title:            candidate.Title,
				URL:              candidate.URL,
				Museum:           museum.Name,
				Start:            dates.Start,
				End:              dates.End,
				Running:          running,
				Upcoming:         upcoming,
				SourcePage:       finalURL,
				MuseumWikidataID: museum.WikidataID,
				Latitude:         museum.Latitude,
				Longitude:        museum.Longitude,
				ScrapedAt:        now,
			})
		}

		if len(found) > 0 {
			// The programme was found; no need to try further candidates.
			break
		}
	}

	return dedupe(found), nil
}

// listingURLs returns the pages worth trying for a site's programme, the links
// its home page offers first and the conventional paths after.
func (s *Scraper) listingURLs(ctx context.Context, base *url.URL) []string {
	var urls []string

	home := *base
	home.Path = "/"
	home.RawQuery = ""
	if body, finalURL, err := s.fetcher.Get(ctx, home.String()); err == nil {
		pageBase, err := url.Parse(finalURL)
		if err != nil {
			pageBase = base
		}
		urls = append(urls, FindListingLinks(body, pageBase)...)
	}

	return append(urls, candidateListingURLs(base)...)
}

// dedupe removes repeated entries and orders them: running first, then by
// closing date so the ones about to end come first.
func dedupe(exhibitions []Exhibition) []Exhibition {
	seen := make(map[string]struct{}, len(exhibitions))
	unique := exhibitions[:0:0]

	for _, e := range exhibitions {
		key := strings.ToLower(e.Title) + "\x00" + e.URL
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, e)
	}

	sort.SliceStable(unique, func(i, j int) bool {
		if unique[i].Running != unique[j].Running {
			return unique[i].Running
		}
		switch {
		case unique[i].End == nil:
			return false
		case unique[j].End == nil:
			return true
		default:
			return unique[i].End.Before(*unique[j].End)
		}
	})
	return unique
}

// ForMuseums scrapes several museums concurrently, with concurrency bounded so
// the crawl stays polite. Museums without a website are skipped.
//
// Museums that share a website are scraped once. Institutions nest — the Musée
// Charles X is a wing of the Louvre and carries louvre.fr as its site, and
// Tate's four galleries share one domain — so without this the same programme
// is fetched repeatedly and every exhibition is reported once per museum
// sharing the site. The first museum given for a site keeps the attribution,
// so passing them in distance order attributes to the nearest.
func (s *Scraper) ForMuseums(ctx context.Context, museums []models.Museum, concurrency int) []Exhibition {
	var all []Exhibition
	s.Stream(ctx, museums, concurrency, func(found []Exhibition) {
		all = append(all, found...)
	})
	return all
}

// Stream scrapes each museum and hands its exhibitions to fn as soon as they
// are found, rather than returning everything at the end.
//
// This exists because holding a run's whole output in memory made the run
// all-or-nothing. A scrape of 6,000 sites took 68 minutes, found 9,148
// exhibitions, and stored none of them: the single write at the end hit one
// mis-encoded title and the entire batch was refused. A crash, a restart or a
// power cut at minute 67 would have cost exactly as much. Work that took an
// hour to produce should not depend on a later step succeeding.
//
// fn is called from one goroutine at a time and must not block for long: the
// workers are held up while it runs. Exhibitions are deduplicated by URL
// across the whole run before fn sees them, because sites cross-list — one
// venue's programme page carries entries another site also shows.
func (s *Scraper) Stream(ctx context.Context, museums []models.Museum, concurrency int, fn func([]Exhibition)) {
	if concurrency < 1 {
		concurrency = 4
	}
	museums = uniqueBySite(museums)

	jobs := make(chan models.Museum)
	// Buffered to the full job count so a worker can always deposit its result
	// and exit. With an unbuffered channel, a caller that stops collecting
	// early — on cancellation — would leave every in-flight worker blocked on
	// the send forever.
	results := make(chan []Exhibition, len(museums))

	var wg sync.WaitGroup
	for range min(concurrency, len(museums)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for museum := range jobs {
				found, err := s.ForMuseum(ctx, museum)
				if err != nil && !errors.Is(err, ErrNoWebsite) {
					log.Printf("exhibitions: %s: %v", museum.Name, err)
				}
				results <- found
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, museum := range museums {
			select {
			case jobs <- museum:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	seen := make(map[string]struct{})
	for found := range results {
		fresh := found[:0:0]
		for _, e := range found {
			if e.URL == "" {
				continue
			}
			if _, dup := seen[e.URL]; dup {
				continue
			}
			seen[e.URL] = struct{}{}
			fresh = append(fresh, e)
		}
		if len(fresh) > 0 {
			fn(fresh)
		}
	}
}

// uniqueBySite keeps the first museum for each distinct website, preserving
// order.
func uniqueBySite(museums []models.Museum) []models.Museum {
	seen := make(map[string]struct{}, len(museums))
	unique := museums[:0:0]

	for _, museum := range museums {
		site := siteKey(museum.Website)
		if site == "" {
			continue
		}
		if _, dup := seen[site]; dup {
			continue
		}
		seen[site] = struct{}{}
		unique = append(unique, museum)
	}
	return unique
}

// siteKey reduces a website URL to the host it serves from, so that
// "https://www.louvre.fr/en" and "https://www.louvre.fr/" count as one site.
func siteKey(website string) string {
	website = strings.TrimSpace(website)
	if website == "" {
		return ""
	}
	parsed, err := url.Parse(website)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(parsed.Host, "www."))
}
