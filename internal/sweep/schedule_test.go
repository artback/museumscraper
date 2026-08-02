package sweep

import (
	"testing"
	"time"
)

func at(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
}

func days(n float64) time.Duration { return time.Duration(n * float64(24*time.Hour)) }

// TestNext_SettlesOnASiteThatNeverChanges is the case the whole package is
// for: a small museum whose site has said the same thing for years should stop
// costing a weekly request.
func TestNext_SettlesOnASiteThatNeverChanges(t *testing.T) {
	now := at(2026, time.August, 1)
	state := State{}

	var sweeps int
	for range 20 {
		plan := Next(state, Unchanged, nil, now)
		state.Interval = plan.Interval
		sweeps++
	}

	if state.Interval != MaxInterval {
		t.Errorf("interval settled at %s, want the %s ceiling", state.Interval, MaxInterval)
	}
	// Reaching the ceiling should take a handful of quiet sweeps, not fifty.
	if got := Next(State{Interval: FirstInterval}, Unchanged, nil, now).Interval; got != days(10.5) {
		t.Errorf("first quiet sweep gave %s, want a week and a half", got)
	}
}

// TestNext_TheFirstReadLearnsNothing guards against every site in the
// catalogue being moved off the starting interval on one sample. A first read
// always differs from the nothing held before it, so treating it as a change
// would halve the interval for all of them at once, in the same direction.
func TestNext_TheFirstReadLearnsNothing(t *testing.T) {
	now := at(2026, time.August, 1)

	for _, outcome := range []Outcome{Changed, Unchanged} {
		plan := Next(State{}, outcome, nil, now)
		if plan.Interval != FirstInterval {
			t.Errorf("%s on a first read gave %s, want the %s it started at",
				outcome, plan.Interval, FirstInterval)
		}
		if plan.Reason != "first read, learning its rate" {
			t.Errorf("%s: Reason = %q", outcome, plan.Reason)
		}
	}

	// The read after it does adapt, because now there is something to compare.
	plan := Next(State{Interval: FirstInterval}, Changed, nil, now)
	if plan.Interval != FirstInterval/2 {
		t.Errorf("second read gave %s, want it halved", plan.Interval)
	}
}

// TestNext_CatchesUpWithASiteThatStartsMoving checks the asymmetry: easing off
// is gentle, but coming back is sharp, because missing a change costs more
// than one extra request.
func TestNext_CatchesUpWithASiteThatStartsMoving(t *testing.T) {
	now := at(2026, time.August, 1)

	settled := State{Interval: MaxInterval}
	plan := Next(settled, Changed, nil, now)
	if plan.Interval != MaxInterval/2 {
		t.Errorf("interval = %s, want it halved from %s", plan.Interval, MaxInterval)
	}

	// And it keeps halving down to the floor rather than stalling.
	state := State{Interval: MaxInterval}
	for range 10 {
		state.Interval = Next(state, Changed, nil, now).Interval
	}
	if state.Interval != MinInterval {
		t.Errorf("interval bottomed out at %s, want the %s floor", state.Interval, MinInterval)
	}
}

// TestNext_AClosingDateBeatsTheRate is the highest-value rule: the one moment
// staleness is certain rather than inferred.
func TestNext_AClosingDateBeatsTheRate(t *testing.T) {
	now := at(2026, time.August, 1)
	closes := at(2026, time.August, 14)

	// A site that looks quiet would otherwise not be read for two months.
	plan := Next(State{Interval: MaxInterval}, Unchanged, &closes, now)

	want := closes.Add(recheckAfterClose)
	if !plan.DueAt.Equal(want) {
		t.Errorf("DueAt = %s, want the day after it closes (%s)", plan.DueAt, want)
	}
	if plan.Reason != "an exhibition closes 2026-08-14" {
		t.Errorf("Reason = %q, should name the closing date", plan.Reason)
	}
}

// TestNext_ADistantClosingDateIsNotBroughtForward: the rule pulls a due date
// nearer, never pushes it out.
func TestNext_ADistantClosingDateIsNotBroughtForward(t *testing.T) {
	now := at(2026, time.August, 1)
	closes := at(2027, time.June, 1)

	plan := Next(State{Interval: MinInterval}, Changed, &closes, now)

	if !plan.DueAt.Equal(now.Add(MinInterval)) {
		t.Errorf("DueAt = %s, want the ordinary %s interval", plan.DueAt, MinInterval)
	}
}

// TestNext_APastClosingDateIsIgnored guards the loop this would otherwise
// cause: a listing that should have been retired but was not would schedule
// its site in the past, and the sweep would read it on every run forever.
func TestNext_APastClosingDateIsIgnored(t *testing.T) {
	now := at(2026, time.August, 1)
	closed := at(2026, time.March, 3)

	plan := Next(State{Interval: FirstInterval}, Unchanged, &closed, now)

	if !plan.DueAt.After(now) {
		t.Fatalf("DueAt = %s, which is not after now (%s)", plan.DueAt, now)
	}
	if !plan.DueAt.Equal(now.Add(clamp(time.Duration(float64(FirstInterval) * growth)))) {
		t.Errorf("DueAt = %s, want the ordinary interval", plan.DueAt)
	}
}

func TestNext_FailuresBackOffThenPark(t *testing.T) {
	now := at(2026, time.August, 1)

	state := State{Interval: FirstInterval}
	var last time.Duration
	for attempt := 1; attempt < MaxFailures; attempt++ {
		plan := Next(state, Failed, nil, now)
		state.ConsecutiveFailures = plan.ConsecutiveFailures

		gap := plan.DueAt.Sub(now)
		if gap <= last {
			t.Errorf("failure %d waited %s, no longer than the previous %s", attempt, gap, last)
		}
		last = gap

		if plan.Park {
			t.Fatalf("parked after %d failures, want %d", attempt, MaxFailures)
		}
		// The interval itself is untouched: the site is not quiet, it is
		// broken, and if it comes back it should resume its old rate.
		if plan.Interval != FirstInterval {
			t.Errorf("Interval = %s, want it left alone at %s", plan.Interval, FirstInterval)
		}
	}

	final := Next(state, Failed, nil, now)
	if !final.Park {
		t.Errorf("still not parked after %d failures", final.ConsecutiveFailures)
	}
	if final.Reason == "" {
		t.Error("a parked site should say why")
	}
}

// TestNext_RecoveryClearsTheFailureCount: a site that answers again rejoins
// the rotation rather than staying on a backoff schedule.
func TestNext_RecoveryClearsTheFailureCount(t *testing.T) {
	now := at(2026, time.August, 1)

	plan := Next(State{Interval: FirstInterval, ConsecutiveFailures: 3}, Changed, nil, now)

	if plan.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want it reset", plan.ConsecutiveFailures)
	}
	if plan.Park {
		t.Error("a site that answered should not be parked")
	}
}
