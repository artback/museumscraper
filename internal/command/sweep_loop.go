package command

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"museum/internal/postgres"
	"museum/internal/sweep"
	"museum/pkg/exhibitions"
)

const (
	// claimLease is how long a claimed site is held before it falls due again
	// on its own. Longer than any single site takes to read, short enough that
	// a sweeper killed mid-batch does not strand its work for an afternoon.
	claimLease = 30 * time.Minute

	// idleWait is how long to wait when nothing is due. The catalogue changes
	// slowly and due dates are days apart, but a crawl can add thousands of
	// museums at any moment, so this is short enough to pick those up promptly
	// and long enough that an idle sweeper is doing nothing but one small
	// query a minute.
	idleWait = time.Minute

	// reportEvery is how often a running sweeper says where it has got to.
	// Without it a process that is working correctly and a process that is
	// wedged look identical in the logs.
	reportEvery = 5 * time.Minute
)

// runSweepLoop reads due sites continuously until the context is cancelled.
//
// The loop is deliberately dull: claim a batch, read it, record it, repeat.
// Everything that decides what to read and when to come back lives in the
// database and in the scheduler, so the process holds no state of its own and
// a restart resumes exactly where it left off — which is what makes it safe to
// stop and safe to run more than one of.
func runSweepLoop(ctx context.Context, batch, concurrency int, rate float64) error {
	db, err := database(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	runner := sweep.NewRunner(db, exhibitions.NewScraper())
	pace := newPacer(rate)
	defer pace.stop()

	var (
		started    = time.Now()
		lastReport = time.Now()
		total      totals
	)

	log.Printf("Sweeping continuously: %d sites per batch, %d at once, up to %.0f sites/minute",
		batch, concurrency, rate)

	for ctx.Err() == nil {
		// Newly crawled museums enter here, at the top of every cycle, rather
		// than through a separate seeding step somebody has to remember.
		if fresh, err := db.DiscoverSites(ctx); err != nil {
			log.Printf("sweep: discovering sites: %v", err)
		} else if fresh > 0 {
			log.Printf("Found %d museum sites not seen before; they lead the queue", fresh)
		}

		claimed, err := db.ClaimDueSites(ctx, time.Now(), batch, claimLease)
		if err != nil {
			log.Printf("sweep: claiming: %v", err)
			if !wait(ctx, idleWait) {
				break
			}
			continue
		}
		if len(claimed) == 0 {
			if !wait(ctx, idleWait) {
				break
			}
			continue
		}

		total.add(sweepBatch(ctx, runner, claimed, concurrency, pace))

		if time.Since(lastReport) >= reportEvery {
			total.report(ctx, db, started)
			lastReport = time.Now()
		}
	}

	log.Printf("Sweeper stopped after %s: %s", time.Since(started).Round(time.Second), total)
	return nil
}

// sweepBatch reads one claimed batch, bounded by the pacer.
func sweepBatch(ctx context.Context, runner *sweep.Runner, claimed []sweep.Target, concurrency int, pace *pacer) totals {
	reports := make(chan sweep.Report, len(claimed))
	jobs := make(chan sweep.Target)

	var wg sync.WaitGroup
	for range min(concurrency, len(claimed)) {
		wg.Go(func() {
			for target := range jobs {
				// Held here rather than inside the reader, so the limit is on
				// how fast the catalogue is swept overall and not on any one
				// site. Per-host politeness is the fetcher's job and is
				// enforced separately.
				if !pace.wait(ctx) {
					return
				}
				reports <- runner.Read(ctx, target)
			}
		})
	}

	go func() {
		defer close(jobs)
		for _, target := range claimed {
			select {
			case jobs <- target:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(reports)
	}()

	var batch totals
	for report := range reports {
		batch.count(report)
	}
	return batch
}

// totals is what a sweeper has done, for the periodic report.
type totals struct {
	changed, unchanged, failed int
	found                      int
	retired                    int64
	parked                     int
}

func (t *totals) count(report sweep.Report) {
	switch report.Outcome {
	case sweep.Changed:
		t.changed++
	case sweep.Unchanged:
		t.unchanged++
	case sweep.Failed:
		t.failed++
	}
	t.found += report.Found
	t.retired += report.Retired
	if report.Parked {
		t.parked++
	}
}

func (t *totals) add(other totals) {
	t.changed += other.changed
	t.unchanged += other.unchanged
	t.failed += other.failed
	t.found += other.found
	t.retired += other.retired
	t.parked += other.parked
}

func (t totals) String() string {
	return fmt.Sprintf("%d changed, %d unchanged, %d failed; %d exhibitions seen, %d retired, %d parked",
		t.changed, t.unchanged, t.failed, t.found, t.retired, t.parked)
}

// report says where the sweeper has got to, and how far behind the catalogue
// is, so a healthy sweeper and a stuck one do not look the same.
func (t totals) report(ctx context.Context, db *postgres.Store, since time.Time) {
	log.Printf("Swept for %s: %s", time.Since(since).Round(time.Second), t)

	status, err := db.SweepStatus(ctx, time.Now())
	if err != nil {
		return
	}
	log.Printf("  %d sites known, %d never read, %d due now, %d parked; median interval %.0f days",
		status.SitesKnown, status.SitesNever, status.SitesDue, status.SitesParked,
		status.MedianIntervalHours/24)
}

// pacer spreads reads out over time, so a sweeper with a large backlog does not
// spend its first hours running flat out.
//
// The fetcher already keeps one request per host per second, which bounds what
// any single museum sees. This bounds what the sweep as a whole does: on a
// first run every site in the catalogue is due at once, and without a ceiling
// the sweeper would open as many connections as its concurrency allows for as
// long as the backlog lasts.
type pacer struct {
	ticker *time.Ticker
}

// newPacer returns a pacer allowing at most perMinute reads a minute. A rate of
// zero or less is unlimited.
//
// One ticker, shared by every worker: a tick goes to exactly one of them,
// which is the behaviour wanted and needs neither a goroutine to feed it nor
// any locking around it.
func newPacer(perMinute float64) *pacer {
	if perMinute <= 0 {
		return &pacer{}
	}
	return &pacer{ticker: time.NewTicker(time.Duration(float64(time.Minute) / perMinute))}
}

// wait blocks until another read may start, reporting false if the context
// ended first.
func (p *pacer) wait(ctx context.Context) bool {
	if p.ticker == nil {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-p.ticker.C:
		return true
	}
}

// stop releases the ticker.
func (p *pacer) stop() {
	if p.ticker != nil {
		p.ticker.Stop()
	}
}

// wait sleeps unless the context ends first, reporting whether to carry on.
func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
