package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"

	"museum/internal/models"
	"museum/pkg/exhibitions"
)

// Harvester is the part of the store an on-demand scrape needs. It is separate
// from Catalogue because scraping writes, and most of the API only reads.
type Harvester interface {
	MuseumsWithWebsitesNear(ctx context.Context, lat, lon, radiusKm float64, limit int) ([]models.Museum, error)
	SaveExhibitions(ctx context.Context, found []exhibitions.Exhibition) (int64, error)
	MergeDuplicateExhibitions(ctx context.Context) (int64, error)
	PruneNavigationListings(ctx context.Context) (int64, error)
	ForgetUnlisted(ctx context.Context, host string, keep []string) (int64, error)
}

// Limits on scraping the API starts on a visitor's behalf.
//
// This turns someone panning a map into requests against museums' own
// websites, so the bounds are the important part of the feature, not a detail
// of it: a cap on how many areas are read at once, a cap on how many sites any
// one area reads, and a long cooldown before the same place is read again.
const (
	scrapeMaxMuseums = 120
	scrapeQueueDepth = 32
	scrapeCooldown   = 24 * time.Hour
	scrapeJobTimeout = 15 * time.Minute

	// scrapeSiteWorkers is how many museum websites are read at once, across
	// every area together rather than within one.
	//
	// What protects a museum's server is the fetcher's own rule of one request
	// per host per second. That gate is held under a mutex on the Fetcher this
	// queue shares between every area it reads, so a site's load is the same
	// whether the workers reading it came from one area or six — the requests
	// queue up behind the same per-host clock either way. This number is
	// therefore about our own resources, not about politeness.
	scrapeSiteWorkers = 24

	// scrapeAdmitters is how many areas can be looked up in the database at
	// once. Small: it is one indexed query per area, and it only has to keep
	// ahead of the site workers.
	scrapeAdmitters = 3

	// scrapeStoreBatch is how many exhibitions are held before writing. A
	// scrape that only persists at the end loses everything if anything goes
	// wrong before it gets there.
	scrapeStoreBatch = 128

	scrapeCellDegrees  = 0.25 // areas are rounded to this grid before dedup
	scrapeMinZoomRadiu = 60.0 // km; wider than a city is not a scrape target
)

// scrapeMinRadiusKm is the smallest area a scrape may read: the circle that
// covers one whole dedup cell.
//
// A scrape marks its entire cell scraped for a day, but reads only the circle
// it was asked for. Those have to agree. They did not: someone zoomed in on a
// single museum asked for a 3 km circle, one website was read, and the other
// forty-odd museums sharing that cell were locked out for the next 24 hours
// without ever having been looked at. Reading at least the cell means the
// cooldown never covers ground the scrape did not.
//
// Half a cell's diagonal, in kilometres. Derived from the cell rather than
// picked, so the two cannot drift apart. A degree of latitude is taken as
// 111.32 km, and latitude is the worst case — a degree of longitude only
// shrinks as you leave the equator.
const scrapeMinRadiusKm = scrapeCellDegrees * math.Sqrt2 / 2 * 111.32

// scrapeState is what a caller is told about an area.
type scrapeState string

const (
	scrapeIdle    scrapeState = "idle"
	scrapeQueued  scrapeState = "queued"
	scrapeRunning scrapeState = "running"
	scrapeDone    scrapeState = "done"
	scrapeCooling scrapeState = "recently-scraped"
)

// scrapeRequest is one area to read.
type scrapeRequest struct {
	lat, lon, radiusKm float64
	cell               string
}

// scrapeProgress is how far along an area is.
//
// Reading a city's museum websites takes minutes, and a page that says only
// "not scraped yet" for all of them is indistinguishable from one where nothing
// is happening. Sites and Found are what a visitor can actually judge: how much
// is left, and whether it is turning anything up.
type scrapeProgress struct {
	Sites int `json:"sites"`
	Read  int `json:"sites_read"`
	Found int `json:"exhibitions_found"`
	// Elapsed is seconds spent reading, filled in when asked rather than
	// stored. Reporting the start instant instead meant an area that had not
	// started yet answered with the zero time, which reads as a date in the
	// year 1 — a number no caller can do anything sensible with.
	Elapsed int `json:"elapsed_seconds"`
	// Waiting is how many other areas are queued ahead of this one. Only
	// meaningful while queued, and zero for the area being read now.
	Waiting int `json:"waiting_behind"`

	started time.Time
}

// scrapeArea is one area being read: the sites still to visit, and the results
// coming back from them.
//
// sent is touched only by the dispatcher, so it needs no lock.
type scrapeArea struct {
	cell    string
	ctx     context.Context
	cancel  context.CancelFunc
	museums []models.Museum
	sent    int
	// results is buffered to the full site count so a worker can always deposit
	// its result and move on, even if the area's collector has stopped.
	results chan []exhibitions.Exhibition
}

