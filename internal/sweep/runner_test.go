package sweep

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"museum/internal/models"
	"museum/pkg/exhibitions"
)

// recordingStore captures what a read wrote, and nothing else.
type recordingStore struct {
	records []Record
	saved   int
}

func (s *recordingStore) SaveExhibitions(context.Context, []exhibitions.Exhibition) (int64, error) {
	s.saved++
	return 0, nil
}
func (s *recordingStore) RetireUnseen(context.Context, string, time.Time) (int64, error) {
	return 0, nil
}
func (s *recordingStore) TouchSite(context.Context, string, time.Time) (int64, error) { return 0, nil }
func (s *recordingStore) SoonestClose(context.Context, string) (*time.Time, error)    { return nil, nil }
func (s *recordingStore) RecordScrape(_ context.Context, record Record, _ time.Time) error {
	s.records = append(s.records, record)
	return nil
}

// TestReadRecordsARobotsRefusalAsExcluded checks the wiring between the
// fetcher's refusal and the scheduler's decision. Read on its own would report
// a site that forbids crawling as Failed, which schedules five more requests to
// a site that has already said no.
func TestReadRecordsARobotsRefusalAsExcluded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><a href="/exhibitions">What's on</a></html>`))
	}))
	defer srv.Close()

	store := &recordingStore{}
	runner := NewRunner(store, exhibitions.NewScraper())

	report := runner.Read(context.Background(), Target{
		Site:   "example.test",
		Museum: models.Museum{Name: "Example Museum", Website: srv.URL},
	})

	if report.Outcome != Excluded {
		t.Errorf("Outcome = %s, want excluded", report.Outcome)
	}
	if !report.Parked {
		t.Error("a site that refused the crawl was not parked")
	}
	if store.saved != 0 {
		t.Error("exhibitions were stored for a site that was never read")
	}
	if len(store.records) != 1 || store.records[0].Outcome != Excluded {
		t.Errorf("records = %+v, want one excluded record", store.records)
	}
}

// TestReadReplaysKnownPagesUntilTheyAreDueForRediscovery is the bound on the
// saving. Replaying the pages a site's listings came from is what makes a
// steady read cost one request instead of two, and the home page is the only
// thing that would ever report the site has added a page beside them — so it
// has to be read again eventually, and eventually has to be a bounded word.
func TestReadReplaysKnownPagesUntilTheyAreDueForRediscovery(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an HTTP server and waits out the per-host rate limit")
	}

	var home int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
		case "/":
			home++
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><a href="/exhibitions">What's on</a></html>`))
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body>
			  <a href="/exhibitions/bronze-age">Bronze Age Britain</a>
			  <time datetime="2026-09-01">1 September</time>
			  <time datetime="2027-01-15">15 January</time>
			</body></html>`))
		}
	}))
	defer srv.Close()

	target := Target{
		Site:         "example.test",
		Museum:       models.Museum{Name: "Example Museum", Website: srv.URL},
		ListingURL:   srv.URL + "/exhibitions",
		ListingPages: []string{srv.URL + "/exhibitions"},
		DiscoveredAt: time.Now().Add(-RediscoverAfter / 2),
	}

	store := &recordingStore{}
	if report := NewRunner(store, exhibitions.NewScraper()).Read(context.Background(), target); report.Found == 0 {
		t.Fatalf("a replayed read found nothing: %+v", report)
	}
	if home != 0 {
		t.Errorf("the home page was read %d times on a replayed read, want 0", home)
	}
	if last := store.records[len(store.records)-1]; last.Discovered {
		t.Error("a replayed read was recorded as discovery, which would postpone the next one")
	}

	// The same site, with its pages found longer ago than the window allows.
	target.DiscoveredAt = time.Now().Add(-2 * RediscoverAfter)
	store = &recordingStore{}
	if report := NewRunner(store, exhibitions.NewScraper()).Read(context.Background(), target); report.Found == 0 {
		t.Fatalf("the rediscovering read found nothing: %+v", report)
	}
	if home != 1 {
		t.Errorf("the home page was read %d times, want 1 — a site overdue for rediscovery must be looked at again", home)
	}
	if last := store.records[len(store.records)-1]; !last.Discovered {
		t.Error("the rediscovering read was not recorded as discovery, so the clock never moves on")
	}
}
