package harvest

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/artback/museumscraper/extract"
)

// Fetcher retrieves a page as a browser would see it.
//
// The interface is declared here, where it is used, rather than beside an
// implementation. That is what lets the harness borrow the catalogue's
// existing polite fetcher — the one that already honours robots.txt, spaces
// requests one per host per second, follows redirects, caps bodies and sorts
// out character encodings — without this package depending on the exhibitions
// scraper it lives beside.
type Fetcher interface {
	// Get returns a page's body and the URL it ended up at after redirects.
	Get(ctx context.Context, url string) (body string, finalURL string, err error)
}

// Archive is the persistent state the loop reads and writes, which *Store
// implements against object storage.
//
// The loop takes an interface rather than the concrete store because it is the
// most delicate logic in the package — when to heal, when to quarantine, what
// a fetch failure means as against an extraction failure — and every one of
// those decisions has to be exercised against histories that would be tedious
// and slow to construct in a real bucket.
type Archive interface {
	Sources(ctx context.Context) ([]extract.Source, error)
	LastRunAt(ctx context.Context, source string) (time.Time, bool, error)
	CurrentArtifact(ctx context.Context, source string) (extract.Artifact, error)
	SaveArtifact(ctx context.Context, artifact extract.Artifact) error
	AppendRun(ctx context.Context, run extract.Run) error
	Runs(ctx context.Context, source string, limit int) ([]extract.Run, error)
	History(ctx context.Context, source string) (extract.History, error)
	PruneRuns(ctx context.Context, source string) (int, error)
	Pause(ctx context.Context, name, reason string) (extract.Source, error)
}

// *Store is the production Archive. The check is here because nothing else
// converts one to the other statically: the CLI assigns a *Store to an Archive
// field, which would fail at the assignment rather than at the definition.
var _ Archive = (*Store)(nil)

// Harvester runs one source through the loop: fetch, execute, validate, and
// heal if the evidence justifies it.
type Harvester struct {
	// Store persists everything.
	Store Archive
	// Fetch retrieves pages.
	Fetch Fetcher

	// Sandbox executes artifacts. Nil means a default sandbox.
	Sandbox *extract.Sandbox
	// Validator grades output. Nil means a default validator with no judge.
	Validator *extract.Validator
	// Generator compiles and heals artifacts. Nil disables both, which is a
	// legitimate configuration: a runner deployed without a model still
	// executes stored artifacts and still grades them, it just cannot repair
	// them.
	Generator *extract.Generator
	// Policy caps how often a source may be healed.
	Policy extract.HealPolicy
	// Sink delivers passing output. Nil publishes nowhere.
	Sink Sink

	// Now supplies the current time. Nil means time.Now.
	Now func() time.Time
}

// Outcome is everything one run of one source did, in the detail an operator
// needs to understand it without reading the store.
type Outcome struct {
	// Run is the record that was appended to history.
	Run extract.Run
	// Assessment is the verdict and its reasoning.
	Assessment extract.Assessment
	// Artifact is the version that executed.
	Artifact extract.Artifact

	// Healed is the replacement artifact, when this run produced one.
	Healed *extract.Artifact
	// Report is the generation report from a heal, when one happened.
	Report *extract.Report

	// Published reports whether the output reached the sink.
	Published bool
	// Quarantined reports that the source was paused, and Alert says why. A
	// quarantined source is one that needs a human.
	Quarantined bool
	Alert       string
}

// Records are the extracted records, which is what a caller running a source
// ad hoc wants back.
func (o Outcome) Records() []extract.Record { return o.Assessment.Records }

// ErrNoGenerator means a heal or a compile was needed and no model is
// configured.
var ErrNoGenerator = errors.New("no generator configured")

