package harvest

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/artback/museumscraper/extract"
	"museum/internal/models"
	"museum/pkg/exhibitions"
)

// ExhibitionSchema is the schema every generated museum extractor is compiled
// against.
//
// It is written here, by hand, and never by a model — which is the division
// the whole design rests on. The model decides how to read a page; this
// decides what a correct reading looks like, so a model that has misunderstood
// a site produces output that fails a check it did not write.
//
// The placeholder lists are the useful part. They are the exact wrong answers
// a museum listing page invites: the "Find out more" button that sits next to
// every title and gets collected instead of it when a selector is aimed one
// element too high. Naming them is worth more than any amount of general
// instruction to the model not to guess.
func ExhibitionSchema() extract.Schema {
	return extract.Schema{
		Name:   "exhibitions",
		Intent: "the exhibitions and displays currently on show at this museum",
		Fields: []extract.Field{
			{
				Name:        "title",
				Kind:        extract.KindString,
				Required:    true,
				Description: "the exhibition's own name, as a visitor would read it",
				Rules: extract.Rules{
					MinLength: 2,
					MaxLength: 300,
					Placeholders: []string{
						"Find out more", "Read more", "More info", "More information",
						"Book now", "Book tickets", "Buy tickets", "Learn more",
						"See more", "View", "View all", "Details", "Exhibition",
						"Exhibitions", "What's On", "Whats On", "Mehr erfahren",
						"En savoir plus", "Läs mer", "Meer informatie", "Ver más",
					},
				},
			},
			{
				Name:        "url",
				Kind:        extract.KindURL,
				Required:    true,
				Description: "link to the exhibition's own page on this site",
			},
			{
				Name:        "start",
				Kind:        extract.KindDate,
				Description: "opening date, ISO 8601, omitted when the page does not give one",
				// Ten years either side. A listing dated further out than that
				// is a site using the year 3000 to mean "no end date", which is
				// a real defect in an opening date rather than a long run.
				Rules: extract.Rules{Min: ptr(-3660.0), Max: ptr(3660.0)},
			},
			{
				Name:        "end",
				Kind:        extract.KindDate,
				Description: "closing date, ISO 8601, omitted when the page does not give one",
				Rules:       extract.Rules{Min: ptr(-3660.0), Max: ptr(3660.0)},
			},
			{
				Name:        "permanent",
				Kind:        extract.KindBool,
				Description: "true when the museum keeps this on show indefinitely",
			},
		},
	}
}

func ptr[T any](v T) *T { return &v }

// ExhibitionFallback reads a museum's site with a generated extractor.
//
// It satisfies exhibitions.Fallback, and is reached only for the museums whose
// sites the hand-written heuristics could read nothing from. That is what
// makes it affordable: the catalogue holds around six thousand museums with
// websites, the heuristics handle most of them, and a model invocation costing
// minutes is spent only on the residue — once per site, not once per run.
type ExhibitionFallback struct {
	// Harvester runs and heals the extractors.
	Harvester *Harvester
	// Store holds the sources and artifacts.
	Store *Store

	// Every is the cadence recorded on a source the fallback defines, so that
	// once a museum has an extractor the scheduler can keep it working
	// independently of the refresh job that created it.
	Every time.Duration

	// MaxCompiles bounds how many new extractors may be generated in one
	// process lifetime.
	//
	// Without it, the first refresh after this was switched on would try to
	// compile an extractor for every unreadable site in the catalogue, at
	// minutes each, and a nightly batch would never finish. Compiling a few
	// per run means the coverage grows over a fortnight instead, which is the
	// right trade for a job that runs every night anyway.
	MaxCompiles int

	// Now supplies the current time. Nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	compiled int
}

// DefaultMaxCompiles is how many new extractors one run will generate.
const DefaultMaxCompiles = 5

// ErrCompileBudget means this run has generated as many new extractors as it
// is allowed to.
var ErrCompileBudget = errors.New("reached this run's budget for new extractors")

var _ exhibitions.Fallback = (*ExhibitionFallback)(nil)

