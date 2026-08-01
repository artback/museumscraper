package command

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"museum/pkg/graceful"

	"museum/internal/postgres"
	"museum/internal/sweep"
	"museum/pkg/exhibitions"
)

// sweepCommand keeps the catalogue's exhibitions current.
//
// It is the freshness half of the pair. "refresh" reads an area because a
// caller asked for that area; "sweep" reads whatever the catalogue itself
// expects to be out of date, wherever in the world that is. The difference is
// who chooses the museums, which is why they are separate commands rather than
// a flag on one.
//
// It runs continuously by default, like "enrich", because keeping data fresh
// is not a thing that finishes. -once makes it a batch job for a cron instead.
func sweepCommand() Command {
	return Command{
		Name:    "sweep",
		Summary: "Keep exhibitions current by rereading the sites expected to be stale",
		Usage:   "[-once] [-dry-run] [-batch 200] [-concurrency 8] [-rate 60]",
		Run:     runSweepCommand,
	}
}

func runSweepCommand(ctx context.Context, args []string) error {
	fs := newFlagSet("sweep", "[-once] [-dry-run] [-batch 200] [-concurrency 8] [-rate 60]", os.Stderr)
	var (
		once        = fs.Bool("once", false, "read one batch and stop, instead of running continuously")
		dryRun      = fs.Bool("dry-run", false, "list what would be read, and why, without reading it")
		batch       = fs.Int("batch", 200, "how many sites to claim at a time")
		concurrency = fs.Int("concurrency", 8, "how many museum sites to read at once")
		rate        = fs.Float64("rate", 60, "ceiling on sites read per minute (0 for no limit)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs("sweep", fs.Args()); err != nil {
		return err
	}

	ctx, cancel := graceful.Context(ctx)
	defer cancel()

	if *dryRun || *once {
		return runSweepOnce(ctx, *batch, *concurrency, *dryRun)
	}
	return runSweepLoop(ctx, *batch, *concurrency, *rate)
}

// runSweepOnce reads a single batch, for a cron that would rather schedule the
// work itself, and for the dry run that shows what the scheduler has decided.
func runSweepOnce(ctx context.Context, budget, concurrency int, dryRun bool) error {
	db, err := database(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	now := time.Now()
	if _, err := db.DiscoverSites(ctx); err != nil {
		return err
	}

	var due []sweep.Target
	if dryRun {
		// A dry run must not claim anything: it is meant to show what the
		// scheduler has decided, not to push those sites out of the queue.
		due, err = db.DueSites(ctx, now, budget)
	} else {
		due, err = db.ClaimDueSites(ctx, now, budget, claimLease)
	}
	if err != nil {
		return err
	}

	status, err := db.SweepStatus(ctx, now)
	if err != nil {
		return err
	}
	log.Printf("%d sites known, %d read at least once, %d never, %d due now, %d parked; median interval %.0f days",
		status.SitesKnown, status.SitesRead, status.SitesNever, status.SitesDue,
		status.SitesParked, status.MedianIntervalHours/24)

	if len(due) == 0 {
		log.Print("Nothing is due; the catalogue is as fresh as it is scheduled to be")
		return nil
	}
	if dryRun {
		return describeSweep(due)
	}

	log.Printf("Sweeping %d sites (%d concurrent)...", len(due), concurrency)
	return runSweepJobs(ctx, db, due, concurrency, now)
}

// describeSweep prints what a sweep would do without touching any museum's
// site, which is how a change to the scheduler gets reviewed before it costs
// anyone else's bandwidth.
func describeSweep(due []sweep.Target) error {
	for _, site := range due {
		when := "never read"
		if !site.NeverRead {
			when = fmt.Sprintf("every %.0f days", site.State.Interval.Hours()/24)
		}
		conditional := "full read"
		if site.ListingURL != "" && (site.ETag != "" || site.LastModified != "") {
			conditional = "conditional"
		}
		log.Printf("  %-40s %-14s %-12s %s", trim(site.Site, 40), when, conditional, site.Museum.Name)
	}
	log.Printf("Dry run: %d sites would be read, none were", len(due))
	return nil
}

// runSweepJobs reads the due sites and records each result.
//
// Each site's bookkeeping — save, retire, reschedule — happens as soon as that
// site is done rather than at the end. A sweep is long and interruptible, and
// work already paid for should survive whatever stops the run.
func runSweepJobs(ctx context.Context, db *postgres.Store, due []sweep.Target, concurrency int, start time.Time) error {
	runner := sweep.NewRunner(db, exhibitions.NewScraper())

	jobs := make(chan sweep.Target)
	results := make(chan sweep.Report, len(due))

	var wg sync.WaitGroup
	for range min(concurrency, len(due)) {
		wg.Go(func() {
			for target := range jobs {
				results <- runner.Read(ctx, target)
			}
		})
	}

	go func() {
		defer close(jobs)
		for _, target := range due {
			select {
			case jobs <- target:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	var (
		byOutcome = map[sweep.Outcome]int{}
		found     int
		retired   int64
		parked    int
	)
	for report := range results {
		byOutcome[report.Outcome]++
		found += report.Found
		retired += report.Retired
		if report.Parked {
			parked++
		}
	}

	log.Printf("Sweep finished in %s: %d changed, %d unchanged, %d failed; %d exhibitions seen, %d retired, %d parked",
		time.Since(start).Round(time.Second),
		byOutcome[sweep.Changed], byOutcome[sweep.Unchanged], byOutcome[sweep.Failed],
		found, retired, parked)
	return ctx.Err()
}

// trim shortens a string for a log column.
func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