// scrapeJob is one museum website for one worker to read.
type scrapeJob struct {
	area   *scrapeArea
	museum models.Museum
}

// scrapeQueue reads museum websites from every waiting area at once, taking one
// site from each area in turn.
//
// The unit of scheduling is the site rather than the area, because the areas
// are wildly different sizes. Giving a whole worker to an area meant a hamlet
// with two museums sat behind a capital with a hundred and twenty and waited
// for all of them, even though its own share of the work was seconds. Taking
// turns site by site, the small area is finished within a couple of rounds
// whatever else is running.
//
// One scraper between all of them is what keeps this polite: the per-host clock
// lives on it, so more workers add no load to any single museum's server.
type scrapeQueue struct {
	store   Harvester
	scraper *exhibitions.Scraper

	requests chan scrapeRequest
	ready    chan *scrapeArea
	jobs     chan scrapeJob

	mu    sync.Mutex
	state map[string]scrapeState
	done  map[string]time.Time
	// progress holds an entry per area being read now. Keyed by cell rather
	// than kept as one struct, because several are in flight and each visitor
	// is asking about their own.
	progress map[string]*scrapeProgress

	stop chan struct{}
	wg   sync.WaitGroup
}

func newScrapeQueue(store Harvester) *scrapeQueue {
	q := &scrapeQueue{
		store:    store,
		scraper:  exhibitions.NewScraper(),
		requests: make(chan scrapeRequest, scrapeQueueDepth),
		ready:    make(chan *scrapeArea),
		jobs:     make(chan scrapeJob),
		state:    make(map[string]scrapeState),
		done:     make(map[string]time.Time),
		progress: make(map[string]*scrapeProgress),
		stop:     make(chan struct{}),
	}

	q.wg.Add(1)
	go q.dispatch()
	q.wg.Add(scrapeAdmitters)
	for range scrapeAdmitters {
		go q.admit()
	}
	q.wg.Add(scrapeSiteWorkers)
	for range scrapeSiteWorkers {
		go q.work()
	}
	return q
}

// cellCentre rounds a point to the centre of its grid cell.
func cellCentre(lat, lon float64) (float64, float64) {
	round := func(v float64) float64 { return math.Round(v/scrapeCellDegrees) * scrapeCellDegrees }
	return round(lat), round(lon)
}

// cellFor names the grid cell a point falls in, so two people looking at the
// same city ask for the same thing and the second is told it is already
// happening.
func cellFor(lat, lon float64) string {
	lat, lon = cellCentre(lat, lon)
	return fmt.Sprintf("%.2f,%.2f", lat, lon)
}

// enqueue asks for an area to be scraped, reporting what will happen.
func (q *scrapeQueue) enqueue(lat, lon, radiusKm float64) (scrapeState, error) {
	if radiusKm <= 0 || radiusKm > scrapeMinZoomRadiu {
		return scrapeIdle, fmt.Errorf("an area of %.0f km is too wide to scrape; zoom in first", radiusKm)
	}
	// Read the cell this is about to claim, not the caller's own circle. The
	// centre moves to the cell's centre and the radius is widened to reach its
	// corners, so the ground read always contains the ground the cooldown then
	// covers. The caller is still told about the area it asked for, which stays
	// true: this only ever reads more.
	cell := cellFor(lat, lon)
	lat, lon = cellCentre(lat, lon)
	radiusKm = math.Max(radiusKm, scrapeMinRadiusKm)

	q.mu.Lock()
	if at, ok := q.done[cell]; ok && time.Since(at) < scrapeCooldown {
		q.mu.Unlock()
		return scrapeCooling, nil
	}
	switch q.state[cell] {
	case scrapeQueued:
		q.mu.Unlock()
		return scrapeQueued, nil
	case scrapeRunning:
		q.mu.Unlock()
		return scrapeRunning, nil
	}
	q.state[cell] = scrapeQueued
	q.mu.Unlock()

	select {
	case q.requests <- scrapeRequest{lat: lat, lon: lon, radiusKm: radiusKm, cell: cell}:
		return scrapeQueued, nil
	default:
		// A full queue is refused rather than grown: the backlog is a promise
		// to hit other people's servers, and an unbounded promise is not one
		// worth making.
		q.mu.Lock()
		delete(q.state, cell)
		q.mu.Unlock()
		return scrapeIdle, errors.New("too many areas are already waiting to be scraped; try again shortly")
	}
}

