// Package harvest runs the self-healing extraction loop over stored sources.
//
// pkg/extract holds the parts that have no opinion about where anything lives:
// how to reduce a page, how to run a script safely, how to grade its output,
// when to heal. This package is where those parts are wired to the object
// storage the rest of the tool already uses, to a schedule, and to somewhere
// for the results to go.
//
// Everything persists in one bucket under one prefix, in the same style as the
// rest of the catalogue: no database, no migrations, and a fresh deployment
// that reconstructs its entire behaviour by reading the store.
package harvest

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"museum/internal/storage"
	"museum/pkg/extract"
)

// Key layout under the bucket. Artifacts are versioned in their own directory
// per source, so listing that directory is the whole of "what versions exist"
// and no pointer object can fall out of step with what it points at.
//
//	harvest/sources/<name>.json
//	harvest/artifacts/<name>/v000004.json
//	harvest/runs/<name>/2026-08-15T12:00:00.000000000Z.json
//	harvest/output/<name>/latest.json
const (
	sourcePrefix   = "harvest/sources/"
	artifactPrefix = "harvest/artifacts/"
	runPrefix      = "harvest/runs/"
	outputPrefix   = "harvest/output/"
)

// runStamp is the time format run keys are named with. It is fixed width and
// UTC, so keys sort lexicographically in the order the runs happened, which is
// what makes "the most recent runs" a suffix of a listing rather than a sort.
const runStamp = "2006-01-02T15:04:05.000000000Z"

// RunRetention is how many runs are kept per source.
//
// Run history is not a log. The validator's trailing average is computed from
// it, so the retention window is a correctness setting rather than a
// housekeeping one: too short and a source that runs daily loses its baseline
// every time it is quiet for a week.
const RunRetention = 200

// historyDepth is how many passing runs the trailing average is taken over.
// Enough that one unusual week does not move it much; short enough that a
// source which has genuinely grown is not judged against last year.
const historyDepth = 20

// Store persists sources, artifacts and run history.
//
// It holds three typed views of the same bucket rather than one untyped one,
// so that listing a prefix decodes into the right type and a malformed object
// is caught where it is read.
type Store struct {
	sources   *storage.S3Service[extract.Source]
	artifacts *storage.S3Service[extract.Artifact]
	runs      *storage.S3Service[extract.Run]
	bucket    string
}

// ErrNoArtifact means a source has been defined but never compiled.
var ErrNoArtifact = errors.New("source has no artifact")

// ErrNoSource means no source of that name is defined.
var ErrNoSource = errors.New("no such source")

// ErrArtifactExists means that version is already stored. Versions are
// immutable, so this is a collision rather than a failure: it means something
// else wrote that version first, and the caller should read it rather than
// treat its own attempt as proof that the source is broken.
var ErrArtifactExists = errors.New("artifact version already exists and versions are immutable")

// OpenStore connects to object storage.
func OpenStore(bucket string) (*Store, error) {
	sources, err := storage.NewS3Service(func(s extract.Source) string {
		return sourcePrefix + s.Name + ".json"
	})
	if err != nil {
		return nil, err
	}
	artifacts, err := storage.NewS3Service(func(a extract.Artifact) string {
		return artifactKey(a.Source, a.Version)
	})
	if err != nil {
		return nil, err
	}
	runs, err := storage.NewS3Service(func(r extract.Run) string {
		return runPrefix + r.Source + "/" + r.At.UTC().Format(runStamp) + ".json"
	})
	if err != nil {
		return nil, err
	}

	return &Store{sources: sources, artifacts: artifacts, runs: runs, bucket: bucket}, nil
}

// EnsureBucket creates the bucket if it is missing.
func (s *Store) EnsureBucket(ctx context.Context, region string) error {
	return s.sources.EnsureBucket(ctx, s.bucket, region)
}

// artifactKey names one version. The number is zero-padded so that a listing
// comes back in version order rather than with v10 before v2.
func artifactKey(source string, version int) string {
	return fmt.Sprintf("%s%s/v%06d.json", artifactPrefix, source, version)
}

// SaveSource writes a source definition, overwriting any previous one.
func (s *Store) SaveSource(ctx context.Context, source extract.Source) error {
	if err := source.Validate(); err != nil {
		return err
	}
	return s.sources.PutObject(ctx, s.bucket, source)
}

// Source reads one source definition.
func (s *Store) Source(ctx context.Context, name string) (extract.Source, error) {
	source, err := s.sources.GetObject(ctx, s.bucket, sourcePrefix+name+".json")
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return extract.Source{}, fmt.Errorf("%w: %s", ErrNoSource, name)
	case err != nil:
		return extract.Source{}, err
	}
	return *source, nil
}

