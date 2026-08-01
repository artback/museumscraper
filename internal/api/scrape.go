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

	"museum/internal/sweep"
	"museum/pkg/exhibitions"
)

// Harvester is the part of the store an on-demand scrape needs. It is separate
// from Catalogue because scraping writes, and most of the API only reads.
//
// It embeds the sweep's own store because an on-demand scrape has to leave
// exactly the trace a scheduled one does. When it did not, the two worked
// against each other: the sweep re-read sites this had read minutes earlier,
// none of this work fed the adaptive interval, vanished listings were retired
// down only one of the two paths, and an area scraped here still reported "no
// exhibitions have been collected here yet", because the coverage report reads
// the attempt record and this was not writing one.
type Harvester interface {
	sweep.Store

	DiscoverSites(ctx context.Context) (int64, error)
	TargetsNear(ctx context.Context, lat, lon, radiusKm float64, limit int) ([]sweep.Target, error)
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
	store  Harvester
	runner *sweep.Runner

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
		runner:   sweep.NewRunner(store, exhibitions.NewScraper()),
		requests: make(chan scrapeRequest, scrapeQueueDepth),
		state:    make(map[string]scrapeState),
		done:     make(map[string]time.Time),
		stop:     make(chan struct{}),
	}
	q.wg.Add(1)
	go q.run()
	return q
}

// cellFor rounds an area to a grid so two people looking at the same city ask
// for the same thing, and the second is told it is already happening.
func cellFor(lat, lon float64) string {
	round := func(v float64) float64 { return math.Round(v/scrapeCellDegrees) * scrapeCellDegrees }
	return fmt.Sprintf("%.2f,%.2f", round(lat), round(lon))
}

// enqueue asks for an area to be scraped, reporting what will happen.
func (q *scrapeQueue) enqueue(lat, lon, radiusKm float64) (scrapeState, error) {
	if radiusKm <= 0 || radiusKm > scrapeMinZoomRadiu {
		return scrapeIdle, fmt.Errorf("an area of %.0f km is too wide to scrape; zoom in first", radiusKm)
	}
	cell := cellFor(lat, lon)

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

	// A site is a row, and a museum added by a crawl since the last sweep does
	// not have one yet. Without this, an area of newly catalogued museums —
	// exactly the case someone panning a map is most likely to hit — would
	// return nothing to read.
	if _, err := q.store.DiscoverSites(ctx); err != nil {
		log.Printf("scrape %s: discovering sites: %v", req.cell, err)
	}

	targets, err := q.store.TargetsNear(ctx, req.lat, req.lon, req.radiusKm, scrapeMaxMuseums)
	if err != nil {
		log.Printf("scrape %s: %v", req.cell, err)
		return
	}
	if len(targets) == 0 {
		return
	}

	start := time.Now()
	var found int
	var retired int64

	// One site at a time, through the same reader the scheduled sweep uses, so
	// this run stores, retires and reschedules identically. Sites already
	// parked for repeated failure are excluded by TargetsNear: someone looking
	// at a map is not a reason to retry a host that has refused six times.
	for _, target := range targets {
		if ctx.Err() != nil {
			break
		}
		report := q.runner.Read(ctx, target)
		found += report.Found
		retired += report.Retired
	}

	if _, err := q.store.PruneNavigationListings(ctx); err != nil {
		log.Printf("scrape %s: pruning: %v", req.cell, err)
	}
	if _, err := q.store.MergeDuplicateExhibitions(ctx); err != nil {
		log.Printf("scrape %s: merging: %v", req.cell, err)
	}

	log.Printf("scrape %s: %d sites, %d exhibitions found, %d retired, %s",
		req.cell, len(targets), found, retired, time.Since(start).Round(time.Second))
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
