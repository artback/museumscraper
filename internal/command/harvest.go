package command

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"museum/internal/env"
	"museum/internal/harvest"
	"museum/pkg/exhibitions"
	"museum/pkg/extract"
	"museum/pkg/graceful"
	"museum/pkg/model"
)

// harvestCommand is the operator surface for the self-healing extraction
// harness.
//
// It takes a verb rather than a pile of mutually exclusive flags because the
// lifecycle genuinely has stages — define, inspect, run, repair, roll back —
// and a single command with an -add, -show, -heal and -rollback flag would
// have to reject most combinations of them at runtime.
func harvestCommand() Command {
	return Command{
		Name:    "harvest",
		Summary: "Define, run and repair generated web extractors",
		Usage:   "<add|list|show|run|heal|rollback|pause|resume|serve> [flags]",
		Run:     runHarvest,
	}
}

// harvestVerbs is the dispatch table, in the order help lists them.
var harvestVerbs = []struct {
	name    string
	summary string
	usage   string
	run     func(ctx context.Context, args []string) error
}{
	{"add", "Define a source and generate its first extractor", "-file SOURCE.json [-yes]", harvestAdd},
	{"list", "List every source with its state", "", harvestList},
	{"show", "Show a source's extractor, recent runs and heal history", "-source NAME [-script]", harvestShow},
	{"run", "Run one source now and print the result without publishing", "-source NAME [-publish] [-json]", harvestRun},
	{"heal", "Force regeneration of a source's extractor", "-source NAME [-yes]", harvestHeal},
	{"rollback", "Restore an earlier version of an extractor", "-source NAME -to N", harvestRollback},
	{"pause", "Stop the scheduler running a source", "-source NAME [-reason TEXT]", harvestPause},
	{"resume", "Undo a pause or a quarantine", "-source NAME", harvestResume},
	{"serve", "Run every source on its own schedule", "[-concurrency 4] [-tick 1m]", harvestServe},
}

func runHarvest(ctx context.Context, args []string) error {
	if len(args) == 0 {
		harvestUsage(os.Stderr)
		return errors.New("harvest needs a verb")
	}

	name := args[0]
	if name == "-h" || name == "--help" || name == "help" {
		harvestUsage(os.Stdout)
		return nil
	}

	for _, verb := range harvestVerbs {
		if verb.name == name {
			return verb.run(ctx, args[1:])
		}
	}

	harvestUsage(os.Stderr)
	return fmt.Errorf("unknown harvest verb %q", name)
}

func harvestUsage(w io.Writer) {
	fmt.Fprintf(w, "Usage:\n  museum harvest <verb> [flags]\n\nVerbs:\n")

	width := 0
	for _, verb := range harvestVerbs {
		width = max(width, len(verb.name))
	}
	for _, verb := range harvestVerbs {
		fmt.Fprintf(w, "  %-*s  %s\n", width, verb.name, verb.summary)
		if verb.usage != "" {
			fmt.Fprintf(w, "  %-*s    %s\n", width, "", verb.usage)
		}
	}
	fmt.Fprintf(w, "\nRun \"museum harvest <verb> -h\" for the flags of one verb.\n")
}

// harvestBucket is where the harness keeps its state.
//
// It is deliberately a different bucket from the catalogue's, not just a
// different prefix. The enricher geocodes from bucket notifications and does
// not check whether a record has already been enriched, so writing thousands
// of artifacts and run records into the bucket that has the notification
// attached would queue Nominatim calls for objects that are not museums at
// all. Separate buckets make that impossible rather than merely unlikely.
func harvestBucket() (string, error) {
	env.LoadEnv()

	if bucket := strings.TrimSpace(os.Getenv("HARVEST_BUCKET_NAME")); bucket != "" {
		return bucket, nil
	}

	base, err := env.LookupEnv("MUSEUM_BUCKET_NAME")
	if err != nil {
		return "", fmt.Errorf("set HARVEST_BUCKET_NAME, or %w", err)
	}
	return base + "-harvest", nil
}

// openHarvestStore connects to the harness's own bucket, creating it if this
// is the first run.
func openHarvestStore(ctx context.Context) (*harvest.Store, error) {
	bucket, err := harvestBucket()
	if err != nil {
		return nil, err
	}

	store, err := harvest.OpenStore(bucket)
	if err != nil {
		return nil, err
	}
	if err := store.EnsureBucket(ctx, ""); err != nil {
		return nil, err
	}
	return store, nil
}