// generator returns the generator with its trial sandbox aligned to the one
// the runner will use.
//
// An artifact is trialled by the generator and executed by the runner, and if
// those two sandboxes differ the trial proves nothing about production. The
// dangerous direction is a trial that is more generous: a script written
// against a standard library the runner does not install passes its trial and
// then throws on every real run, which reads as a broken source and spends a
// heal to rediscover the same script.
func (h *Harvester) generator() *extract.Generator {
	if h.Generator == nil || h.Generator.Sandbox != nil {
		return h.Generator
	}

	// Copied rather than filled in. One Harvester is shared across the refresh
	// scraper's workers — eight of them by default — so writing the alignment
	// back onto the caller's Generator is a data race on a struct several
	// goroutines are reading.
	aligned := *h.Generator
	aligned.Sandbox = h.sandbox()
	return &aligned
}

// Draft generates a source's first artifact without storing it.
//
// The artifact has already been run against the page that produced it and has
// passed validation before this returns — that check is not what Draft
// withholds. What it withholds is the write, so that an interactive caller can
// show an operator what was generated and get an answer before anything is
// committed.
func (h *Harvester) Draft(ctx context.Context, source extract.Source) (extract.Artifact, extract.Report, error) {
	if h.Generator == nil {
		return extract.Artifact{}, extract.Report{}, ErrNoGenerator
	}

	page, err := h.fetch(ctx, source.URL)
	if err != nil {
		return extract.Artifact{}, extract.Report{}, err
	}
	return h.generator().Generate(ctx, source, page)
}

// Compile generates a source's first artifact and stores it.
//
// This is the unattended path, used where there is no operator to ask — the
// exhibitions fallback meeting a site it has never seen. Interactive callers
// want Draft followed by SaveArtifact.
func (h *Harvester) Compile(ctx context.Context, source extract.Source) (extract.Artifact, extract.Report, error) {
	artifact, report, err := h.Draft(ctx, source)
	if err != nil {
		return extract.Artifact{}, report, err
	}
	if err := h.Store.SaveArtifact(ctx, artifact); err != nil {
		return extract.Artifact{}, report, err
	}
	return artifact, report, nil
}

// Regenerate produces a replacement artifact without storing it.
//
// The operator-facing heal is deliberately two steps: this returns a candidate
// that has passed its trial, and storing it is a separate call the CLI makes
// only after showing the operator what changed. An artifact approved sight
// unseen is not an artifact anyone reviewed.
func (h *Harvester) Regenerate(ctx context.Context, source extract.Source, previous extract.Artifact, reason string) (extract.Artifact, extract.Report, error) {
	if h.Generator == nil {
		return extract.Artifact{}, extract.Report{}, ErrNoGenerator
	}

	page, err := h.fetch(ctx, source.URL)
	if err != nil {
		return extract.Artifact{}, extract.Report{}, err
	}
	return h.generator().Heal(ctx, source, page, previous, reason)
}

