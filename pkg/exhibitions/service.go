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
	"slices"
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

	// Permanent is true for a display the museum keeps out indefinitely. Such
	// an entry carries no dates, and is running by definition: it has none
	// because it has no end, not because the listing failed to give them. A
	// caller telling a visitor what they can see today wants these alongside
	// the temporary shows; one deciding what to catch before it closes does
	// not, and the flag is how the two are told apart.
	Permanent bool `json:"permanent,omitempty"`

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

// maxPermanentPages bounds the second pass, over the pages a home page named
// as holding permanent displays. Sites keep one such page, occasionally two —
// one per building.
const maxPermanentPages = 2

// maxInfoPages bounds the search for a museum's description of itself. It runs
// only for the sites that yielded no programme at all, so it is spent on the
// museums that would otherwise contribute nothing rather than added to every
// scrape.
const maxInfoPages = 2

// Fallback reads a museum's site by some other means when the heuristics here
// have found nothing.
//
// The interface is declared here, where it is called, so that this package
// stays what it has always been: a self-contained heuristic scraper with no
// dependency on the machinery that might one day replace it. The catalogue
// wires in the generated-extractor harness; the tests wire in nothing, and
// every existing behaviour is unchanged when Fallback is nil.
type Fallback interface {
	ForMuseum(ctx context.Context, museum models.Museum) ([]Exhibition, error)
}

// Scraper reads exhibition listings from museum websites.
type Scraper struct {
	fetcher *Fetcher
	now     func() time.Time

	// Fallback is consulted only for museums this scraper could read nothing
	// from. Nil disables it, which is the default and what every test uses.
	Fallback Fallback
}

// NewScraper returns a Scraper using a polite fetcher.
func NewScraper() *Scraper {
	return &Scraper{fetcher: NewFetcher(), now: time.Now}
}

// ErrNoWebsite means the museum record carries no site to scrape.
var ErrNoWebsite = errors.New("museum has no website")

// ForMuseum returns the exhibitions listed on a museum's own website, both the
// temporary programme and whatever is permanently on show.
//
// It looks for the programme page in two ways: the links the home page offers,
// and the conventional paths sites use ("/whats-on", "/exhibitions",
// "/permanent", and the non-English equivalents). Pages that fail, are
// disallowed by robots.txt, or yield nothing are skipped quietly — across
// thousands of sites, some fraction will always be unreachable.
//
// A site that lists nothing at all falls through to permanentDisplay, which
// reads the museum's own description of itself. See permanent.go for why.
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
	home := s.readHome(ctx, base)

	var (
		found   []Exhibition
		visited = make(map[string]struct{})
	)

	for _, listingURL := range slices.Concat(home.listings, candidateListingURLs(base)) {
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

		found = append(found, s.harvest(ctx, listingURL, base, museum, now, false)...)
		if len(found) > 0 {
			// The programme was found; no need to try further candidates.
			break
		}
	}

	// The permanent displays are a second pass, not more candidates for the
	// first, because the loop above stops the moment it finds anything. On a
	// site that publishes both, what it finds is the temporary programme —
	// that is what a home page leads with — and it then never looks at the
	// permanent page sitting beside it.
	//
	// Only links the home page itself labelled permanent are followed, so a
	// site with no such page costs nothing: guessing at conventional paths
	// would spend a request per miss on every museum in the catalogue.
	permanentPages := 0
	for _, listingURL := range home.permanent {
		if ctx.Err() != nil {
			return dedupe(found), ctx.Err()
		}
		// Counted separately from the pages above rather than sharing their
		// budget: a site whose programme was found on the first try would
		// otherwise be allowed four more requests here.
		if permanentPages >= maxPermanentPages {
			break
		}
		if _, seen := visited[listingURL]; seen {
			continue
		}
		visited[listingURL] = struct{}{}
		permanentPages++

		found = append(found, s.harvest(ctx, listingURL, base, museum, now, true)...)
	}

	// Everything above is free and language-bound. When it has come back with
	// nothing, and only then, the generated-extractor fallback is worth its
	// cost — which is a model invocation the first time a site is seen, and a
	// sandboxed script execution every time after.
	//
	// The ordering is the whole point: this runs on the sites the heuristics
	// could not read, not on the thousands they read perfectly well. Putting
	// it first would be a language model reading six thousand museum websites
	// a night to rediscover what a CSS selector already knew.
	if len(found) == 0 && s.Fallback != nil {
		switch generated, err := s.Fallback.ForMuseum(ctx, museum); {
		case err != nil:
			// A fallback that cannot run is not a scrape that failed. The
			// museum still gets its permanent display below.
			log.Printf("exhibitions: fallback extractor for %s: %v", museum.Name, err)
		default:
			found = append(found, generated...)
		}
	}

	if len(found) == 0 {
		if display, ok := s.permanentDisplay(ctx, base, home, museum, now); ok {
			found = append(found, display)
		}
	}

	return dedupe(found), nil
}

