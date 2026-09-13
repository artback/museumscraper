package crawlstate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeStore is object storage with nothing in it but a map.
type fakeStore struct {
	objects map[string][]byte
	getErr  error
	putErr  error
}

func (f *fakeStore) GetJSON(_ context.Context, _, key string, out any) error {
	if f.getErr != nil {
		return f.getErr
	}
	data, ok := f.objects[key]
	if !ok {
		return errors.New("not found")
	}
	return json.Unmarshal(data, out)
}

func (f *fakeStore) PutJSON(_ context.Context, _, key string, value any) error {
	if f.putErr != nil {
		return f.putErr
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[key] = data
	return nil
}

func TestRoundTrip(t *testing.T) {
	store := &fakeStore{}
	keeper := NewKeeper(store, "museum")
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	want := State{Source: "overture", LastRunAt: now, LastChangeAt: now, Version: "2026-08-19.0", Museums: 185000}
	if err := keeper.Save(ctx, want); err != nil {
		t.Fatal(err)
	}

	got := keeper.Load(ctx, "overture")
	if !got.LastRunAt.Equal(want.LastRunAt) || got.Version != want.Version || got.Museums != want.Museums {
		t.Errorf("loaded %+v, want %+v", got, want)
	}
}

// TestLoadOfAnUnknownSourceIsUsable: a source that has never run, and a bucket
// that cannot be read, must both come back as "never ran" rather than an error
// — the worst an unreadable state costs is one unnecessary crawl, which is
// what would have happened without any of this.
func TestLoadOfAnUnknownSourceIsUsable(t *testing.T) {
	ctx := context.Background()

	for name, keeper := range map[string]*Keeper{
		"empty bucket":    NewKeeper(&fakeStore{}, "museum"),
		"unreadable":      NewKeeper(&fakeStore{getErr: errors.New("network is down")}, "museum"),
		"no store at all": NewKeeper(nil, "museum"),
	} {
		state := keeper.Load(ctx, "overture")
		if state.Source != "overture" || !state.LastRunAt.IsZero() {
			t.Errorf("%s: got %+v, want a zero state for overture", name, state)
		}
		if !Due(state, 30*24*time.Hour, time.Now()) {
			t.Errorf("%s: a source that has never run must be due", name)
		}
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	cadence := 7 * 24 * time.Hour

	cases := []struct {
		name    string
		lastRun time.Time
		want    bool
	}{
		{"never run", time.Time{}, true},
		{"just now", now, false},
		{"six days ago", now.Add(-6 * 24 * time.Hour), false},
		{"exactly a week ago", now.Add(-cadence), true},
		{"a month ago", now.Add(-30 * 24 * time.Hour), true},
	}
	for _, c := range cases {
		if got := Due(State{LastRunAt: c.lastRun}, cadence, now); got != c.want {
			t.Errorf("%s: Due = %v, want %v", c.name, got, c.want)
		}
	}

	// A cadence of zero means the source has no natural rhythm.
	if !Due(State{LastRunAt: now}, 0, now) {
		t.Error("a source with no cadence should always be due")
	}
}

// TestRanSeparatesRunningFromChanging: a source that is being polled
// fruitlessly should be visible as exactly that, so LastChangeAt must not move
// when the upstream is still serving the version we already read.
func TestRanSeparatesRunningFromChanging(t *testing.T) {
	first := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	second := first.AddDate(0, 1, 0)

	after := Ran(State{Source: "overture"}, "2026-07-22.0", 180000, first)
	if !after.LastChangeAt.Equal(first) || after.Version != "2026-07-22.0" {
		t.Fatalf("first run: %+v", after)
	}

	// Ran again, same release: it ran, but nothing changed.
	again := Ran(after, "2026-07-22.0", 180000, second)
	if !again.LastRunAt.Equal(second) {
		t.Errorf("LastRunAt did not move: %+v", again)
	}
	if !again.LastChangeAt.Equal(first) {
		t.Errorf("LastChangeAt moved for an unchanged version: %+v", again)
	}

	// A new release moves both.
	moved := Ran(again, "2026-08-19.0", 185000, second)
	if !moved.LastChangeAt.Equal(second) || moved.Version != "2026-08-19.0" || moved.Museums != 185000 {
		t.Errorf("new release: %+v", moved)
	}
}

// TestRanWithNoVersionHandle: most of the API sources cannot say what version
// they are serving, so running them is the only evidence there is.
func TestRanWithNoVersionHandle(t *testing.T) {
	now := time.Now()

	state := Ran(State{Source: "wikidata"}, "", 81000, now)
	if !state.LastRunAt.Equal(now) || !state.LastChangeAt.Equal(now) {
		t.Errorf("got %+v, want both times set", state)
	}
}

// TestWithVersionKeepsTheOthers: the registers source has one version per
// register and they change independently, so recording one must not drop the
// rest.
func TestWithVersionKeepsTheOthers(t *testing.T) {
	state := State{Source: "registers"}.
		WithVersion("imls", `"abc123"`).
		WithVersion("museofile", "Wed, 27 Aug 2025 10:00:00 GMT")

	if got := state.VersionOf("imls"); got != `"abc123"` {
		t.Errorf("imls version = %q", got)
	}
	if got := state.VersionOf("museofile"); got == "" {
		t.Error("museofile version was lost")
	}

	// Updating one leaves the other alone.
	state = state.WithVersion("imls", `"def456"`)
	if state.VersionOf("museofile") == "" {
		t.Error("updating one register dropped another")
	}
	if state.VersionOf("imls") != `"def456"` {
		t.Error("the new version was not stored")
	}
}

func TestSaveRefusesAnUnnamedSource(t *testing.T) {
	if err := NewKeeper(&fakeStore{}, "museum").Save(context.Background(), State{}); err == nil {
		t.Error("saving a state with no source should fail")
	}
}
