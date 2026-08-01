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
// of it. One area at a time, a cap on how many sites any one area reads, and a
// long cooldown before the same place is read again.
const (
	scrapeMaxMuseums   = 120
	scrapeConcurrency  = 6
	scrapeQueueDepth   = 32
	scrapeCooldown     = 24 * time.Hour
	scrapeJobTimeout   = 15 * time.Minute
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

// scrapeQueue runs at most one area scrape at a time.
//
// A single worker on purpose. The point of the queue is not throughput; it is
// that a page which starts a scrape whenever someone looks at a new city must
// never turn into many simultaneous crawlers pointed at other people's servers.
type scrapeQueue struct {
	store   Harvester
	scraper *exhibitions.Scraper

	requests chan scrapeRequest

	mu      sync.Mutex
	state   map[string]scrapeState
	done    map[string]time.Time
	running string

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
		stop:     make(chan struct{}),
	}
	q.wg.Add(1)
	go q.run()
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

// status reports what is known about an area without asking for anything.
func (q *scrapeQueue) status(lat, lon float64) scrapeState {
	cell := cellFor(lat, lon)

	q.mu.Lock()
	defer q.mu.Unlock()
	if at, ok := q.done[cell]; ok && time.Since(at) < scrapeCooldown {
		return scrapeCooling
	}
	if state, ok := q.state[cell]; ok {
		return state
	}
	return scrapeIdle
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
			q.running = req.cell
			q.mu.Unlock()

			q.scrape(req)

			q.mu.Lock()
			delete(q.state, req.cell)
			q.done[req.cell] = time.Now()
			q.running = ""
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
	if len(museums) == 0 {
		return
	}

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

	q.scraper.Stream(ctx, museums, scrapeConcurrency, func(batch []exhibitions.Exhibition) {
		found += len(batch)
		buffer = append(buffer, batch...)
		if len(buffer) >= 128 {
			flush()
		}
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

	var state scrapeState
	if r.Method == http.MethodPost {
		state, err = s.scrapes.enqueue(q.lat, q.lon, q.radiusKm)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	} else {
		state = s.scrapes.status(q.lat, q.lon)
	}

	status := http.StatusOK
	if state == scrapeQueued || state == scrapeRunning {
		status = http.StatusAccepted
	}

	writeJSON(w, status, map[string]any{
		"state": string(state),
		"area":  echo(q),
	})
}
