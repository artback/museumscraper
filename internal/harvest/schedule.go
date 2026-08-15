package harvest

import (
	"context"
	"log"
	"math/rand/v2"
	"sync"
	"time"

	"museum/pkg/extract"
)

// Scheduler defaults.
const (
	// DefaultTick is how often the scheduler looks for work. It is not the
	// cadence anything runs at — that is per source — only how finely a
	// source's own interval can be honoured.
	DefaultTick = time.Minute

	// DefaultConcurrency is how many sources may run at once. Low, because
	// each one is a fetch of somebody else's website and the politeness that
	// matters is the aggregate.
	DefaultConcurrency = 4

	// DefaultJitter spreads the start of runs that came due together.
	//
	// Sources defined in one sitting get the same interval and, without this,
	// the same due time forever after: fifty museums added on a Tuesday would
	// be fetched in the same second every day. The jitter is applied per run
	// rather than per source so the alignment does not simply re-form.
	DefaultJitter = 30 * time.Second
)

// Scheduler runs sources on their own cadences.
//
// It holds no queue. A source that is still running when it next comes due is
// skipped rather than queued, because the run that is in flight is fetching
// the same page the queued one would: letting them stack up would multiply
// requests to a site that is already answering slowly, which is exactly when
// it should be asked less.
type Scheduler struct {
	// Harvester runs one source.
	Harvester *Harvester
	// Store supplies the sources and their history.
	Store Archive

	// Tick is how often to look for due sources. Zero means DefaultTick.
	Tick time.Duration
	// Concurrency bounds simultaneous runs. Zero means DefaultConcurrency.
	Concurrency int
	// Jitter is the maximum delay added before a run. Zero means
	// DefaultJitter; negative disables it.
	Jitter time.Duration

	// Now supplies the current time. Nil means time.Now.
	Now func() time.Time

	// inFlight is the set of sources currently running. It is a map guarded by
	// a mutex rather than a channel because the question asked of it —  "is
	// this name running?" — is a lookup in shared state, not a handoff.
	mu       sync.Mutex
	inFlight map[string]bool
}

// Run schedules sources until ctx is cancelled.
//
// It returns only when every run it started has finished, so a caller that
// cancels and returns knows nothing is still touching the store.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.tick())
	defer ticker.Stop()

	slots := make(chan struct{}, s.concurrency())
	var wg sync.WaitGroup

	// Wait before returning, whichever way this exits. A scheduler that
	// returned while runs were still writing to the store would let a caller
	// close the process out from under them.
	defer wg.Wait()

	log.Printf("harvest: scheduling every %s, %d sources at a time", s.tick(), s.concurrency())

	for {
		select {
		case <-ctx.Done():
			log.Printf("harvest: stopping, waiting for runs in flight")
			return ctx.Err()

		case <-ticker.C:
			due, err := s.due(ctx)
			if err != nil {
				// A store that cannot be listed this minute may well be
				// listable the next, and a scheduler is a long-running
				// service: it complains and carries on.
				log.Printf("harvest: could not list due sources: %v", err)
				continue
			}

			for _, source := range due {
				if !s.claim(source.Name) {
					log.Printf("harvest: %s is still running, skipping this tick", source.Name)
					continue
				}

				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					s.release(source.Name)
					return ctx.Err()
				}

				wg.Add(1)
				go func(source extract.Source) {
					defer wg.Done()
					defer func() { <-slots }()
					defer s.release(source.Name)

					s.runOne(ctx, source)
				}(source)
			}
		}
	}
}

// runOne waits out the jitter and then runs a source.
func (s *Scheduler) runOne(ctx context.Context, source extract.Source) {
	if delay := s.jitter(); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		}
	}

	outcome, err := s.Harvester.Once(ctx, source)
	switch {
	case err != nil:
		log.Printf("harvest: %s failed: %v", source.Name, err)
	case outcome.Quarantined:
		log.Printf("harvest: %s quarantined: %s", source.Name, outcome.Alert)
	default:
		log.Printf("harvest: %s %s, %d records%s", source.Name,
			outcome.Run.Verdict, outcome.Run.Records, published(outcome))
	}
}

func published(outcome Outcome) string {
	if outcome.Published {
		return ", published"
	}
	return ", held"
}

// due returns the sources that should run now.
func (s *Scheduler) due(ctx context.Context) ([]extract.Source, error) {
	sources, err := s.Store.Sources(ctx)
	if err != nil {
		return nil, err
	}

	now := s.now()
	var due []extract.Source

	for _, source := range sources {
		// A paused source costs nothing and is skipped before anything is
		// read on its behalf. Quarantine works by setting this, so a source
		// that needs a human stops consuming fetches and model invocations
		// the moment it is quarantined.
		if source.Paused || source.Every <= 0 {
			continue
		}

		// Asked by key rather than by reading runs: this happens for every
		// source on every tick, and downloading a source's whole retained
		// history to find out when it last ran made a tick cost hundreds of
		// object GETs.
		lastRun, ran, err := s.Store.LastRunAt(ctx, source.Name)
		if err != nil {
			log.Printf("harvest: could not read %s history, skipping: %v", source.Name, err)
			continue
		}
		if ran && now.Sub(lastRun) < source.Every.Every() {
			continue
		}
		due = append(due, source)
	}
	return due, nil
}

// claim marks a source as running, reporting false when it already was.
func (s *Scheduler) claim(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.inFlight[name] {
		return false
	}
	if s.inFlight == nil {
		s.inFlight = make(map[string]bool)
	}
	s.inFlight[name] = true
	return true
}

func (s *Scheduler) release(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, name)
}

func (s *Scheduler) tick() time.Duration {
	if s.Tick > 0 {
		return s.Tick
	}
	return DefaultTick
}

func (s *Scheduler) concurrency() int {
	if s.Concurrency > 0 {
		return s.Concurrency
	}
	return DefaultConcurrency
}

func (s *Scheduler) jitter() time.Duration {
	switch {
	case s.Jitter < 0:
		return 0
	case s.Jitter == 0:
		return rand.N(DefaultJitter)
	default:
		return rand.N(s.Jitter)
	}
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}