// ForMuseum extracts a museum's programme with its generated artifact,
// compiling one first if the museum has never been seen.
func (f *ExhibitionFallback) ForMuseum(ctx context.Context, museum models.Museum) ([]exhibitions.Exhibition, error) {
	site := strings.TrimSpace(museum.Website)
	if site == "" {
		return nil, exhibitions.ErrNoWebsite
	}

	name, err := SourceName(site)
	if err != nil {
		return nil, err
	}

	source, err := f.Store.Source(ctx, name)
	switch {
	case errors.Is(err, ErrNoSource):
		source, err = f.define(ctx, name, site)
		if err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	}

	// The stored source must be for this museum's site, not merely for a name
	// derived from it.
	//
	// SourceName folds a host down to a key, and the fold is not injective:
	// foo.example.com and the quite separate foo-example.com both become
	// foo-example-com. Without this check the second museum would harvest the
	// first one's page and asExhibitions would stamp its own name, Wikidata ID
	// and coordinates onto records that came from somewhere else — bad data
	// with no signal that anything went wrong.
	if !sameHost(source.URL, site) {
		return nil, fmt.Errorf("source %s is registered to %s, not to %s: refusing to harvest one museum's site under another's name",
			name, source.URL, site)
	}

	// A quarantined source is one a human has to look at. Running it anyway
	// would spend a fetch to reproduce a failure that is already recorded.
	if source.Paused {
		return nil, fmt.Errorf("source %s is paused: %s", name, source.PausedReason)
	}

	outcome, err := f.Harvester.Once(ctx, source)
	if err != nil {
		return nil, err
	}
	if !outcome.Run.Verdict.Publishable() {
		// Held rather than returned. A suspect or failed extraction is exactly
		// the bad data this whole apparatus exists to keep out of the
		// catalogue, and the museum falls through to its permanent display
		// instead.
		return nil, nil
	}

	return f.asExhibitions(museum, outcome.Records()), nil
}

// define creates a source for a site not seen before and compiles its first
// artifact.
func (f *ExhibitionFallback) define(ctx context.Context, name, site string) (extract.Source, error) {
	if !f.claimCompile() {
		return extract.Source{}, fmt.Errorf("%w: %d already generated", ErrCompileBudget, f.budget())
	}

	every := f.Every
	if every <= 0 {
		every = 24 * time.Hour
	}

	source := extract.Source{
		Name:   name,
		URL:    site,
		Schema: ExhibitionSchema(),
		Every:  extract.Duration(every),
		Expect: extract.Expectation{
			MinRecords: 1,
			// Museum programmes move slowly in number even as they turn over
			// completely in content, but a small museum going from three shows
			// to one is ordinary rather than suspicious. A wide band keeps the
			// volumetric rung catching collapses without crying at every
			// season change.
			Tolerance: 0.75,
		},
		CreatedAt: f.now(),
	}

	if err := f.Store.SaveSource(ctx, source); err != nil {
		return extract.Source{}, err
	}

	log.Printf("harvest: compiling a first extractor for %s (%s)", name, site)
	artifact, report, err := f.Harvester.Compile(ctx, source)

	// Two museums on one domain can reach this together — uniqueBySite folds
	// hosts differently from SourceName, so a duplicate job survives it. Both
	// generate a v1 and the loser is told the version already exists. That is
	// another worker having succeeded, not this site being uncompilable, and
	// pausing the source for it would quarantine a extractor that works.
	if errors.Is(err, ErrArtifactExists) {
		log.Printf("harvest: %s was compiled concurrently, using the stored version", name)
		return source, nil
	}
	if err != nil {
		// The source is left defined even though compiling failed, so that the
		// operator can see it, inspect the attempts, and retry by hand rather
		// than having to rediscover which sites were tried.
		if _, pauseErr := f.Store.Pause(ctx, name, fmt.Sprintf("first compile failed: %v", err)); pauseErr != nil {
			log.Printf("harvest: could not pause %s after a failed compile: %v", name, pauseErr)
		}
		return extract.Source{}, fmt.Errorf("compile %s (%d attempts, page reduced %s): %w",
			name, len(report.Attempts), report.Reduction, err)
	}

	log.Printf("harvest: %s compiled to v%d after %d attempts",
		name, artifact.Version, artifact.Provenance.Attempts)
	return source, nil
}

