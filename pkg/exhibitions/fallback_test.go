package exhibitions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"museum/internal/models"
)

// countingFallback records how often it was consulted.
type countingFallback struct {
	calls atomic.Int64
	found []Exhibition
}

func (f *countingFallback) ForMuseum(context.Context, models.Museum) ([]Exhibition, error) {
	f.calls.Add(1)
	return f.found, nil
}

// siteServing starts a site whose pages come from a map of path to body, and
// which answers 404 for everything else. robots.txt allows everything, since
// the point here is what the scraper decides rather than what it is allowed.
func siteServing(t *testing.T, pages map[string]string) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(body))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// TestFallbackOnlyWhenNothingWasFound is the cost guarantee. The generated
// extractor costs a model invocation the first time it meets a site; the
// heuristics cost nothing. So the expensive path must be reached only for the
// sites the cheap one could read nothing from.
func TestFallbackOnlyWhenNothingWasFound(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an HTTP server and waits out the per-host rate limit")
	}

	// A site the heuristics read perfectly well: a home page linking to a
	// programme, and dated entries on it.
	readable := siteServing(t, map[string]string{
		"/": `<html><body><a href="/exhibitions">Exhibitions</a></body></html>`,
		"/exhibitions": `<html><body>
		  <a href="/exhibition/bronze-age">Bronze Age Britain</a>
		  <time datetime="2026-09-01">1 September</time>
		  <time datetime="2027-01-15">15 January</time>
		</body></html>`,
	})

	fallback := &countingFallback{}
	scraper := NewScraper()
	scraper.Fallback = fallback

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	found, err := scraper.ForMuseum(ctx, models.Museum{Name: "Readable", Website: readable.URL})
	if err != nil {
		t.Fatalf("ForMuseum(readable site) error = %v", err)
	}
	if len(found) == 0 {
		t.Fatal("the heuristics read nothing from a site built for them to read")
	}
	if calls := fallback.calls.Load(); calls != 0 {
		t.Errorf("the fallback was consulted %d times for a site the heuristics read; want 0", calls)
	}
}

func TestFallbackWhenHeuristicsFindNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an HTTP server and waits out the per-host rate limit")
	}

	// A site with a programme rendered in a way the heuristics cannot date:
	// no time elements, no JSON-LD, no month names they know.
	opaque := siteServing(t, map[string]string{
		"/": `<html><body><div id="app"></div></body></html>`,
	})

	fallback := &countingFallback{found: []Exhibition{
		{Title: "Recovered By The Harness", URL: opaque.URL + "/exhibition/one"},
	}}
	scraper := NewScraper()
	scraper.Fallback = fallback

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	found, err := scraper.ForMuseum(ctx, models.Museum{Name: "Opaque", Website: opaque.URL})
	if err != nil {
		t.Fatalf("ForMuseum(opaque site) error = %v", err)
	}

	if calls := fallback.calls.Load(); calls != 1 {
		t.Fatalf("the fallback was consulted %d times for an unreadable site; want 1", calls)
	}

	var recovered bool
	for _, e := range found {
		if e.Title == "Recovered By The Harness" {
			recovered = true
		}
	}
	if !recovered {
		t.Errorf("what the fallback found did not reach the result: %+v", found)
	}
}

// TestNoFallbackByDefault keeps the existing scraper exactly as it was for
// every caller that has not opted in.
func TestNoFallbackByDefault(t *testing.T) {
	if scraper := NewScraper(); scraper.Fallback != nil {
		t.Error("NewScraper() came with a fallback attached; it must be opt-in")
	}
}
