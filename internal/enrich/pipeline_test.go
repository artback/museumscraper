package enrich

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// museum is a stand-in for the object a pipeline enriches.
type museum struct{ Name string }

// item is the pipeline's item type for these tests. Using the real Item rather
// than a bare map matters: steps in the same stage run concurrently against the
// same item, so an unsynchronised results map is a data race the -race detector
// reports.
type item = Item[museum]

func newItem() *item { return NewItem(museum{Name: "test"}) }

func stepAddFoo(_ context.Context, i *item) error {
	i.Set("foo", "bar")
	return nil
}

func stepAddValue(key string, val any) Step[item] {
	return func(_ context.Context, i *item) error {
		i.Set(key, val)
		return nil
	}
}

func stepError(_ context.Context, _ *item) error {
	return errors.New("mock step failed")
}

func TestPipeline_Process(t *testing.T) {
	tests := []struct {
		name     string
		stages   []Stage[item]
		expected map[string]any
	}{
		{
			name:     "single step adds foo",
			stages:   []Stage[item]{NewStage(stepAddFoo)},
			expected: map[string]any{"foo": "bar"},
		},
		{
			name: "two steps in one stage run in parallel",
			stages: []Stage[item]{
				NewStage(
					stepAddValue("x", 1),
					stepAddValue("y", 2),
				),
			},
			expected: map[string]any{"x": 1, "y": 2},
		},
		{
			name: "multi-stage sequential dependency",
			stages: []Stage[item]{
				NewStage(stepAddValue("a", "first")),
				NewStage(stepAddValue("b", "second")),
			},
			expected: map[string]any{"a": "first", "b": "second"},
		},
		{
			name: "step error does not break pipeline",
			stages: []Stage[item]{
				NewStage(stepError),
				NewStage(stepAddValue("ok", true)),
			},
			expected: map[string]any{"ok": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			input := newItem()
			in := make(chan *item, 1)
			in <- input
			close(in)

			processed := NewPipeline(tt.stages...).Process(ctx, in)

			if processed != 1 {
				t.Errorf("processed = %d, want 1", processed)
			}
			if got := input.Results(); !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("got %+v, expected %+v", got, tt.expected)
			}
		})
	}
}

// TestPipeline_StopsOnCancel checks that a cancelled context ends the run
// instead of draining the rest of the channel.
func TestPipeline_StopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	in := make(chan *item, 2)
	in <- newItem()
	in <- newItem()
	close(in)

	if processed := NewPipeline(NewStage(stepAddFoo)).Process(ctx, in); processed != 0 {
		t.Errorf("processed = %d, want 0 for an already-cancelled context", processed)
	}
}

func TestItem_TypedAccessors(t *testing.T) {
	i := newItem()

	// Values merged from a struct arrive via JSON, so whole numbers become
	// float64. Int64 must cope with that rather than panicking on a type
	// assertion.
	if err := i.Merge(struct {
		OsmType string `json:"osm_type"`
		OsmID   int64  `json:"osm_id"`
	}{OsmType: "R", OsmID: 7515426}); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	if got, ok := i.String("osm_type"); !ok || got != "R" {
		t.Errorf("String(osm_type) = %q, %v; want \"R\", true", got, ok)
	}
	if got, ok := i.Int64("osm_id"); !ok || got != 7515426 {
		t.Errorf("Int64(osm_id) = %d, %v; want 7515426, true", got, ok)
	}
	if _, ok := i.Int64("absent"); ok {
		t.Error("Int64(absent) reported ok for a missing key")
	}
	if _, ok := i.String("osm_id"); ok {
		t.Error("String(osm_id) reported ok for a numeric value")
	}
}

// TestItem_ConcurrentMerge exercises the locking directly, so the race detector
// has something to bite on even if the pipeline changes shape.
func TestItem_ConcurrentMerge(t *testing.T) {
	i := newItem()
	done := make(chan struct{})

	for n := range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			i.Set("shared", n)
			_ = i.Results()
		}()
	}
	for range 8 {
		<-done
	}

	if _, ok := i.Results()["shared"]; !ok {
		t.Error("expected the shared key to be present")
	}
}

// TestPipeline_SignalsDone checks that the completion callback fires after the
// stages run — it is what lets a Kafka consumer commit an offset safely.
func TestPipeline_SignalsDone(t *testing.T) {
	cases := []struct {
		name   string
		stages []Stage[item]
	}{
		{name: "successful stages", stages: []Stage[item]{NewStage(stepAddFoo)}},
		// A failed enrichment is still finished with; withholding the signal
		// would stall whatever is waiting on it.
		{name: "failing stage", stages: []Stage[item]{NewStage(stepError)}},
		{name: "no stages", stages: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			calls := 0
			input := newItem()
			input.OnDone = func() { calls++ }

			in := make(chan *item, 1)
			in <- input
			close(in)

			NewPipeline(tc.stages...).Process(ctx, in)

			if calls != 1 {
				t.Errorf("OnDone called %d times, want exactly 1", calls)
			}
		})
	}
}

// TestItem_DoneIsIdempotent guards the acknowledgement against double firing,
// which would let a consumer commit past work that is still in flight.
func TestItem_DoneIsIdempotent(t *testing.T) {
	calls := 0
	i := newItem()
	i.OnDone = func() { calls++ }

	i.done()
	i.done()
	i.done()

	if calls != 1 {
		t.Errorf("OnDone called %d times, want exactly 1", calls)
	}
}