// Once runs a source through the loop exactly once.
//
// At most one heal happens per call, which is where the PRD's "one healing
// attempt per run" is enforced: there is no loop here to go round twice.
func (h *Harvester) Once(ctx context.Context, source extract.Source) (Outcome, error) {
	started := h.now()

	artifact, err := h.Store.CurrentArtifact(ctx, source.Name)
	if err != nil {
		return Outcome{}, err
	}

	outcome := Outcome{Artifact: artifact}
	run := extract.Run{
		Source:  source.Name,
		At:      started,
		Version: artifact.Version,
	}

	// A page that could not be fetched is not an artifact that stopped
	// working. Grading it as a failure would authorise a heal, and a model
	// invocation is a wasteful response to a timeout.
	page, err := h.fetch(ctx, source.URL)
	if err != nil {
		run.Verdict = extract.Fail
		run.Err = err.Error()
		run.Duration = h.now().Sub(started)
		outcome.Run = run
		outcome.Assessment = extract.Assessment{
			Verdict:  extract.Fail,
			Findings: []string{"could not fetch the page: " + err.Error()},
		}
		return outcome, h.record(ctx, run)
	}

	// History is read once and threaded through. It was being read again inside
	// every grading and once more before healing, which on the store is a full
	// listing each time.
	history := h.history(ctx, source.Name)

	run.Fingerprint = extract.Fingerprint(page)
	run.Drifted = extract.Drifted(h.baseline(artifact, history), run.Fingerprint)

	assessment := h.execute(ctx, source, artifact.Script, page, history)
	run.Verdict = assessment.Verdict
	run.Records = len(assessment.Records)
	run.Findings = assessment.Findings

	// The drift signal decides how a bad grade is read, which is the whole
	// reason for computing it.
	heal, why := extract.ShouldHeal(assessment, run.Drifted)
	if heal {
		assessment, run, outcome = h.heal(ctx, source, artifact, page, run, outcome, why, history)
	}

	outcome.Assessment = assessment
	run.Duration = h.now().Sub(started)

	if assessment.Publishable() {
		if err := h.publish(ctx, source, run, assessment.Records); err != nil {
			// Failing to publish is not failing to extract. The run keeps its
			// verdict and the delivery failure is reported separately, so a
			// broken sink cannot make a healthy source look broken.
			log.Printf("harvest: %s extracted %d records but could not publish them: %v",
				source.Name, len(assessment.Records), err)
		} else {
			outcome.Published = true
		}
	}

	outcome.Run = run
	return outcome, h.record(ctx, run)
}

// heal regenerates an artifact and re-validates against the same page.
func (h *Harvester) heal(
	ctx context.Context,
	source extract.Source,
	broken extract.Artifact,
	page *extract.Page,
	run extract.Run,
	outcome Outcome,
	why string,
	history extract.History,
) (extract.Assessment, extract.Run, Outcome) {
	assessment := extract.Assessment{Verdict: run.Verdict, Findings: run.Findings}

	if h.Generator == nil {
		return assessment, run, outcome
	}

	// The cap that stops a permanently dead source spending a model invocation
	// on every schedule tick.
	recent, err := h.Store.Runs(ctx, source.Name, RunRetention)
	if err != nil {
		log.Printf("harvest: %s could not read run history, not healing: %v", source.Name, err)
		return assessment, run, outcome
	}
	if escalate, reason := h.Policy.Escalate(recent, h.now()); escalate {
		outcome = h.quarantine(ctx, source, outcome, reason)
		return assessment, run, outcome
	}

	log.Printf("harvest: healing %s from v%d — %s", source.Name, broken.Version, why)

	healed, report, err := h.generator().Heal(ctx, source, page, broken, why)
	outcome.Report = &report
	if err != nil {
		outcome = h.quarantine(ctx, source, outcome,
			fmt.Sprintf("healing failed after %d attempts: %v", len(report.Attempts), err))
		return assessment, run, outcome
	}

	// The healed artifact passed its trial inside Heal. It is re-run here
	// against this source's real history, because the trial is graded with no
	// baseline and a replacement that extracts three records where two hundred
	// are expected has not fixed anything.
	// Only a replacement that still cannot extract is grounds for quarantine.
	//
	// A heal that comes back suspect has usually done its job: the extractor
	// reads the page again and the count is simply out of character, which is a
	// fact about the museum rather than a defect in the script. A museum with
	// four shows on instead of its usual twenty grades suspect however
	// perfectly it is extracted, and quarantining for that would take a working
	// source offline for having a quiet month. Suspect output is still withheld
	// — it is not publishable — so nothing wrong escapes either way.
	revalidated := h.execute(ctx, source, healed.Script, page, history)
	if revalidated.Verdict == extract.Fail {
		outcome = h.quarantine(ctx, source, outcome, fmt.Sprintf(
			"the healed artifact still fails: %s",
			firstOr(revalidated.Findings, "no reason recorded")))
		return assessment, run, outcome
	}

	if err := h.Store.SaveArtifact(ctx, healed); err != nil {
		log.Printf("harvest: %s healed but the new artifact could not be stored: %v", source.Name, err)
		return assessment, run, outcome
	}

	outcome.Healed = &healed
	run.Healed, run.HealedTo, run.Version = true, healed.Version, healed.Version
	run.Verdict = revalidated.Verdict
	run.Records = len(revalidated.Records)
	run.Findings = append(run.Findings, fmt.Sprintf("healed to v%d", healed.Version))

	log.Printf("harvest: %s healed to v%d, now extracting %d records",
		source.Name, healed.Version, len(revalidated.Records))
	return revalidated, run, outcome
}