// Sources reads every source definition, in name order.
//
// The callback is locked because EachObject decodes objects concurrently and
// calls back from each of its goroutines. Appending unguarded loses entries,
// and losing a run record is not a cosmetic fault: the trailing average the
// validator judges every count against is computed from them.
func (s *Store) Sources(ctx context.Context) ([]extract.Source, error) {
	var (
		mu      sync.Mutex
		sources []extract.Source
	)
	if err := s.sources.EachObject(ctx, s.bucket, sourcePrefix, func(_ string, source extract.Source) {
		mu.Lock()
		defer mu.Unlock()
		sources = append(sources, source)
	}); err != nil {
		return nil, err
	}

	slices.SortFunc(sources, func(a, b extract.Source) int {
		return strings.Compare(a.Name, b.Name)
	})
	return sources, nil
}

// SaveArtifact writes one version.
//
// A version is never overwritten: rollback is reading an older key, and the
// diff an operator reviews after a heal is a diff of two objects that both
// still exist. StoreObject rather than PutObject enforces that, and reports a
// collision rather than silently replacing history.
func (s *Store) SaveArtifact(ctx context.Context, artifact extract.Artifact) error {
	if artifact.Source == "" || artifact.Version < 1 {
		return fmt.Errorf("artifact needs a source and a version, got %q v%d",
			artifact.Source, artifact.Version)
	}

	written, err := s.artifacts.StoreObject(ctx, s.bucket, artifact)
	if err != nil {
		return err
	}
	if !written {
		return fmt.Errorf("%w: %s v%d", ErrArtifactExists, artifact.Source, artifact.Version)
	}
	return nil
}

// Artifact reads one version.
func (s *Store) Artifact(ctx context.Context, source string, version int) (extract.Artifact, error) {
	artifact, err := s.artifacts.GetObject(ctx, s.bucket, artifactKey(source, version))
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return extract.Artifact{}, fmt.Errorf("%w: %s v%d", ErrNoArtifact, source, version)
	case err != nil:
		return extract.Artifact{}, err
	}
	return *artifact, nil
}

// Artifacts reads every version of a source's artifact, oldest first.
func (s *Store) Artifacts(ctx context.Context, source string) ([]extract.Artifact, error) {
	var (
		mu        sync.Mutex
		artifacts []extract.Artifact
	)
	if err := s.artifacts.EachObject(ctx, s.bucket, artifactPrefix+source+"/",
		func(_ string, artifact extract.Artifact) {
			mu.Lock()
			defer mu.Unlock()
			artifacts = append(artifacts, artifact)
		}); err != nil {
		return nil, err
	}

	slices.SortFunc(artifacts, func(a, b extract.Artifact) int { return a.Version - b.Version })
	return artifacts, nil
}

// CurrentArtifact reads the highest version of a source's artifact.
//
// The version is zero-padded into the key precisely so a listing comes back in
// version order, so the answer is the last key and one GET — not, as this used
// to be, every version downloaded and decoded to read one integer.
func (s *Store) CurrentArtifact(ctx context.Context, source string) (extract.Artifact, error) {
	keys, err := s.artifacts.ListKeys(ctx, s.bucket, artifactPrefix+source+"/")
	if err != nil {
		return extract.Artifact{}, err
	}
	if len(keys) == 0 {
		return extract.Artifact{}, fmt.Errorf("%w: %s", ErrNoArtifact, source)
	}

	slices.Sort(keys)
	artifact, err := s.artifacts.GetObject(ctx, s.bucket, keys[len(keys)-1])
	if err != nil {
		return extract.Artifact{}, err
	}
	return *artifact, nil
}

// LastRunAt returns when a source last ran, without reading any run.
//
// Run keys are fixed-width UTC timestamps, so the most recent run is the last
// key of the listing. The scheduler asks this of every source on every tick;
// answering it by downloading up to RunRetention objects per source turned a
// tick into hundreds of GETs against MinIO on a Pi.
func (s *Store) LastRunAt(ctx context.Context, source string) (time.Time, bool, error) {
	keys, err := s.runs.ListKeys(ctx, s.bucket, runPrefix+source+"/")
	if err != nil {
		return time.Time{}, false, err
	}
	if len(keys) == 0 {
		return time.Time{}, false, nil
	}

	slices.Sort(keys)
	newest := keys[len(keys)-1]

	stamp := strings.TrimSuffix(path.Base(newest), ".json")
	at, err := time.Parse(runStamp, stamp)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("run key %q is not a timestamp: %w", newest, err)
	}
	return at, true, nil
}

// Rollback republishes an older version as the current one.
//
// It copies rather than deletes. Deleting the versions above would destroy the
// record of what was tried and why it was wrong, which is the thing that stops
// the next heal from repeating it — and a rollback is itself a decision worth
// being able to see later.
func (s *Store) Rollback(ctx context.Context, source string, to int) (extract.Artifact, error) {
	target, err := s.Artifact(ctx, source, to)
	if err != nil {
		return extract.Artifact{}, err
	}
	current, err := s.CurrentArtifact(ctx, source)
	if err != nil {
		return extract.Artifact{}, err
	}
	if target.Version == current.Version {
		return extract.Artifact{}, fmt.Errorf("%s is already at v%d", source, to)
	}

	restored := target
	restored.Version = current.Version + 1
	restored.Parent = target.Version
	restored.Reason = fmt.Sprintf("rolled back to v%d by the operator", target.Version)
	restored.CreatedAt = time.Now().UTC()

	if err := s.SaveArtifact(ctx, restored); err != nil {
		return extract.Artifact{}, err
	}
	return restored, nil
}

