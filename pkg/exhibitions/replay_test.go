package exhibitions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"museum/internal/models"
)

// countingSite serves a fixed set of pages and records how often each was
// asked for, which is the only thing these tests are about: the same
// exhibitions for fewer requests.
type countingSite struct {
	*httptest.Server

	mu   sync.Mutex
	hits map[string]int
}

func serveCounting(t *testing.T, pages map[string]string) *countingSite {
	t.Helper()

	site := &countingSite{hits: map[string]int{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		site.mu.Lock()
		site.hits[r.URL.Path]++
		site.mu.Unlock()

		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(body))
	})

	site.Server = httptest.NewServer(mux)
	t.Cleanup(site.Server.Close)
	return site
}

func (c *countingSite) count(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits[path]
}

const datedListing = `<html><body>
  <a href="/exhibitions/bronze-age">Bronze Age Britain</a>
  <time datetime="2026-09-01">1 September</time>
  <time datetime="2027-01-15">15 January</time>
</body></html>`

// TestReplaySkipsTheHomePage is the saving. Discovery reads the home page to
// answer one question — which page holds the programme — and that is a thing a
// museum changes far more rarely than it changes the programme itself. A site
// whose listing is where it was last week is worth one request, not two.
func TestReplaySkipsTheHomePage(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an HTTP server and waits out the per-host rate limit")
	}

	site := serveCounting(t, map[string]string{
		"/":            `<html><body><a href="/exhibitions">Exhibitions</a></body></html>`,
		"/exhibitions": datedListing,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	museum := models.Museum{Name: "Example", Website: site.URL}

	first, err := NewScraper().ForSite(ctx, museum, Known{})
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(first.Exhibitions) == 0 {
		t.Fatal("the first read found nothing; the fixture is wrong")
	}
	if !first.Discovered {
		t.Error("a read with nothing known did not report itself as discovery")
	}
	if len(first.ListingPages) != 1 {
		t.Fatalf("Result.ListingPages = %v, want the page the programme came from", first.ListingPages)
	}
	if site.count("/") != 1 {
		t.Fatalf("discovery read the home page %d times, want 1", site.count("/"))
	}

	// Second visit, told what the first one learned.
	second, err := NewScraper().ForSite(ctx, museum, Known{
		ListingURL:   first.ListingURL,
		ListingPages: first.ListingPages,
	})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}

	if site.count("/") != 1 {
		t.Errorf("the home page was read %d times, want 1 — the second read should not have needed it",
			site.count("/"))
	}
	if len(second.Exhibitions) != len(first.Exhibitions) {
		t.Errorf("replay found %d exhibitions, want the %d discovery found",
			len(second.Exhibitions), len(first.Exhibitions))
	}
	if second.Discovered {
		t.Error("a replayed read reported itself as discovery, which would reset the rediscovery clock")
	}
	if second.ListingURL == "" || len(second.ListingPages) != 1 {
		t.Errorf("replay did not carry its pages forward: %+v", second)
	}
}

// TestReplayFallsBackWhenTheProgrammeHasMoved is the safety half. Anything
// short of the known pages still yielding something has to go through
// discovery, because "nothing here" and "it moved" are the same answer, and
// acting on the first retires a museum's whole programme.
func TestReplayFallsBackWhenTheProgrammeHasMoved(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an HTTP server and waits out the per-host rate limit")
	}

	site := serveCounting(t, map[string]string{
		"/":         `<html><body><a href="/whats-on">What's On</a></body></html>`,
		"/whats-on": datedListing,
		"/old-page": `<html><body><p>This page has moved.</p></body></html>`,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := NewScraper().ForSite(ctx,
		models.Museum{Name: "Example", Website: site.URL},
		Known{ListingURL: site.URL + "/old-page", ListingPages: []string{site.URL + "/old-page"}})
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if len(result.Exhibitions) == 0 {
		t.Fatal("a replay that found nothing did not fall through to discovery")
	}
	if !result.Discovered {
		t.Error("the fallback read did not report itself as discovery")
	}
	if site.count("/") != 1 {
		t.Errorf("the home page was read %d times, want 1", site.count("/"))
	}
	if len(result.ListingPages) != 1 || result.ListingPages[0] == site.URL+"/old-page" {
		t.Errorf("Result.ListingPages = %v, want the page that actually worked", result.ListingPages)
	}
}

// TestReplayReadsPermanentPagesAsPermanent is why the two kinds of page are
// kept apart. A page a home page labelled as holding permanent displays lists
// entries that never repeat the claim; read as an ordinary listing, every one
// of them is dropped for having no dates — and a replayed read that came back
// with less than discovery found would retire them.
func TestReplayReadsPermanentPagesAsPermanent(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an HTTP server and waits out the per-host rate limit")
	}

	const standing = `<html><body>
	  <a href="/displays/radios">Radios through the ages</a>
	  <a href="/displays/telegraph">The telegraph room</a>
	</body></html>`

	site := serveCounting(t, map[string]string{
		"/": `<html><body>
		    <a href="/exhibitions">Exhibitions</a>
		    <a href="/permanent">Permanent exhibitions</a>
		  </body></html>`,
		"/exhibitions": datedListing,
		"/permanent":   standing,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	museum := models.Museum{Name: "Example", Website: site.URL}

	first, err := NewScraper().ForSite(ctx, museum, Known{})
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(first.PermanentPages) == 0 {
		t.Fatalf("discovery recorded no permanent pages: %+v", first)
	}

	second, err := NewScraper().ForSite(ctx, museum, Known{
		ListingURL:     first.ListingURL,
		ListingPages:   first.ListingPages,
		PermanentPages: first.PermanentPages,
	})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}

	if site.count("/") != 1 {
		t.Errorf("the home page was read %d times, want 1", site.count("/"))
	}
	if len(second.Exhibitions) != len(first.Exhibitions) {
		t.Fatalf("replay found %d entries, want the %d discovery found — a shortfall here retires listings",
			len(second.Exhibitions), len(first.Exhibitions))
	}

	var permanent int
	for _, e := range second.Exhibitions {
		if e.Permanent {
			permanent++
		}
	}
	if permanent == 0 {
		t.Error("the replayed permanent page yielded no permanent displays, so it was read as an ordinary listing")
	}
}
