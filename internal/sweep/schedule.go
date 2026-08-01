// Package sweep decides when a museum's website is next worth reading.
//
// The scheduling problem here is not "how often should we scrape". Refreshing
// everything on one cadence gets both halves wrong at once: the Tate changes
// its programme faster than a weekly sweep notices, and a village radio museum
// whose site last changed in 2019 is read fifty times a year to confirm it
// still says the same thing. With thousands of sites, a polite crawler and one
// request per host per second, the sweep budget is the scarce resource and
// spending it evenly wastes most of it.
//
// So the question this package answers is narrower and more useful: given what
// happened last time, when is this site next likely to be *wrong*? Three
// signals decide it, in increasing order of how much they are trusted.
//
// The interval adapts to what the site actually does. A sweep that finds no
// change lengthens the gap; one that finds a change halves it. Nobody
// configures this per museum and nobody could — it settles each site where its
// own publishing rate puts it, and follows when that rate changes.
//
// A failure backs off exponentially and eventually parks the site, because a
// site returning 403 to every request is not going to start answering because
// we asked a fiftieth time, and the requests it costs are taken from museums
// that would have answered.
//
// A closing date overrides both. If a stored exhibition closes on the 14th
// then on the 15th our copy is wrong, whatever the site's usual rate is — this
// is the one moment staleness is not a guess but a certainty, and it is worth
// more than any amount of inference about how often the page tends to move.
package sweep

import (
	"fmt"
	"time"
)

// Outcome is what one attempt at a site produced.
type Outcome int

const (
	// Changed means the site returned something other than what we held.
	Changed Outcome = iota
	// Unchanged means it returned what we already had — including the case
	// where the site answered 304 and returned nothing at all.
	Unchanged
	// Failed means the site could not be read: refused, timed out, gone.
	Failed
)

func (o Outcome) String() string {
	switch o {
	case Changed:
		return "changed"
	case Unchanged:
		return "unchanged"
	case Failed:
		return "failed"
	}
	return "unknown"
}

const (
	// FirstInterval is where a site starts before anything is known about it.
	// A week is short enough to learn a site's rate within a month and long
	// enough that seeding thousands of sites does not swamp the first sweeps.
	FirstInterval = 7 * 24 * time.Hour

	// MinInterval is the floor. Museum programmes turn over in weeks; reading
	// a site twice a day could not find more than reading it every other day,
	// and the politeness budget is better spent on breadth.
	MinInterval = 2 * 24 * time.Hour

	// MaxInterval is the ceiling. Even a site that has not changed in two
	// years gets looked at every couple of months, because the alternative is
	// never noticing that it finally did.
	MaxInterval = 60 * 24 * time.Hour

	// growth is how much an unchanged read lengthens the gap. Gentle on
	// purpose: overshooting means missing a change for weeks, and the cost of
	// being wrong is asymmetric with the cost of one more request.
	growth = 1.5

	// shrink is how much a change shortens it. Sharper than growth, so a site
	// that starts moving is caught up with quickly rather than over a month of
	// halvings.
	shrink = 2.0

	// failureBackoff is the gap after a first failure, doubling per
	// consecutive failure.
	failureBackoff = 12 * time.Hour

	// maxBackoffShift caps that doubling at 12h << 5, about sixteen days.
	maxBackoffShift = 5

	// MaxFailures is how many consecutive failures a site gets before it is
	// parked and stops costing anything.
	MaxFailures = 6

	// recheckAfterClose is how long after an exhibition closes the site is
	// read again. A day, because a museum updates its listings around the
	// closing date rather than at midnight on it.
	recheckAfterClose = 24 * time.Hour
)

// State is what the scheduler remembers about a site between sweeps.
type State struct {
	// Interval is the current gap between reads. Zero for a site never read.
	Interval time.Duration
	// ConsecutiveFailures is how many attempts in a row have failed.
	ConsecutiveFailures int
}

// Plan is when to come back to a site, and why.
type Plan struct {
	Interval            time.Duration
	DueAt               time.Time
	ConsecutiveFailures int

	// Park is set when the site has failed too often to keep paying for.
	Park bool

	// Reason says which rule chose this date. It is carried into the database
	// and into the sweep's log because a scheduler whose decisions cannot be
	// read back is one nobody can tell is working: "parked after 6 failures"
	// and "an exhibition closes 2026-08-14" are the difference between a
	// sweep that looks idle and one that is behaving correctly.
	Reason string
}

// Next decides when a site should be read again.
//
// soonestClose is the earliest closing date among the exhibitions currently
// held for the site, or nil when it holds none that close. A date already past
// is ignored: it means a listing that should have been retired was not, and
// acting on it would schedule the site in the past and sweep it every run.
func Next(state State, outcome Outcome, soonestClose *time.Time, now time.Time) Plan {
	interval := state.Interval
	if interval <= 0 {
		interval = FirstInterval
	}

	if outcome == Failed {
		failures := state.ConsecutiveFailures + 1
		if failures >= MaxFailures {
			return Plan{
				Interval:            interval,
				DueAt:               now.Add(MaxInterval),
				ConsecutiveFailures: failures,
				Park:                true,
				Reason:              fmt.Sprintf("parked after %d consecutive failures", failures),
			}
		}
		backoff := failureBackoff << min(failures-1, maxBackoffShift)
		return Plan{
			Interval:            interval,
			DueAt:               now.Add(backoff),
			ConsecutiveFailures: failures,
			Reason:              fmt.Sprintf("failure %d of %d, retrying in %s", failures, MaxFailures, round(backoff)),
		}
	}

	reason := "unchanged, easing off"
	switch {
	case state.Interval <= 0:
		// The first successful read has nothing to compare against, so it says
		// nothing about how often this site changes. Adapting on it would move
		// every site in the catalogue off the starting interval on the
		// strength of one sample — in the same direction, since a first read
		// always differs from the nothing held before it.
		reason = "first read, learning its rate"
	case outcome == Unchanged:
		interval = clamp(time.Duration(float64(interval) * growth))
	case outcome == Changed:
		interval = clamp(time.Duration(float64(interval) / shrink))
		reason = "changed, checking sooner"
	}

	plan := Plan{Interval: interval, DueAt: now.Add(interval), Reason: reason}

	// A known closing date beats any inference about the site's rate.
	if soonestClose != nil {
		recheck := soonestClose.Add(recheckAfterClose)
		if recheck.After(now) && recheck.Before(plan.DueAt) {
			plan.DueAt = recheck
			plan.Reason = "an exhibition closes " + soonestClose.Format("2006-01-02")
		}
	}
	return plan
}

// clamp holds an interval inside the floor and ceiling.
func clamp(interval time.Duration) time.Duration {
	return min(max(interval, MinInterval), MaxInterval)
}

// round trims a duration to something readable in a log line.
func round(d time.Duration) time.Duration { return d.Round(time.Hour) }