// claimCompile takes one unit of the compile budget, reporting whether there
// was one to take.
func (f *ExhibitionFallback) claimCompile() bool {
	limit := f.MaxCompiles
	if limit <= 0 {
		limit = DefaultMaxCompiles
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.compiled >= limit {
		return false
	}
	f.compiled++
	return true
}

func (f *ExhibitionFallback) budget() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.compiled
}

// asExhibitions maps generated records onto the catalogue's own type.
//
// The museum's identity, position and scrape time come from the museum record
// rather than from the page, because those are facts the catalogue already
// knows and a model has no business inventing.
func (f *ExhibitionFallback) asExhibitions(museum models.Museum, records []extract.Record) []exhibitions.Exhibition {
	now := f.now()
	out := make([]exhibitions.Exhibition, 0, len(records))

	for _, record := range records {
		title, _ := record.String("title")
		link, _ := record.String("url")
		if title == "" || link == "" {
			continue
		}

		permanent, _ := record["permanent"].(bool)
		start := recordDate(record, "start")
		end := recordDate(record, "end")

		// A permanent display carries no dates by definition: it has none
		// because it has no end, not because the listing failed to give them.
		// Keeping an end date on one would contradict the flag, and the
		// catalogue's own quality checks report exactly that as an error.
		if permanent {
			start, end = nil, nil
		}

		out = append(out, exhibitions.Exhibition{
			Title:            title,
			URL:              link,
			Museum:           museum.Name,
			Start:            start,
			End:              end,
			Running:          permanent || running(start, end, now),
			Upcoming:         start != nil && start.After(now),
			Permanent:        permanent,
			SourcePage:       museum.Website,
			MuseumWikidataID: museum.WikidataID,
			Latitude:         museum.Latitude,
			Longitude:        museum.Longitude,
			ScrapedAt:        now,
		})
	}
	return out
}

// running reports whether a date range covers now, treating a missing bound as
// open at that end.
func running(start, end *time.Time, now time.Time) bool {
	if start != nil && start.After(now) {
		return false
	}
	if end != nil && end.Before(now) {
		return false
	}
	return start != nil || end != nil
}

// recordDate reads an ISO date out of a record.
//
// Only ISO layouts are accepted, because that is what the generated artifact
// was told to emit and what the validator already checked. A record reaching
// here with "15 January" in it would have failed validation, so there is
// nothing to be lenient about.
func recordDate(record extract.Record, field string) *time.Time {
	text, ok := record.String(field)
	if !ok || strings.TrimSpace(text) == "" {
		return nil
	}

	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02", "2006-01"} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(text)); err == nil {
			return &parsed
		}
	}
	return nil
}

func (f *ExhibitionFallback) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now().UTC()
}

// sameHost reports whether two website references name the same host,
// comparing the way SourceName folds them.
func sameHost(a, b string) bool {
	nameA, errA := SourceName(a)
	nameB, errB := SourceName(b)
	if errA != nil || errB != nil {
		return false
	}
	if nameA != nameB {
		return false
	}

	// Same folded name is not enough — that is exactly the collision being
	// guarded against. Compare the hosts themselves.
	hostA, errA := hostOf(a)
	hostB, errB := hostOf(b)
	return errA == nil && errB == nil && hostA == hostB
}

// hostOf returns a website's lowercase host, without "www.".
func hostOf(website string) (string, error) {
	trimmed := strings.TrimSpace(website)
	if !strings.Contains(trimmed, "//") {
		trimmed = "https://" + trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("museum website %q is not a usable URL", website)
	}
	return strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www."), nil
}

// SourceName derives a stable source name from a website.
//
// It is the host, without "www.", with dots turned into dashes: one extractor
// per site rather than per museum. Tate publishes four galleries on one
// domain, and they share a listing page, so compiling four identical
// extractors would spend four model invocations to produce the same script.
func SourceName(website string) (string, error) {
	trimmed := strings.TrimSpace(website)
	if !strings.Contains(trimmed, "//") {
		trimmed = "https://" + trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("museum website %q is not a usable URL", website)
	}

	host := strings.ToLower(parsed.Hostname())
	host = strings.TrimPrefix(host, "www.")
	if host == "" {
		return "", fmt.Errorf("museum website %q has no host", website)
	}
	return strings.ReplaceAll(host, ".", "-"), nil
}