// harvester builds a fully wired harvester.
//
// withModel is false for the verbs that only execute stored artifacts, so that
// running or inspecting a source works on a deployment with no inference
// server at all.
func harvester(ctx context.Context, withModel bool) (*harvest.Harvester, *harvest.Store, error) {
	store, err := openHarvestStore(ctx)
	if err != nil {
		return nil, nil, err
	}

	h := &harvest.Harvester{
		Store: store,
		Fetch: exhibitions.NewFetcher(),
		Sink:  harvest.NewStoreSink(store),
	}

	if webhook := strings.TrimSpace(os.Getenv("HARVEST_WEBHOOK_URL")); webhook != "" {
		h.Sink = harvest.Sinks{h.Sink, harvest.NewWebhookSink(webhook)}
	}

	if withModel {
		client, err := model.New()
		if err != nil {
			return nil, nil, err
		}
		// The harvester aligns the generator's trial sandbox with its own, so
		// the trial proves something about production.
		h.Generator = &extract.Generator{Model: client}

		// The model-judged rung is off unless asked for. It is the only thing
		// that can cost a model invocation on a run that is otherwise passing,
		// which is the guarantee the whole design is built around, so turning
		// it on is a deliberate act.
		if os.Getenv("HARVEST_MODEL_JUDGE") == "true" {
			h.Validator = &extract.Validator{Judge: extract.NewJudge(client)}
		}
	}
	return h, store, nil
}

func harvestAdd(ctx context.Context, args []string) error {
	fs := newFlagSet("harvest add", "-file SOURCE.json [-yes]", os.Stderr)
	var (
		file    = fs.String("file", "", "JSON file defining the source and its schema")
		every   = fs.Duration("every", 0, "override the source's schedule, e.g. 24h")
		confirm = fs.Bool("yes", false, "store the extractor without asking")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("harvest add needs -file")
	}

	source, err := readSourceFile(*file)
	if err != nil {
		return err
	}
	if *every > 0 {
		source.Every = extract.Duration(*every)
	}
	if err := source.Validate(); err != nil {
		return err
	}

	h, store, err := harvester(ctx, true)
	if err != nil {
		return err
	}

	// A source that exists but has no artifact is not a conflict, it is an
	// unfinished definition — which is the state a failed first compile leaves
	// behind, deliberately, so the operator can see what was tried. Refusing it
	// here made that state unreachable by every verb: add refused, heal and run
	// both need an artifact, and rollback needs a version. The only way out was
	// deleting the object by hand.
	switch existing, err := store.Source(ctx, source.Name); {
	case err == nil:
		if _, err := store.CurrentArtifact(ctx, source.Name); err == nil {
			return fmt.Errorf("source %q already exists (%s) and has an extractor; use harvest heal to regenerate it",
				existing.Name, existing.URL)
		}
		fmt.Printf("%s is already defined but has no extractor%s. Generating one.\n\n",
			existing.Name, pausedNote(existing))
	case !errors.Is(err, harvest.ErrNoSource):
		return err
	}

	fmt.Printf("Reading %s and generating an extractor. This is the slow part.\n\n", source.URL)

	// Generated first, shown, and only then committed. An operator asked to
	// approve an artifact they have not seen is not reviewing anything, so
	// this drafts rather than compiles: nothing is written until the answer.
	artifact, report, err := h.Draft(ctx, source)
	if err != nil {
		printAttempts(report)
		return err
	}
	printAttempts(report)

	fmt.Printf("Generated v%d for %s using %s, %d attempt(s):\n\n",
		artifact.Version, source.Name, artifact.Provenance.Model, artifact.Provenance.Attempts)
	fmt.Println(indent(artifact.Script))
	fmt.Println()

	if !*confirm && !ask("Keep this extractor?") {
		return errors.New("abandoned; nothing was stored")
	}

	// Stored unpaused: a first compile that failed pauses the source, and
	// succeeding now is what lifts that.
	source.Paused, source.PausedReason = false, ""
	if err := store.SaveSource(ctx, source); err != nil {
		return err
	}
	if err := store.SaveArtifact(ctx, artifact); err != nil {
		return err
	}
	fmt.Printf("\nStored %s at v%d. Run it with:\n  museum harvest run -source %s\n",
		source.Name, artifact.Version, source.Name)
	return nil
}