// status reports what is known about an area without asking for anything, and
// how far along it is when it is being read now.
func (q *scrapeQueue) status(lat, lon float64) (scrapeState, scrapeProgress) {
	cell := cellFor(lat, lon)

	q.mu.Lock()
	defer q.mu.Unlock()
	if at, ok := q.done[cell]; ok && time.Since(at) < scrapeCooldown {
		return scrapeCooling, scrapeProgress{}
	}
	state, ok := q.state[cell]
	switch {
	case !ok:
		return scrapeIdle, scrapeProgress{}
	case q.progress[cell] != nil:
		progress := *q.progress[cell]
		progress.Elapsed = int(time.Since(progress.started).Seconds())
		return state, progress
	case state == scrapeQueued:
		// Everything still in the channel is waiting alongside this one. With
		// several workers taking from it they do not all go first, so this is
		// how many are waiting rather than how many are ahead.
		return state, scrapeProgress{Waiting: len(q.requests)}
	}
	return state, scrapeProgress{}
}

// admit turns a requested area into the list of sites it means, and hands that
// to the dispatcher.
//
// Separate from the dispatcher because it talks to the database: a slow query
// here must not stop sites already in flight from being handed out.
func (q *scrapeQueue) admit() {
	defer q.wg.Done()

	for {
		select {
		case <-q.stop:
			return
		case req := <-q.requests:
			q.prepare(req)
		}
	}
}

// prepare looks up one area's sites and starts collecting for it.
func (q *scrapeQueue) prepare(req scrapeRequest) {
	// Detached from any request: the visitor who triggered this has long since
	// had their response, and the work should not die with their connection.
	ctx, cancel := context.WithTimeout(context.Background(), scrapeJobTimeout)

	museums, err := q.store.MuseumsWithWebsitesNear(ctx, req.lat, req.lon, req.radiusKm, scrapeMaxMuseums)
	if err != nil {
		// No cooldown on a failure. The cooldown means "this has been read
		// recently", and a database that was briefly unavailable has not read
		// anything — leaving it set would block the area for a day over a
		// hiccup that lasted a second.
		log.Printf("scrape %s: %v", req.cell, err)
		q.release(req.cell, false)
		cancel()
		return
	}
	// Deduplicated before the count is published: several museums can share one
	// website, and a total that counts them separately is a progress bar that
	// stops short of the end.
	museums = exhibitions.UniqueBySite(museums)
	if len(museums) == 0 {
		q.release(req.cell, true)
		cancel()
		return
	}

	q.mu.Lock()
	q.state[req.cell] = scrapeRunning
	q.progress[req.cell] = &scrapeProgress{Sites: len(museums), started: time.Now()}
	q.mu.Unlock()

	area := &scrapeArea{
		cell:    req.cell,
		ctx:     ctx,
		cancel:  cancel,
		museums: museums,
		results: make(chan []exhibitions.Exhibition, len(museums)),
	}

	q.wg.Add(1)
	go q.collect(area)

	select {
	case q.ready <- area:
	case <-q.stop:
		cancel()
	}
}

// dispatch hands out one site at a time, taking areas in turn.
//
// Round-robin rather than first-come: an area's place in the queue should not
// decide how long a different, smaller area waits.
func (q *scrapeQueue) dispatch() {
	defer q.wg.Done()
	// Closing this is what tells the workers to stop, so it has to happen on
	// every exit from here.
	defer close(q.jobs)

	var areas []*scrapeArea
	turn := 0

	for {
		// A nil channel blocks forever in a select, which is exactly what is
		// wanted while there is nothing to hand out.
		var offer chan scrapeJob
		var job scrapeJob
		if len(areas) > 0 {
			area := areas[turn]
			job = scrapeJob{area: area, museum: area.museums[area.sent]}
			offer = q.jobs
		}

		select {
		case <-q.stop:
			return
		case area := <-q.ready:
			areas = append(areas, area)
		case offer <- job:
			area := areas[turn]
			area.sent++
			if area.sent == len(area.museums) {
				// Fully handed out. Its collector goes on working; there is
				// simply nothing left here to give anyone.
				areas = append(areas[:turn], areas[turn+1:]...)
			} else {
				turn++
			}
			if turn >= len(areas) {
				turn = 0
			}
		}
	}
}

// work reads one museum website at a time, for whichever area it was given.
func (q *scrapeQueue) work() {
	defer q.wg.Done()

	for job := range q.jobs {
		found, err := q.scraper.ForMuseum(job.area.ctx, job.museum)
		if err != nil && !errors.Is(err, exhibitions.ErrNoWebsite) {
			log.Printf("scrape %s: %s: %v", job.area.cell, job.museum.Name, err)
		}
		// Never blocks: the channel holds one slot per site in the area.
		job.area.results <- found
	}
}