// harvest reads one listing page and returns the exhibitions on it.
//
// assumePermanent says the page was reached by a link that named it a page of
// permanent displays, which is a claim its own markup often does not repeat: a
// page headed "Fasta utställningar" lists entries that say nothing about their
// own permanence, because the heading already did.
func (s *Scraper) harvest(ctx context.Context, listingURL string, base *url.URL, museum models.Museum, now time.Time, assumePermanent bool) []Exhibition {
	body, finalURL, err := s.fetcher.Get(ctx, listingURL)
	if err != nil {
		return nil
	}

	pageBase, err := url.Parse(finalURL)
	if err != nil {
		pageBase = base
	}
	// Whether this page holds permanent displays is a property of the page,
	// read once: its entries carry no dates and mostly do not repeat the label
	// the page already gave them.
	pagePermanent := assumePermanent || isPermanentListing(body, pageBase)

	var found []Exhibition
	for _, candidate := range candidatesOn(body, pageBase) {
		dates := datesFor(candidate, now)
		// An entry with no readable dates cannot be placed in time, and listing
		// pages are full of links that are not exhibitions at all; requiring a
		// date is what separates the two. A permanent display is the exception,
		// and has to name itself one to be kept: otherwise the rule that keeps
		// the noise out is gone.
		permanent := dates.Permanent
		if dates.IsZero() && !permanent {
			if !candidateIsPermanent(candidate, pagePermanent) {
				continue
			}
			permanent = true
		}

		running, upcoming := dates.Runs(now), dates.Upcoming(now)
		if permanent {
			running, upcoming = true, false
		}
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
			Permanent:        permanent,
			SourcePage:       finalURL,
			MuseumWikidataID: museum.WikidataID,
			Latitude:         museum.Latitude,
			Longitude:        museum.Longitude,
			ScrapedAt:        now,
		})
	}
	return found
}

// permanentDisplay returns the museum itself as a single permanent entry, for
// the many museums that publish no programme to read.
//
// Radiomuseet in Göteborg is the shape this is for. It has no exhibitions page,
// no calendar and no date on the site at all; what it has is a museum
// information page describing crystal sets, a 1960 living room, military radio
// and a workshop, all permanently on show. Nothing above finds any of that, and
// the museum comes back empty — which reads, to anyone asking what is on near
// them, as though there were nothing to see.
//
// One entry is the right granularity. Breaking such a site into an entry per
// room would mean deciding which of a site's dozens of pages are display areas
// and which are the shop, the newsletter and the committee, and there is no
// signal that survives contact with more than one site. The museum is the thing
// permanently on show; the page describing it is where a visitor should be sent.
func (s *Scraper) permanentDisplay(ctx context.Context, base *url.URL, home homePage, museum models.Museum, now time.Time) (Exhibition, bool) {
	tried := make(map[string]struct{}, maxInfoPages)

	// A page the site itself called its permanent exhibition comes first, and
	// is a better answer than the visitor information: the Jewish Museum
	// Berlin's "/dauerausstellung" describes what is on show, while its
	// "/rund-um-den-besuch" describes the cloakroom. It reaches here rather
	// than being listed above only when it lists no entries — which is the
	// usual case, because a museum with one permanent exhibition writes a page
	// about it rather than a page of links to it.
	for _, infoURL := range slices.Concat(home.permanent, home.info, infoURLs(base)) {
		if ctx.Err() != nil {
			return Exhibition{}, false
		}
		if len(tried) >= maxInfoPages {
			break
		}
		if _, seen := tried[infoURL]; seen {
			continue
		}
		tried[infoURL] = struct{}{}

		body, finalURL, err := s.fetcher.Get(ctx, infoURL)
		if err != nil {
			continue
		}
		// The page has to actually describe something on show. Without this a
		// site whose "/about" is the history of the founding association would
		// be recorded as having a permanent display on the strength of the URL
		// alone.
		if !describesDisplay(body) {
			continue
		}

		return Exhibition{
			// The museum's own name, because the museum is what is on show. A
			// page heading here is "Museum information" as often as anything,
			// and would make a worse title than the thing it describes.
			Title:            museum.Name,
			URL:              finalURL,
			Museum:           museum.Name,
			Running:          true,
			Permanent:        true,
			SourcePage:       finalURL,
			MuseumWikidataID: museum.WikidataID,
			Latitude:         museum.Latitude,
			Longitude:        museum.Longitude,
			ScrapedAt:        now,
		}, true
	}

	return Exhibition{}, false
}

