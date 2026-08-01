package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
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

	// scrapeWorkers is how many areas are read at once, and scrapeConcurrency
	// how many sites within each. Their product is the number of fetches that
	// can be in flight, which is not what politeness is measured in.
	//
	// What protects a museum's server is the fetcher's own rule of one request
	// per host per second. That gate is held under a mutex on the Fetcher this
	// queue shares between every area it reads, so a site's load is the same
	// whether one area is being read or five — the requests queue up behind the
	// same per-host clock either way. Serialising whole areas on top of that
	// protected nobody: two cities have no websites in common, and the second
	// visitor waited three minutes for the first visitor's city to finish.
	scrapeWorkers     = 3
	scrapeConcurrency = 10

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

// scrapeQueue reads several areas at once, bounded, sharing one scraper.
//
// One scraper between all of them is what keeps this polite: the per-host clock
// lives on it, so adding workers adds no load to any single museum's server. It
// only stops one visitor's city from being read before another visitor's city
// can start.
type scrapeQueue struct {
	store   Harvester
	scraper *exhibitions.Scraper

	requests chan scrapeRequest

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
		state:    make(map[string]scrapeState),
		done:     make(map[string]time.Time),
		progress: make(map[string]*scrapeProgress),
		stop:     make(chan struct{}),
	}
	q.wg.Add(scrapeWorkers)
	for range scrapeWorkers {
		go q.run()
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

func (q *scrapeQueue) run() {
	defer q.wg.Done()

	for {
		select {
		case <-q.stop:
			return
		case req := <-q.requests:
			q.mu.Lock()
			q.state[req.cell] = scrapeRunning
			q.progress[req.cell] = &scrapeProgress{started: time.Now()}
			q.mu.Unlock()

			q.scrape(req)

			q.mu.Lock()
			delete(q.state, req.cell)
			delete(q.progress, req.cell)
			q.done[req.cell] = time.Now()
			q.mu.Unlock()
		}
	}
}

// scrape reads one area's museum sites and stores what it finds.
func (q *scrapeQueue) scrape(req scrapeRequest) {
	// Detached from any request: the visitor who triggered this has long since
	// had their response, and the work should not die with their connection.
	ctx, cancel := context.WithTimeout(context.Background(), scrapeJobTimeout)
	defer cancel()

	museums, err := q.store.MuseumsWithWebsitesNear(ctx, req.lat, req.lon, req.radiusKm, scrapeMaxMuseums)
	if err != nil {
		log.Printf("scrape %s: %v", req.cell, err)
		return
	}
	// Deduplicated here rather than left to Stream, which does it silently:
	// several museums can share one website, and a total that counts them
	// separately is a progress bar that stops short of the end.
	museums = exhibitions.UniqueBySite(museums)
	if len(museums) == 0 {
		return
	}

	q.setProgress(req.cell, func(p *scrapeProgress) { p.Sites = len(museums) })

	start := time.Now()
	var found, stored int

	// Stored in batches as they arrive, for the same reason the command-line
	// refresh does: a scrape that only persists at the end loses everything if
	// anything goes wrong before it gets there.
	buffer := make([]exhibitions.Exhibition, 0, 128)
	flush := func() {
		if len(buffer) == 0 {
			return
		}
		n, err := q.store.SaveExhibitions(ctx, buffer)
		if err != nil {
			log.Printf("scrape %s: storing: %v", req.cell, err)
		}
		stored += int(n)
		buffer = buffer[:0]
	}

	// Stream calls this once per site, whether or not that site had anything,
	// so counting the calls is counting the sites read.
	q.scraper.Stream(ctx, museums, scrapeConcurrency, func(batch []exhibitions.Exhibition) {
		found += len(batch)
		buffer = append(buffer, batch...)
		if len(buffer) >= 128 {
			flush()
		}
		q.setProgress(req.cell, func(p *scrapeProgress) { p.Read++; p.Found = found })
	})
	flush()

	if _, err := q.store.PruneNavigationListings(ctx); err != nil {
		log.Printf("scrape %s: pruning: %v", req.cell, err)
	}
	if _, err := q.store.MergeDuplicateExhibitions(ctx); err != nil {
		log.Printf("scrape %s: merging: %v", req.cell, err)
	}

	log.Printf("scrape %s: %d sites, %d exhibitions found, %d stored, %s",
		req.cell, len(museums), found, stored, time.Since(start).Round(time.Second))
}

// setProgress edits one area's progress under the lock. Stream calls the
// collector from one goroutine at a time, but every request asking how far
// along the area is reads this from another, and other areas are being read
// alongside it.
func (q *scrapeQueue) setProgress(cell string, edit func(*scrapeProgress)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if p := q.progress[cell]; p != nil {
		edit(p)
	}
}

// close stops the worker and waits for the job in flight.
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
