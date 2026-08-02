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
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

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
	result, err := s.ForSite(ctx, museum, Known{})
	return result.Exhibitions, err
}

// Known is what a previous sweep learned about a site: which page its listings
// came from, and the cache tags that page carried.
type Known struct {
	ListingURL string
	Validators Validators
}

// Result is one site's worth of a sweep.
type Result struct {
	Exhibitions []Exhibition

	// ListingURL is the page the exhibitions were read from, and Validators
	// are its cache tags, to be offered back next time.
	ListingURL string
	Validators Validators

	// Reached is true when at least one page was successfully fetched.
	//
	// Without it a site that could not be reached at all is indistinguishable
	// from one read fine that lists nothing, because this scraper skips failed
	// pages quietly by design. A sweep needs the difference: one should back
	// off and eventually stop costing anything, the other is a normal result
	// to be rechecked on the usual schedule.
	Reached bool

	// Unchanged is set when the site answered that its listing page has not
	// moved since we last read it. Exhibitions is empty then, and that means
	// "what you hold is still current", not "there is nothing here" — a caller
	// must not mistake the two, or it will retire a museum's whole programme
	// on the strength of a 304.
	Unchanged bool
}

// ForSite reads a museum's website for a sweep, taking the short path when the
// site can tell us nothing has changed.
//
// The fast path is worth the extra entry point. A steady site costs one
// conditional request that transfers no body and needs no parsing, in place of
// the two to four requests full discovery makes — and most museums are steady
// most of the time.
func (s *Scraper) ForSite(ctx context.Context, museum models.Museum, known Known) (Result, error) {
	if strings.TrimSpace(museum.Website) == "" {
		return Result{}, ErrNoWebsite
	}

	if known.ListingURL != "" && !known.Validators.none() {
		page, err := s.fetcher.Fetch(ctx, known.ListingURL, known.Validators)
		// A failure here is not the site's answer, only this shortcut's: fall
		// through to full discovery rather than reporting the site broken.
		if err == nil && page.Unchanged {
			return Result{
				ListingURL: known.ListingURL,
				Validators: known.Validators,
				Reached:    true,
				Unchanged:  true,
			}, nil
		}
	}

	return s.readSite(ctx, museum)
}