// quarantine pauses a source and records why, so it stops costing anything
// until a human has looked at it.
func (h *Harvester) quarantine(ctx context.Context, source extract.Source, outcome Outcome, reason string) Outcome {
	outcome.Quarantined = true
	outcome.Alert = reason

	if _, err := h.Store.Pause(ctx, source.Name, reason); err != nil {
		log.Printf("harvest: %s should be quarantined but could not be paused: %v", source.Name, err)
	}
	log.Printf("harvest: QUARANTINED %s — %s", source.Name, reason)
	return outcome
}

// baseline is the fingerprint this run's page is compared against.
//
// The last passing run's signature is preferred over the artifact's. An
// artifact records the page it was written from and is then immutable, so a
// site that changes cosmetically once would otherwise report drift on every
// run for the rest of its life.
func (h *Harvester) baseline(artifact extract.Artifact, history extract.History) string {
	if history.Fingerprint != "" {
		return history.Fingerprint
	}
	return artifact.Fingerprint
}

// history reads the source's baseline, degrading to an empty one.
func (h *Harvester) history(ctx context.Context, source string) extract.History {
	history, err := h.Store.History(ctx, source)
	if err != nil {
		log.Printf("harvest: %s could not read history, grading without a baseline: %v", source, err)
		return extract.History{}
	}
	return history
}

// execute runs a script and grades what it produced.
func (h *Harvester) execute(ctx context.Context, source extract.Source, script string, page *extract.Page, history extract.History) extract.Assessment {
	output, err := h.sandbox().Run(ctx, script, page)
	if err != nil {
		// A script that would not compile, threw, or ran away is a definite
		// failure rather than an ungraded one, and is exactly what healing is
		// for.
		return extract.Assessment{
			Verdict:  extract.Fail,
			Findings: []string{"artifact did not run: " + err.Error()},
		}
	}

	return h.validator().Validate(ctx, source, output.Records, history)
}

// record appends the run and prunes history that has aged out.
func (h *Harvester) record(ctx context.Context, run extract.Run) error {
	if err := h.Store.AppendRun(ctx, run); err != nil {
		return err
	}
	if _, err := h.Store.PruneRuns(ctx, run.Source); err != nil {
		// Pruning is housekeeping; failing at it must not fail the run that
		// has already happened.
		log.Printf("harvest: could not prune %s run history: %v", run.Source, err)
	}
	return nil
}

func (h *Harvester) publish(ctx context.Context, source extract.Source, run extract.Run, records []extract.Record) error {
	if h.Sink == nil {
		return nil
	}
	return h.Sink.Publish(ctx, source, run, records)
}

func (h *Harvester) fetch(ctx context.Context, url string) (*extract.Page, error) {
	if h.Fetch == nil {
		return nil, errors.New("harvester has no fetcher")
	}

	body, finalURL, err := h.Fetch.Get(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	if finalURL == "" {
		finalURL = url
	}
	return extract.ParsePage(finalURL, body)
}

func (h *Harvester) sandbox() *extract.Sandbox {
	if h.Sandbox != nil {
		return h.Sandbox
	}
	sandbox := extract.NewSandbox()
	sandbox.Library = ExhibitionLibrary()
	return sandbox
}

func (h *Harvester) validator() *extract.Validator {
	if h.Validator != nil {
		return h.Validator
	}
	return &extract.Validator{}
}

func (h *Harvester) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now().UTC()
}

func firstOr(values []string, fallback string) string {
	if len(values) == 0 {
		return fallback
	}
	return values[0]
}
