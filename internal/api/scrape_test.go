package api

import (
	"context"
	"math"
	"testing"
	"time"

	"museum/internal/models"
	"museum/pkg/exhibitions"
)

// fakeHarvester records the circle a scrape was told to read, so a test can
// check it against the area the queue then marks as done.
type fakeHarvester struct {
	asked chan [3]float64
}

func (h *fakeHarvester) MuseumsWithWebsitesNear(_ context.Context, lat, lon, radiusKm float64, _ int) ([]models.Museum, error) {
	h.asked <- [3]float64{lat, lon, radiusKm}
	return nil, nil
}

func (h *fakeHarvester) SaveExhibitions(context.Context, []exhibitions.Exhibition) (int64, error) {
	return 0, nil
}
func (h *fakeHarvester) MergeDuplicateExhibitions(context.Context) (int64, error) { return 0, nil }
func (h *fakeHarvester) PruneNavigationListings(context.Context) (int64, error)   { return 0, nil }

// A scrape must read every part of the cell it then blocks for a day.
//
// It did not. The circle came from the caller — a visitor zoomed in on one
// museum asked for 3 km — while the cooldown was applied to the whole 0.25°
// cell around it. One website was read and the rest of that cell was refused
// for the next 24 hours without ever having been looked at, which is why the
// scraper looked as though it only ever covered the museum in the middle of
// the screen.
func TestScrapeReadsTheWholeCellItClaims(t *testing.T) {
	harvester := &fakeHarvester{asked: make(chan [3]float64, 1)}
	queue := newScrapeQueue(harvester)
	defer queue.close()

	// A tight view of one museum, deliberately off-centre in its cell so that
	// widening alone would not be enough to reach the far corner.
	const lat, lon = 57.7089, 11.9746
	if _, err := queue.enqueue(lat, lon, 3); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var asked [3]float64
	select {
	case asked = <-harvester.asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the scrape never asked for any museums")
	}

	// Every corner of the claimed cell has to fall inside the circle read.
	centreLat, centreLon := cellCentre(lat, lon)
	half := scrapeCellDegrees / 2
	for _, corner := range [][2]float64{
		{centreLat - half, centreLon - half},
		{centreLat - half, centreLon + half},
		{centreLat + half, centreLon - half},
		{centreLat + half, centreLon + half},
	} {
		km := haversineKm(asked[0], asked[1], corner[0], corner[1])
		if km > asked[2] {
			t.Errorf("corner %.4f,%.4f is %.1f km from the scraped centre but only %.1f km was read;"+
				" the cooldown would cover ground nothing looked at", corner[0], corner[1], km, asked[2])
		}
	}
}

// A caller asking for more than the cell keeps its own reach.
func TestScrapeKeepsAWiderRequest(t *testing.T) {
	harvester := &fakeHarvester{asked: make(chan [3]float64, 1)}
	queue := newScrapeQueue(harvester)
	defer queue.close()

	if _, err := queue.enqueue(57.7089, 11.9746, 45); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case asked := <-harvester.asked:
		if asked[2] != 45 {
			t.Errorf("read %.1f km, want the 45 km asked for", asked[2])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the scrape never asked for any museums")
	}
}

// blockingHarvester holds every scrape inside the store call until released, so
// a test can see how many areas are being read at the same moment.
type blockingHarvester struct {
	entered chan struct{}
	release chan struct{}
}

func (h *blockingHarvester) MuseumsWithWebsitesNear(context.Context, float64, float64, float64, int) ([]models.Museum, error) {
	h.entered <- struct{}{}
	<-h.release
	return nil, nil
}

func (h *blockingHarvester) SaveExhibitions(context.Context, []exhibitions.Exhibition) (int64, error) {
	return 0, nil
}
func (h *blockingHarvester) MergeDuplicateExhibitions(context.Context) (int64, error) { return 0, nil }
func (h *blockingHarvester) PruneNavigationListings(context.Context) (int64, error)   { return 0, nil }

// Different places must not wait for each other.
//
// One worker read one area at a time, so a visitor opening Copenhagen queued
// behind a visitor who had opened Stockholm and waited out its full three
// minutes before their own city even started. Nothing was protected by that:
// two cities share no websites, and what keeps a museum's server safe is the
// fetcher's per-host clock, which every area shares regardless.
func TestScrapeReadsSeveralAreasAtOnce(t *testing.T) {
	// Three areas offered and two required to be running at once. Written as
	// numbers rather than as scrapeWorkers, so that turning the workers back
	// down to one fails here instead of quietly making the test trivial.
	const offered, wantAtOnce = 3, 2

	harvester := &blockingHarvester{
		entered: make(chan struct{}, offered),
		release: make(chan struct{}),
	}
	queue := newScrapeQueue(harvester)
	defer func() { close(harvester.release); queue.close() }()

	// Far enough apart to be different cells, and so different areas.
	for i := range offered {
		if _, err := queue.enqueue(50+float64(i), 10, 5); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// Every one of these is held inside the store call, so each arrival is an
	// area being read at this moment rather than one that has been and gone.
	for i := range wantAtOnce {
		select {
		case <-harvester.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("%d of %d areas were being read at once; the rest wait for a city"+
				" they share no websites with", i, wantAtOnce)
		}
	}
}

// A small area must not wait for a large one to finish.
//
// Handing a whole area to a worker meant the unit of scheduling was the area,
// and the areas are wildly different sizes: a hamlet with two museums sat
// behind a capital with a hundred and twenty and waited out all of them, though
// its own share of the work was seconds. The dispatcher takes one site from
// each waiting area in turn, so the small one is done in a couple of rounds.
func TestScrapeTakesSitesFromEveryAreaInTurn(t *testing.T) {
	area := func(cell string, sites int) *scrapeArea {
		museums := make([]models.Museum, sites)
		for i := range museums {
			museums[i] = models.Museum{Name: cell, Website: "https://example.invalid/"}
		}
		return &scrapeArea{cell: cell, museums: museums}
	}

	// Only the dispatcher: no workers, so this test reads the jobs itself and
	// sees the order they are handed out in.
	q := &scrapeQueue{
		ready: make(chan *scrapeArea),
		jobs:  make(chan scrapeJob),
		stop:  make(chan struct{}),
	}
	q.wg.Add(1)
	go q.dispatch()
	defer q.close()

	big, small := area("big", 120), area("small", 2)
	q.ready <- big
	q.ready <- small

	// Within the first handful of sites handed out, the small area has to be
	// finished. Before, its first site came after the big area's hundred and
	// twentieth.
	const patience = 6
	var smallSeen int
	for i := range patience {
		select {
		case job := <-q.jobs:
			if job.area == small {
				smallSeen++
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the dispatcher stopped handing out work after %d sites", i)
		}
		if smallSeen == len(small.museums) {
			return
		}
	}
	t.Errorf("after %d sites the small area had %d of its %d read; it is waiting for the large one",
		patience, smallSeen, len(small.museums))
}

// haversineKm is the great-circle distance, for checking coverage in the test's
// own terms rather than the implementation's.
func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const earthKm = 6371
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthKm * math.Asin(math.Min(1, math.Sqrt(a)))
}
