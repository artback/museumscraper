package sweep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"museum/internal/models"
	"museum/pkg/exhibitions"
)

// Reading one site is here, rather than in whichever command happens to want
// it, because two things read museum websites and both have to leave the same
// trace.
//
// The scheduled sweep reads what is due. The API reads an area on demand, when
// someone looks at a city nobody has scraped yet. When only the sweep recorded
// its attempts, the two worked against each other: the sweep re-read sites the
// API had read minutes earlier, none of the API's work taught the scheduler
// anything, listings that vanished were only retired down one of the two
// paths, and — worst, because it is visible to callers — an area the API had
// just scraped still reported "nobody has looked here yet", because the
// coverage report reads the attempt record and the API was not writing one.

// Target is a site to read, with what is already known about it.
type Target struct {
	// Site is the host, which is the unit that gets fetched. Museums share
	// websites, so this is not one per museum.
	Site string
	// Museum is the most prominent museum published on it, and the one the
	// exhibitions found are attributed to.
	Museum models.Museum

	// ListingURL and the validators are where its listings were last read
	// from, for the conditional request that asks whether anything moved.
	ListingURL   string
	ETag         string
	LastModified string

	// Fingerprint digests what the last successful read produced.
	Fingerprint string

	State State

	// NeverRead is true for a site with no record at all.
	NeverRead bool
}

// Record is one attempt's result, written whatever the outcome.
type Record struct {
	Site        string
	Plan        Plan
	Outcome     Outcome
	FoundCount  int
	Fingerprint string

	ListingURL   string
	ETag         string
	LastModified string
}

// Store is what reading a site needs from the database.
type Store interface {
	SaveExhibitions(ctx context.Context, found []exhibitions.Exhibition) (int64, error)
	RetireUnseen(ctx context.Context, site string, seenFrom time.Time) (int64, error)
	TouchSite(ctx context.Context, site string, now time.Time) (int64, error)
	SoonestClose(ctx context.Context, site string) (*time.Time, error)
	RecordScrape(ctx context.Context, record Record, now time.Time) error
}

// Runner reads sites and records what each one cost.
type Runner struct {
	store   Store
	scraper *exhibitions.Scraper
}

// NewRunner returns a Runner over the given store.
func NewRunner(store Store, scraper *exhibitions.Scraper) *Runner {
	if scraper == nil {
		scraper = exhibitions.NewScraper()
	}
	return &Runner{store: store, scraper: scraper}
}

// Report is what reading one site produced.
type Report struct {
	Site    string
	Outcome Outcome
	Found   int
	Retired int64
	DueAt   time.Time
	Reason  string
	Parked  bool
}

// Read reads one site, stores what it lists, retires what it no longer lists,
// and schedules the next visit.
func (r *Runner) Read(ctx context.Context, target Target) Report {
	// The moment the read started, not the moment it ended: anything last seen
	// before this was not seen by this read, and using the end time would
	// retire listings found early in a slow read.
	startedAt := time.Now()

	result, err := r.scraper.ForSite(ctx, target.Museum, exhibitions.Known{
		ListingURL: target.ListingURL,
		Validators: exhibitions.Validators{ETag: target.ETag, LastModified: target.LastModified},
	})

	outcome := Changed
	record := Record{Site: target.Site}

	switch {
	case err != nil && !errors.Is(err, exhibitions.ErrNoWebsite):
		outcome = Failed
	case err != nil:
		// No website to read is not a failure of the site; nothing to do.
		outcome = Unchanged
	case !result.Reached:
		// Not one page answered. The scraper reports this as an ordinary empty
		// result, because it skips failed pages quietly by design, and without
		// checking here a dead host would be recorded as a museum that lists
		// nothing and reread on the normal schedule forever.
		outcome = Failed
	case result.Unchanged:
		outcome = Unchanged
		// The site's word that its listings have not moved. Everything held
		// for it is still current, and saying so is what stops the next
		// successful read from retiring all of it.
		if _, err := r.store.TouchSite(ctx, target.Site, startedAt); err != nil {
			log.Printf("sweep: %s: %v", target.Site, err)
		}
		record.ListingURL, record.ETag, record.LastModified = target.ListingURL, target.ETag, target.LastModified
		record.Fingerprint = target.Fingerprint
	default:
		record.Fingerprint = Fingerprint(result.Exhibitions)
		if record.Fingerprint == target.Fingerprint {
			outcome = Unchanged
		}
		record.ListingURL = result.ListingURL
		record.ETag, record.LastModified = result.Validators.ETag, result.Validators.LastModified
	}

	report := Report{Site: target.Site, Outcome: outcome}

	if outcome != Failed && len(result.Exhibitions) > 0 {
		if _, err := r.store.SaveExhibitions(ctx, result.Exhibitions); err != nil {
			log.Printf("sweep: %s: %v", target.Site, err)
		}
		report.Found = len(result.Exhibitions)
		record.FoundCount = len(result.Exhibitions)

		// Retire only after a read that found something. A site answering with
		// an empty page is far more often blocked or JavaScript-rendered than
		// genuinely closed, and acting on that erases a whole programme.
		if gone, err := r.store.RetireUnseen(ctx, target.Site, startedAt); err != nil {
			log.Printf("sweep: %s: %v", target.Site, err)
		} else {
			report.Retired = gone
		}
	}

	soonest, err := r.store.SoonestClose(ctx, target.Site)
	if err != nil {
		log.Printf("sweep: %s: %v", target.Site, err)
	}

	record.Outcome = outcome
	record.Plan = Next(target.State, outcome, soonest, startedAt)
	if err := r.store.RecordScrape(ctx, record, startedAt); err != nil {
		log.Printf("sweep: %s: %v", target.Site, err)
	}

	report.DueAt, report.Reason, report.Parked = record.Plan.DueAt, record.Plan.Reason, record.Plan.Park
	return report
}

// Fingerprint digests what a read produced, so "did anything change" is an
// exact question.
//
// Titles and dates as well as URLs: a museum that pushes a closing date back
// has changed something worth knowing about, and a digest of the URLs alone
// would call that no change and let the interval drift longer.
func Fingerprint(found []exhibitions.Exhibition) string {
	lines := make([]string, 0, len(found))
	for _, e := range found {
		var start, end string
		if e.Start != nil {
			start = e.Start.Format("2006-01-02")
		}
		if e.End != nil {
			end = e.End.Format("2006-01-02")
		}
		lines = append(lines, strings.Join([]string{e.URL, e.Title, start, end,
			fmt.Sprint(e.Permanent)}, "\x00"))
	}
	// Sorted, because the order entries come off a page is not a change.
	sort.Strings(lines)

	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}