// collect gathers one area's results, stores them as they arrive, and finishes
// the area off.
//
// One of these per area, so the batching and the deduplication belong to that
// area alone even though the workers filling it are shared with every other.
func (q *scrapeQueue) collect(area *scrapeArea) {
	defer q.wg.Done()
	defer area.cancel()

	start := time.Now()
	var found, stored, forgotten int64

	// Deduplicated by URL within the area, because sites cross-list: one
	// venue's programme page carries entries another venue's site also shows.
	seen := make(map[string]struct{})
	buffer := make([]exhibitions.Exhibition, 0, scrapeStoreBatch)

	flush := func() {
		if len(buffer) == 0 {
			return
		}
		n, err := q.store.SaveExhibitions(area.ctx, buffer)
		if err != nil {
			log.Printf("scrape %s: storing: %v", area.cell, err)
		}
		stored += n
		buffer = buffer[:0]
	}

	for range area.museums {
		select {
		case <-q.stop:
			// Keep what has already been read rather than discarding it.
			flush()
			return
		case batch := <-area.results:
			// What this site listed is now the whole truth about it, so
			// anything else we hold for that host has stopped being true.
			// Done before the batch is filtered, because a URL dropped here
			// for being a duplicate of another site's entry is still one this
			// site offered.
			forgotten += q.forgetUnlisted(area, batch)

			for _, e := range batch {
				if e.URL == "" {
					continue
				}
				if _, dup := seen[e.URL]; dup {
					continue
				}
				seen[e.URL] = struct{}{}
				buffer = append(buffer, e)
				found++
			}
			if len(buffer) >= scrapeStoreBatch {
				flush()
			}
			q.setProgress(area.cell, func(p *scrapeProgress) { p.Read++; p.Found = int(found) })
		}
	}
	flush()

	if _, err := q.store.PruneNavigationListings(area.ctx); err != nil {
		log.Printf("scrape %s: pruning: %v", area.cell, err)
	}
	if _, err := q.store.MergeDuplicateExhibitions(area.ctx); err != nil {
		log.Printf("scrape %s: merging: %v", area.cell, err)
	}

	log.Printf("scrape %s: %d sites, %d exhibitions found, %d stored, %d no longer listed, %s",
		area.cell, len(area.museums), found, stored, forgotten, time.Since(start).Round(time.Second))

	q.release(area.cell, true)
}

// forgetUnlisted drops whatever the site behind this batch no longer shows.
func (q *scrapeQueue) forgetUnlisted(area *scrapeArea, batch []exhibitions.Exhibition) int64 {
	if len(batch) == 0 {
		return 0
	}
	host := hostOf(batch[0].SourcePage)
	keep := make([]string, 0, len(batch))
	for _, e := range batch {
		keep = append(keep, e.URL)
	}

	gone, err := q.store.ForgetUnlisted(area.ctx, host, keep)
	if err != nil {
		log.Printf("scrape %s: forgetting unlisted: %v", area.cell, err)
		return 0
	}
	return gone
}

// hostOf is the host a page was served from, or "" if it cannot be read.
func hostOf(page string) string {
	parsed, err := url.Parse(page)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// release lets go of an area, either recording that it has just been read or
// leaving it free to be asked for again.
func (q *scrapeQueue) release(cell string, scraped bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.state, cell)
	delete(q.progress, cell)
	if scraped {
		q.done[cell] = time.Now()
	}
}

// setProgress edits one area's progress under the lock. Its collector is the
// only writer, but every request asking how far along the area is reads this
// from another goroutine.
func (q *scrapeQueue) setProgress(cell string, edit func(*scrapeProgress)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if p := q.progress[cell]; p != nil {
		edit(p)
	}
}

// close stops every worker and waits for the work in flight.
func (q *scrapeQueue) close() {
	close(q.stop)
	q.wg.Wait()
}

// handleScrape starts or reports on a scrape of an area.
func (s *Server) handleScrape(w http.ResponseWriter, r *http.Request) {
	if s.scrapes == nil {
		writeError(w, http.StatusNotImplemented, errors.New("on-demand scraping is not enabled"))
		return
	}

	q, err := s.parseQuery(r)
	if err != nil {
		writeQueryError(w, r, err)
		return
	}

	var (
		state    scrapeState
		progress scrapeProgress
	)
	if r.Method == http.MethodPost {
		state, err = s.scrapes.enqueue(q.lat, q.lon, q.radiusKm)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		// The state a POST returns is what enqueue decided; the numbers behind
		// it come from the same place a GET would read them, so a caller that
		// starts a scrape and one that polls an existing one see the same shape.
		_, progress = s.scrapes.status(q.lat, q.lon)
	} else {
		state, progress = s.scrapes.status(q.lat, q.lon)
	}

	status := http.StatusOK
	if state == scrapeQueued || state == scrapeRunning {
		status = http.StatusAccepted
	}

	body := map[string]any{
		"state": string(state),
		"area":  echo(q),
	}
	if state == scrapeQueued || state == scrapeRunning {
		body["progress"] = progress
	}
	writeJSON(w, status, body)
}