// pausedNote explains a paused source in a sentence fragment.
func pausedNote(source extract.Source) string {
	if !source.Paused {
		return ""
	}
	return " (paused: " + source.PausedReason + ")"
}

// readSourceFile loads a source definition.
func readSourceFile(path string) (extract.Source, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return extract.Source{}, fmt.Errorf("read %s: %w", path, err)
	}

	var source extract.Source
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&source); err != nil {
		// Strict, because a misspelled field in a schema is silently ignored
		// otherwise and the rule the operator thought they had written is
		// simply absent from every check.
		return extract.Source{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if source.CreatedAt.IsZero() {
		source.CreatedAt = time.Now().UTC()
	}
	return source, nil
}

func harvestList(ctx context.Context, args []string) error {
	fs := newFlagSet("harvest list", "", os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, err := openHarvestStore(ctx)
	if err != nil {
		return err
	}
	sources, err := store.Sources(ctx)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		fmt.Println("No sources defined. Add one with: museum harvest add -file SOURCE.json")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SOURCE\tVERSION\tEVERY\tLAST RUN\tVERDICT\tRECORDS\tSTATE")

	for _, source := range sources {
		version, verdict, records, when := "-", "-", "-", "never"

		if artifact, err := store.CurrentArtifact(ctx, source.Name); err == nil {
			version = fmt.Sprintf("v%d", artifact.Version)
		}
		if runs, err := store.Runs(ctx, source.Name, 1); err == nil && len(runs) > 0 {
			verdict = string(runs[0].Verdict)
			records = fmt.Sprint(runs[0].Records)
			when = runs[0].At.Format(time.RFC3339)
		}

		state := "active"
		if source.Paused {
			state = "paused: " + source.PausedReason
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			source.Name, version, source.Every, when, verdict, records, state)
	}
	return w.Flush()
}

func harvestShow(ctx context.Context, args []string) error {
	fs := newFlagSet("harvest show", "-source NAME [-script] [-runs 10]", os.Stderr)
	var (
		name       = fs.String("source", "", "the source to inspect")
		showScript = fs.Bool("script", false, "print the current extractor's source")
		limit      = fs.Int("runs", 10, "how many recent runs to show")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("harvest show needs -source")
	}

	store, err := openHarvestStore(ctx)
	if err != nil {
		return err
	}
	source, err := store.Source(ctx, *name)
	if err != nil {
		return err
	}

	fmt.Printf("%s\n  url:    %s\n  schema: %s, %d fields\n  every:  %s\n",
		source.Name, source.URL, source.Schema.Name, len(source.Schema.Fields), source.Every)
	if source.Paused {
		fmt.Printf("  PAUSED: %s\n", source.PausedReason)
	}

	artifacts, err := store.Artifacts(ctx, *name)
	if err != nil {
		return err
	}
	if len(artifacts) == 0 {
		fmt.Println("\nNo extractor has been generated yet.")
		return nil
	}

	current := artifacts[len(artifacts)-1]
	fmt.Printf("\nExtractor v%d, generated %s by %s (prompt %s), %d attempt(s)\n",
		current.Version, current.CreatedAt.Format(time.RFC3339),
		current.Provenance.Model, current.Provenance.Prompt, current.Provenance.Attempts)

	// The heal history is the diffable record the PRD asks for: every version,
	// what it came from, and why it was regenerated.
	if len(artifacts) > 1 {
		fmt.Println("\nVersions:")
		for _, artifact := range artifacts {
			reason := artifact.Reason
			if reason == "" {
				reason = "first generation"
			}
			fmt.Printf("  v%-3d %s  %s\n", artifact.Version,
				artifact.CreatedAt.Format("2006-01-02 15:04"), reason)
		}
	}

	runs, err := store.Runs(ctx, *name, *limit)
	if err != nil {
		return err
	}
	if len(runs) > 0 {
		fmt.Printf("\nLast %d run(s):\n", len(runs))
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  WHEN\tVERDICT\tRECORDS\tVERSION\tDRIFT\tNOTE")
		for _, run := range runs {
			drift := ""
			if run.Drifted {
				drift = "drifted"
			}
			note := run.Err
			if note == "" && len(run.Findings) > 0 {
				note = run.Findings[0]
			}
			fmt.Fprintf(w, "  %s\t%s\t%d\tv%d\t%s\t%s\n",
				run.At.Format("2006-01-02 15:04"), run.Verdict, run.Records,
				run.Version, drift, truncate(note, 60))
		}
		w.Flush()
	}

	if *showScript {
		fmt.Printf("\nv%d:\n\n%s\n", current.Version, indent(current.Script))
	}
	return nil
}

func harvestRun(ctx context.Context, args []string) error {
	fs := newFlagSet("harvest run", "-source NAME [-publish] [-json]", os.Stderr)
	var (
		name    = fs.String("source", "", "the source to run")
		publish = fs.Bool("publish", false, "deliver the result to the sinks")
		asJSON  = fs.Bool("json", false, "print the records as JSON")
		allow   = fs.Bool("heal", false, "allow this run to regenerate a broken extractor")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("harvest run needs -source")
	}

	h, store, err := harvester(ctx, *allow)
	if err != nil {
		return err
	}
	// An ad hoc run prints and does not publish unless asked, which is what
	// makes it safe to use for checking a suspicion about a source.
	if !*publish {
		h.Sink = nil
	}

	source, err := store.Source(ctx, *name)
	if err != nil {
		return err
	}

	outcome, err := h.Once(ctx, source)
	if err != nil {
		return err
	}

	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(outcome.Records()); err != nil {
			return err
		}
	}

	fmt.Printf("\n%s: %s, %d records, v%d, %s\n", source.Name, outcome.Run.Verdict,
		outcome.Run.Records, outcome.Run.Version, outcome.Run.Duration.Round(time.Millisecond))
	if outcome.Run.Drifted {
		fmt.Println("  the page's structure has changed since this extractor was written")
	}
	for _, finding := range outcome.Run.Findings {
		fmt.Printf("  - %s\n", finding)
	}
	if outcome.Healed != nil {
		fmt.Printf("  healed to v%d\n", outcome.Healed.Version)
	}
	if outcome.Quarantined {
		fmt.Printf("  QUARANTINED: %s\n", outcome.Alert)
	}
	if *publish && outcome.Published {
		fmt.Println("  published")
	}

	if !*asJSON && len(outcome.Records()) > 0 {
		fmt.Println("\nFirst few records:")
		for i, record := range outcome.Records() {
			if i >= 5 {
				fmt.Printf("  … and %d more\n", len(outcome.Records())-i)
				break
			}
			fmt.Printf("  %v\n", record)
		}
	}
	return nil
}

func harvestHeal(ctx context.Context, args []string) error {
	fs := newFlagSet("harvest heal", "-source NAME [-yes]", os.Stderr)
	var (
		name    = fs.String("source", "", "the source to regenerate")
		confirm = fs.Bool("yes", false, "store the new extractor without asking")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("harvest heal needs -source")
	}

	h, store, err := harvester(ctx, true)
	if err != nil {
		return err
	}
	source, err := store.Source(ctx, *name)
	if err != nil {
		return err
	}
	previous, err := store.CurrentArtifact(ctx, *name)
	if err != nil {
		return err
	}

	fmt.Printf("Regenerating %s from %s.\n\n", source.Name, source.URL)

	artifact, report, err := h.Regenerate(ctx, source, previous, "forced by the operator")
	if err != nil {
		printAttempts(report)
		return err
	}
	printAttempts(report)

	fmt.Printf("v%d → v%d:\n\n%s\n\n", previous.Version, artifact.Version, indent(artifact.Script))

	if !*confirm && !ask("Replace the current extractor with this?") {
		return errors.New("abandoned; the previous version is still current")
	}
	if err := store.SaveArtifact(ctx, artifact); err != nil {
		return err
	}

	fmt.Printf("\n%s is now at v%d. Roll back with:\n  museum harvest rollback -source %s -to %d\n",
		source.Name, artifact.Version, source.Name, previous.Version)
	return nil
}

func harvestRollback(ctx context.Context, args []string) error {
	fs := newFlagSet("harvest rollback", "-source NAME -to N", os.Stderr)
	var (
		name = fs.String("source", "", "the source to roll back")
		to   = fs.Int("to", 0, "the version to restore")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *to < 1 {
		return errors.New("harvest rollback needs -source and -to")
	}

	store, err := openHarvestStore(ctx)
	if err != nil {
		return err
	}

	restored, err := store.Rollback(ctx, *name, *to)
	if err != nil {
		return err
	}
	fmt.Printf("%s restored v%d as v%d.\n", *name, *to, restored.Version)
	return nil
}

func harvestPause(ctx context.Context, args []string) error {
	fs := newFlagSet("harvest pause", "-source NAME [-reason TEXT]", os.Stderr)
	var (
		name   = fs.String("source", "", "the source to pause")
		reason = fs.String("reason", "paused by the operator", "why")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("harvest pause needs -source")
	}

	store, err := openHarvestStore(ctx)
	if err != nil {
		return err
	}
	if _, err := store.Pause(ctx, *name, *reason); err != nil {
		return err
	}
	fmt.Printf("%s paused: %s\n", *name, *reason)
	return nil
}

func harvestResume(ctx context.Context, args []string) error {
	fs := newFlagSet("harvest resume", "-source NAME", os.Stderr)
	name := fs.String("source", "", "the source to resume")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("harvest resume needs -source")
	}

	store, err := openHarvestStore(ctx)
	if err != nil {
		return err
	}
	if _, err := store.Resume(ctx, *name); err != nil {
		return err
	}
	fmt.Printf("%s resumed.\n", *name)
	return nil
}

func harvestServe(ctx context.Context, args []string) error {
	fs := newFlagSet("harvest serve", "[-concurrency 4] [-tick 1m]", os.Stderr)
	var (
		concurrency = fs.Int("concurrency", harvest.DefaultConcurrency, "sources to run at once")
		tick        = fs.Duration("tick", harvest.DefaultTick, "how often to look for due sources")
		jitter      = fs.Duration("jitter", harvest.DefaultJitter, "maximum delay added before a run")
		noHeal      = fs.Bool("no-heal", false, "execute and grade but never regenerate")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := graceful.Context(ctx)
	defer cancel()

	h, store, err := harvester(ctx, !*noHeal)
	if err != nil {
		return err
	}

	scheduler := &harvest.Scheduler{
		Harvester:   h,
		Store:       store,
		Tick:        *tick,
		Concurrency: *concurrency,
		Jitter:      *jitter,
	}

	// A cancelled context is how this is meant to stop, so it is not an error.
	if err := scheduler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// printAttempts reports what generation tried, so a failure explains itself.
func printAttempts(report extract.Report) {
	if report.Reduction.OriginalBytes > 0 {
		fmt.Printf("Page reduced: %s\n", report.Reduction)
	}
	for _, attempt := range report.Attempts {
		if attempt.Problem == "" {
			fmt.Printf("  attempt %d: accepted, %d records\n", attempt.Number, attempt.Records)
			continue
		}
		fmt.Printf("  attempt %d: %s\n", attempt.Number, attempt.Problem)
		for _, finding := range attempt.Findings {
			fmt.Printf("      %s\n", finding)
		}
	}
}

// ask puts a yes-or-no question to the operator.
//
// A non-interactive run — a container, a scheduler — has no one to answer, so
// an unreadable stdin is a no rather than a hang.
func ask(question string) bool {
	fmt.Printf("%s [y/N] ", question)

	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fmt.Println()
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// indent shifts a script right so it reads as a quoted block.
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "    " + line
	}
	return strings.Join(lines, "\n")
}

// exhibitionFallback builds the generated-extractor fallback the refresh job
// consults for museums its heuristics could read nothing from.
//
// It needs a model, because meeting a site for the first time means compiling
// an extractor for it. A deployment without one should run refresh without
// -fallback rather than with a fallback that cannot do anything.
func exhibitionFallback(ctx context.Context, maxCompiles int) (*harvest.ExhibitionFallback, error) {
	h, store, err := harvester(ctx, true)
	if err != nil {
		return nil, err
	}

	return &harvest.ExhibitionFallback{
		Harvester:   h,
		Store:       store,
		MaxCompiles: maxCompiles,
	}, nil
}