// AppendRun records one run.
func (s *Store) AppendRun(ctx context.Context, run extract.Run) error {
	if run.Source == "" {
		return errors.New("run has no source")
	}
	if run.At.IsZero() {
		run.At = time.Now().UTC()
	}
	return s.runs.PutObject(ctx, s.bucket, run)
}

// Runs reads a source's most recent runs, newest first. A limit of zero reads
// everything retained.
func (s *Store) Runs(ctx context.Context, source string, limit int) ([]extract.Run, error) {
	var (
		mu   sync.Mutex
		runs []extract.Run
	)
	if err := s.runs.EachObject(ctx, s.bucket, runPrefix+source+"/",
		func(_ string, run extract.Run) {
			mu.Lock()
			defer mu.Unlock()
			runs = append(runs, run)
		}); err != nil {
		return nil, err
	}

	slices.SortFunc(runs, func(a, b extract.Run) int { return b.At.Compare(a.At) })
	if limit > 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

// History returns the record counts of a source's recent passing runs, which
// is what the validator's volumetric rung is judged against.
//
// Only passing runs count. Including the failures would poison the average
// with exactly the numbers it exists to catch: a source that broke and
// returned two records for a week would end up with a baseline that accepts
// two records.
func (s *Store) History(ctx context.Context, source string) (extract.History, error) {
	runs, err := s.Runs(ctx, source, 0)
	if err != nil {
		return extract.History{}, err
	}

	var (
		counts      []int
		fingerprint string
	)
	for _, run := range runs {
		if run.Verdict != extract.Pass {
			continue
		}
		if fingerprint == "" {
			fingerprint = run.Fingerprint
		}
		counts = append(counts, run.Records)
		if len(counts) >= historyDepth {
			break
		}
	}
	return extract.History{Counts: counts, Fingerprint: fingerprint, Complete: true}, nil
}

// PruneRuns deletes all but the most recent RunRetention runs for a source,
// and reports how many it removed.
// It works on keys alone. Reconstructing the key from a decoded run's
// timestamp meant any object that failed to decode could never be pruned —
// the one class of object most likely to need it — and it downloaded the whole
// history to decide what to delete.
func (s *Store) PruneRuns(ctx context.Context, source string) (int, error) {
	keys, err := s.runs.ListKeys(ctx, s.bucket, runPrefix+source+"/")
	if err != nil {
		return 0, err
	}
	if len(keys) <= RunRetention {
		return 0, nil
	}

	// Sorted ascending, so the surplus is the oldest at the front. Getting this
	// slice the wrong way round would delete the newest runs and leave the
	// validator judging against ancient counts.
	slices.Sort(keys)

	removed := 0
	for _, key := range keys[:len(keys)-RunRetention] {
		if err := s.runs.RemoveObject(ctx, s.bucket, key); err != nil {
			return removed, fmt.Errorf("prune %s: %w", key, err)
		}
		removed++
	}
	return removed, nil
}

// Pause stops the scheduler picking a source up, recording why.
func (s *Store) Pause(ctx context.Context, name, reason string) (extract.Source, error) {
	source, err := s.Source(ctx, name)
	if err != nil {
		return extract.Source{}, err
	}
	source.Paused, source.PausedReason = true, reason
	return source, s.SaveSource(ctx, source)
}

// Resume undoes a pause, including one set by quarantine.
func (s *Store) Resume(ctx context.Context, name string) (extract.Source, error) {
	source, err := s.Source(ctx, name)
	if err != nil {
		return extract.Source{}, err
	}
	source.Paused, source.PausedReason = false, ""
	return source, s.SaveSource(ctx, source)
}

// SaveOutput writes a source's latest delivery to the pull interface's key.
//
// One key per source, overwritten each time. A consumer polls it and sees each
// extraction once however many times the source ran, which is the whole of the
// idempotency guarantee on this side.
func (s *Store) SaveOutput(ctx context.Context, delivery Delivery) error {
	return s.sources.PutJSON(ctx, s.bucket, outputPrefix+delivery.Source+"/latest.json", delivery)
}

// Output reads back what a source last published.
func (s *Store) Output(ctx context.Context, source string) (Delivery, error) {
	var delivery Delivery
	if err := s.sources.GetJSON(ctx, s.bucket, outputPrefix+source+"/latest.json", &delivery); err != nil {
		return Delivery{}, err
	}
	return delivery, nil
}
