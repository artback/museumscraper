// Package crawlstate remembers what each source left behind, so a crawl can
// tell what is worth doing again.
//
// The sources have nothing in common except that re-reading them is expensive
// and usually pointless. Overture publishes once a month and never rewrites a
// release; the American museum file has not changed since 2018 and its
// publisher has said it never will; Muséofile moves a few times a year;
// Wikidata changes every minute. Running all of them on one cadence means
// either reading a 2 GB release twelve times to find eleven copies of what we
// already had, or waiting a month to pick up a Wikipedia edit.
//
// Two questions answer that, and they are different questions. *Is it due* is
// about how fast a source goes stale, and is answered from the clock. *Has it
// changed* is about what the upstream actually says, and is answered by asking
// it as cheaply as it can be asked — a release listing, an ETag — before
// spending anything on the data behind it. A source can be due and unchanged,
// which is the case worth catching: it costs one request to find out and saves
// the rest.
//
// State lives in the bucket, under one key per source, because that is how
// everything else in this catalogue coordinates. No subcommand calls another
// and none of them share a database for this: they leave records under key
// prefixes and read each other's leavings.
package crawlstate

import (
	"context"
	"fmt"
	"path"
	"time"

	"museum/internal/storage"
)

// Prefix is where per-source state is kept in the bucket.
const Prefix = "crawl_state/"

// State is what one source left behind.
type State struct {
	Source string `json:"source"`

	// LastRunAt is when the source last actually ran. A run that was skipped
	// as not due does not move it: the question it answers is "how long since
	// we read this", and a skip read nothing.
	LastRunAt time.Time `json:"last_run_at,omitzero"`

	// LastChangeAt is when the upstream last turned out to have something new.
	// Kept apart from LastRunAt so a source that is being polled fruitlessly is
	// visible as exactly that.
	LastChangeAt time.Time `json:"last_change_at,omitzero"`

	// Version identifies what the upstream was serving when it was last read —
	// an Overture release id, an ETag, a Last-Modified. Empty for a source that
	// offers no such handle, which is most of the APIs.
	Version string `json:"version,omitempty"`

	// Versions is the same thing for a source that reads several files, keyed
	// by the name each carries in a record's Sources. The registers source has
	// one per register, and they change independently.
	Versions map[string]string `json:"versions,omitempty"`

	// Museums is how many the source produced last time it ran, which is what
	// makes a source that has quietly stopped finding anything visible.
	Museums int `json:"museums,omitempty"`
}

// VersionOf returns the stored version for one file of a multi-file source.
func (s State) VersionOf(name string) string { return s.Versions[name] }

// WithVersion returns a copy carrying a version for one file of a multi-file
// source.
func (s State) WithVersion(name, version string) State {
	versions := make(map[string]string, len(s.Versions)+1)
	for k, v := range s.Versions {
		versions[k] = v
	}
	if version == "" {
		delete(versions, name)
	} else {
		versions[name] = version
	}
	s.Versions = versions
	return s
}

// Store is the bit of object storage this package needs.
type Store interface {
	GetJSON(ctx context.Context, bucket, key string, out any) error
	PutJSON(ctx context.Context, bucket, key string, value any) error
}

// Keeper reads and writes source state in one bucket.
type Keeper struct {
	store  Store
	bucket string
}

// NewKeeper returns a Keeper over the given bucket.
func NewKeeper(store Store, bucket string) *Keeper {
	return &Keeper{store: store, bucket: bucket}
}

// Load returns what a source left behind, or a zero State for one that has
// never run.
//
// A state that cannot be read is not an error the caller should act on: the
// worst it costs is running a source that did not need running, which is what
// would have happened anyway without any of this.
func (k *Keeper) Load(ctx context.Context, source string) State {
	state := State{Source: source}
	if k == nil || k.store == nil {
		return state
	}

	if err := k.store.GetJSON(ctx, k.bucket, keyFor(source), &state); err != nil {
		return State{Source: source}
	}
	state.Source = source
	return state
}

// Save records what a source found.
func (k *Keeper) Save(ctx context.Context, state State) error {
	if k == nil || k.store == nil {
		return nil
	}
	if state.Source == "" {
		return fmt.Errorf("cannot save state with no source")
	}
	return k.store.PutJSON(ctx, k.bucket, keyFor(state.Source), state)
}

// keyFor is where one source's state lives.
func keyFor(source string) string { return path.Join(Prefix, source+".json") }

// Due reports whether enough time has passed since a source last ran.
//
// A source that has never run is always due, and a cadence of zero means the
// source has no natural rhythm and should run whenever it is asked.
func Due(state State, cadence time.Duration, now time.Time) bool {
	if cadence <= 0 || state.LastRunAt.IsZero() {
		return true
	}
	return !now.Before(state.LastRunAt.Add(cadence))
}

// NextDue is when a source will next be worth running, for a log line that
// says how long the skip lasts.
func NextDue(state State, cadence time.Duration) time.Time {
	if state.LastRunAt.IsZero() {
		return time.Time{}
	}
	return state.LastRunAt.Add(cadence)
}

// Ran returns the state to store after a source has run, given what the
// upstream said its version was and how many museums came out.
//
// LastChangeAt moves only when the version actually moved, so "we keep asking
// and it keeps saying the same thing" stays distinguishable from "it changed
// and we read it".
func Ran(state State, version string, museums int, now time.Time) State {
	next := state
	next.LastRunAt = now
	next.Museums = museums

	if version != "" && version != state.Version {
		next.LastChangeAt = now
	}
	if version != "" {
		next.Version = version
	}
	if state.Version == "" && version == "" {
		// A source with no version handle at all: every run is the only
		// evidence there is, so treat reading it as the change.
		next.LastChangeAt = now
	}
	return next
}

// compile-time check that the storage service satisfies Store.
var _ Store = (*storage.S3Service[struct{}])(nil)
