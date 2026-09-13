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