// readSite performs full discovery: home page, listing pages, permanent pages,
// and the museum's own description as a last resort.
func (s *Scraper) readSite(ctx context.Context, museum models.Museum) (Result, error) {
	base, err := url.Parse(strings.TrimSpace(museum.Website))
	if err != nil || base.Host == "" {
		return Result{}, errors.New("museum website is not a usable URL")
	}
	if base.Scheme == "" {
		base.Scheme = "https"
	}

	now := s.now()
	scope := venueScope(base)
	home := s.readHome(ctx, base, scope)

	var (
		result  = Result{Reached: home.reached}
		found   []Exhibition
		visited = make(map[string]struct{})
	)

	for _, listingURL := range slices.Concat(home.listings, candidateListingURLs(base, scope)) {
		if !withinScope(listingURL, base, scope) {
			continue
		}
		if ctx.Err() != nil {
			result.Exhibitions = found
			return result, ctx.Err()
		}
		if len(visited) >= maxListingPages {
			break
		}
		if _, seen := visited[listingURL]; seen {
			continue
		}
		visited[listingURL] = struct{}{}

		page, entries := s.harvest(ctx, listingURL, base, museum, now, false)
		result.Reached = result.Reached || page.URL != ""
		found = append(found, entries...)
		if len(found) > 0 {
			// The programme was found; no need to try further candidates.
			// Remember which page it came from, and what it was tagged with, so
			// the next sweep can ask this page directly whether it has moved.
			result.ListingURL, result.Validators = page.URL, page.Validators
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
		if !withinScope(listingURL, base, scope) {
			continue
		}
		if ctx.Err() != nil {
			result.Exhibitions = dedupe(found)
			return result, ctx.Err()
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

		page, entries := s.harvest(ctx, listingURL, base, museum, now, true)
		result.Reached = result.Reached || page.URL != ""
		found = append(found, entries...)
	}

	if len(found) == 0 {
		if display, ok := s.permanentDisplay(ctx, base, scope, home, museum, now); ok {
			found = append(found, display)
			result.Reached = true
			// The page describing the museum is the page whose changing
			// matters for this site, so it is what the next sweep should ask
			// about.
			result.ListingURL = display.SourcePage
		}
	}

	result.Exhibitions = dedupe(found)
	return result, nil
}

// harvest reads one listing page and returns the exhibitions on it.
//
// assumePermanent says the page was reached by a link that named it a page of
// permanent displays, which is a claim its own markup often does not repeat: a
// page headed "Fasta utställningar" lists entries that say nothing about their
// own permanence, because the heading already did.
func (s *Scraper) harvest(ctx context.Context, listingURL string, base *url.URL, museum models.Museum, now time.Time, assumePermanent bool) (Page, []Exhibition) {
	page, err := s.fetcher.Fetch(ctx, listingURL, Validators{})
	if err != nil {
		return Page{}, nil
	}
	body, finalURL := page.Body, page.URL

	pageBase, err := url.Parse(finalURL)
	if err != nil {
		pageBase = base
	}
	// Whether this page holds permanent displays is a property of the page,
	// read once: its entries carry no dates and mostly do not repeat the label
	// the page already gave them.
	pagePermanent := assumePermanent || isPermanentListing(body, pageBase)

	// The section this page indexes, when it is a museum's exhibitions index
	// rather than some other page that happens to link to exhibitions.
	section := ProgrammeSection(pageBase)

	var found []Exhibition
	for _, candidate := range candidatesOn(body, pageBase, section) {
		dates := datesFor(candidate, now)
		// An entry with no readable dates cannot be placed in time, and listing
		// pages are full of links that are not exhibitions at all; requiring a
		// date is what separates the two. A permanent display is the exception,
		// and has to name itself one to be kept: otherwise the rule that keeps
		// the noise out is gone.
		// An entry the museum files as an exhibition, but gives no dates for, is
		// permanent. That is what a museum means by it: a show with a closing
		// date says so, and one that says nothing is not going anywhere.
		//
		// Requiring a date instead discarded them. It is a good rule against
		// noise — links to /visit and /tickets carry no dates either — but the
		// noise it guards against is not filed under a museum's exhibitions
		// section, and an entry that is has already been vouched for. Göteborgs
		// stadsmuseum lists ten exhibitions with no dates on the index at all,
		// Kalmar konstmuseum three, and every one was thrown away.
		//
		// Calling them permanent rather than merely undated is the honest
		// reading and also the useful one: it puts them behind everything with
		// a closing date, where a visitor deciding what to catch first wants
		// them, and says plainly on the page why they carry no dates.
		permanent := dates.Permanent
		if dates.IsZero() && !permanent {
			if !candidate.Strong && !EntryUnder(section, candidate.URL) &&
				!candidateIsPermanent(candidate, pagePermanent) {
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
	return page, found
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
func (s *Scraper) permanentDisplay(ctx context.Context, base *url.URL, scope string, home homePage, museum models.Museum, now time.Time) (Exhibition, bool) {
	tried := make(map[string]struct{}, maxInfoPages)

	// A page the site itself called its permanent exhibition comes first, and
	// is a better answer than the visitor information: the Jewish Museum
	// Berlin's "/dauerausstellung" describes what is on show, while its
	// "/rund-um-den-besuch" describes the cloakroom. It reaches here rather
	// than being listed above only when it lists no entries — which is the
	// usual case, because a museum with one permanent exhibition writes a page
	// about it rather than a page of links to it.
	for _, infoURL := range slices.Concat(home.permanent, home.info, infoURLs(base)) {
		// The same confinement as the listings: on a shared site the "about"
		// page describes the institution, and standing in for a single venue's
		// permanent display it would be wrong about all of them.
		if !withinScope(infoURL, base, scope) {
			continue
		}
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
	// reached is whether the front page was read at all.
	reached bool
	// listings are the links that look like a programme, best first.
	listings []string
	// permanent are the links the page labelled as leading to displays that
	// are always on.
	permanent []string
	// info are the links that look like the museum describing itself and what
	// it holds, best first.
	info []string
}

// venueScope returns the path a museum occupies when its website points at one
// venue inside a larger site, and "" when the website is the site itself.
//
// Museums share websites far more often than the scraper assumed: 20,724
// records in the catalogue sit on a host another museum also claims, across
// 6,761 such hosts. Göteborgs stadsmuseum publishes one programme at
// /utstallningar/ and gives each of its venues a page — Hem i Haga at
// /besok-oss/hem-i-haga/, Lilla Änggården beside it. Reading from the site root
// gave Hem i Haga the whole museum's ten exhibitions, none of which are
// specifically there. heritageireland.ie does the same for thirteen places,
// jerseyheritage.org for seven, glasgowlife.org.uk for nine.
//
// A path ending in something that looks like a file — /index.html,
// /default.aspx — is the front page written out longhand, not a venue.
func venueScope(base *url.URL) string {
	trimmed := strings.Trim(base.Path, "/")
	if trimmed == "" || strings.Contains(path.Base(trimmed), ".") {
		return ""
	}
	return "/" + trimmed + "/"
}

// withinScope reports whether a candidate page may be read on this museum's
// behalf: the same host, and at or below its own page.
//
// There is deliberately no fallback to the site root when a scoped search finds
// nothing. The programme at the root belongs to the institution, and attributing
// it to one venue is the fault this exists to fix — for Hem i Haga the honest
// answer is that nothing is listed for it, which is what its own page says.
func withinScope(rawURL string, base *url.URL, scope string) bool {
	if scope == "" {
		return true
	}
	candidate, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if candidate.Host != "" && !strings.EqualFold(candidate.Host, base.Host) {
		return false
	}
	within := candidate.Path
	if !strings.HasSuffix(within, "/") {
		within += "/"
	}
	return strings.HasPrefix(within, scope)
}

// readHome fetches the page a site should be read from and sorts its links: the
// museum's own page when it has one, and the front page otherwise. A page that
// cannot be read is not an error: the conventional paths are tried regardless.
func (s *Scraper) readHome(ctx context.Context, base *url.URL, scope string) homePage {
	home := *base
	home.Path = "/"
	if scope != "" {
		home.Path = scope
	}
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
		reached:   true,
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
	// Keyed on the URL as well, because one page can name the same entry twice.
	// Göteborgs naturhistoriska museum links each of its halls from both its
	// exhibitions index and its permanent-displays page, once as a heading and
	// once as a photograph with no text at all, so the same URL arrived as
	// "Däggdjurssalen" and as "Daggdjurssalen" read off the slug. The URL is the
	// exhibition's identity — it is the primary key in the database — and the
	// better-written of the two titles is the one to keep.
	index := make(map[string]int, len(exhibitions))
	unique := exhibitions[:0:0]

	for _, e := range exhibitions {
		title := strings.ToLower(strings.TrimSpace(e.Title))
		at, dup := index[title]
		if !dup {
			at, dup = index[e.URL]
		}
		if dup {
			widen(&unique[at], e)
			if betterTitle(e.Title, unique[at].Title) {
				unique[at].Title = e.Title
			}
			index[strings.ToLower(strings.TrimSpace(unique[at].Title))] = at
			continue
		}
		index[title] = len(unique)
		if e.URL != "" {
			index[e.URL] = len(unique)
		}
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

// SiteKey reduces a website URL to the host it serves from, so that
// "https://www.louvre.fr/en" and "https://www.louvre.fr/" count as one site.
//
// Exported because the site is the unit a sweep schedules and the unit stored
// listings are retired by, so the database and the scraper have to agree on
// what one is. Two implementations of this would drift, and the symptom would
// be a site scheduled under one key and recorded under another — swept every
// run, forever, with nothing to show why.
func SiteKey(website string) string { return siteKey(website) }

// siteKey reduces a website URL to the host it serves from.
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

// betterTitle reports whether a is the more informative of two titles for the
// same exhibition.
//
// The comparison that matters is against a title read off a URL slug, which is
// the fallback when a card carries no text. A slug has had its accents stripped
// and its capitalisation invented — "Daggdjurssalen" beside the museum's own
// "Däggdjurssalen" — so a title carrying letters outside ASCII is the museum's
// own wording, and wins. Otherwise the longer one carries more.
func betterTitle(a, b string) bool {
	if nonASCII(a) != nonASCII(b) {
		return nonASCII(a)
	}
	return len(a) > len(b)
}

func nonASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return true
		}
	}
	return false
}