// widen stretches a kept entry to cover another occurrence of the same event,
// so four one-day listings become one entry spanning all four days.
//
// A permanent display is left alone: its dates are absent because it has none,
// and taking a same-named dated listing's bounds would turn "always on" into a
// run that ends.
func widen(kept *Exhibition, other Exhibition) {
	if kept.Permanent {
		return
	}
	if other.Start != nil && (kept.Start == nil || other.Start.Before(*kept.Start)) {
		kept.Start = other.Start
	}
	if other.End != nil && (kept.End == nil || other.End.After(*kept.End)) {
		kept.End = other.End
	}
}

// homePage is what a site's front page points at, read once.
//
// Both searches start here — the programme, and failing that the museum's
// description of itself — and the front page is not worth fetching twice.
type homePage struct {
	// listings are the links that look like a programme, best first.
	listings []string
	// permanent are the links the page labelled as leading to displays that
	// are always on.
	permanent []string
	// info are the links that look like the museum describing itself and what
	// it holds, best first.
	info []string
}

// readHome fetches a site's front page and sorts its links. A front page that
// cannot be read is not an error: the conventional paths are tried regardless.
func (s *Scraper) readHome(ctx context.Context, base *url.URL) homePage {
	home := *base
	home.Path = "/"
	home.RawQuery = ""
	home.Fragment = ""

	body, finalURL, err := s.fetcher.Get(ctx, home.String())
	if err != nil {
		return homePage{}
	}

	pageBase, err := url.Parse(finalURL)
	if err != nil {
		pageBase = base
	}
	return homePage{
		listings:  FindListingLinks(body, pageBase),
		permanent: FindPermanentLinks(body, pageBase),
		info:      FindInfoLinks(body, pageBase),
	}
}

// dedupe removes repeated entries and orders them: running first, then by
// closing date so the ones about to end come first.
func dedupe(exhibitions []Exhibition) []Exhibition {
	// Keyed on the title alone, not the title and URL.
	//
	// Museums publish recurring events as one entry per occurrence, each with
	// its own URL: Kalmar konstmuseum listed "Konstparken" four times across
	// four days, and Hasselblad Center listed one exhibition's guided tours
	// seven times. Keying on the URL treated every occurrence as a separate
	// exhibition. Merging them keeps one entry spanning the whole run, which is
	// what a visitor is actually asking about.
	index := make(map[string]int, len(exhibitions))
	unique := exhibitions[:0:0]

	for _, e := range exhibitions {
		key := strings.ToLower(strings.TrimSpace(e.Title))
		if at, dup := index[key]; dup {
			widen(&unique[at], e)
			continue
		}
		index[key] = len(unique)
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
// fn is called exactly once per site, in whatever order the sites finish, and
// with an empty slice for a site that had nothing. Counting the calls is
// therefore counting the sites read, which is what a caller reporting progress
// needs — most sites find nothing, so calling fn only when something turned up
// would make a progress bar that barely moves.
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
		fn(fresh)
	}
}

// uniqueBySite keeps the first museum for each distinct website, preserving
// order.
// UniqueBySite is uniqueBySite for callers that need to know how many sites a
// list of museums really amounts to before handing it over — a caller reporting
// progress cannot count what Stream silently drops.
func UniqueBySite(museums []models.Museum) []models.Museum { return uniqueBySite(museums) }

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
	// Lowercased before "www." is trimmed, and via Hostname so a port is not
	// part of the key. Trimming first left "WWW.example.org" and
	// "www.example.org" as two different sites, and "example.org:80" as a
	// third, so the same museum website was scraped several times over.
	return strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
}
