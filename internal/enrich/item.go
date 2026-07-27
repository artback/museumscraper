package enrich

import (
	"encoding/json"
	"fmt"
	"maps"
	"sync"
)

// Item carries an object through the pipeline together with the enrichment
// results accumulated along the way.
//
// Steps within a stage run concurrently against the same Item, so the results
// map is guarded by a mutex and is only reachable through these methods.
type Item[T any] struct {
	// Object is the entity being enriched. Steps may read it freely; they
	// should not mutate it concurrently.
	Object T

	// OnDone, when set, is called once every stage has run against the item —
	// successfully or not. It lets the source of the item know the work is
	// finished, which is what makes acknowledging a message safe.
	OnDone func()

	mu      sync.Mutex
	results map[string]any
}

// NewItem wraps obj in a pipeline item with an empty result set.
func NewItem[T any](obj T) *Item[T] {
	return &Item[T]{Object: obj, results: make(map[string]any)}
}

// Merge flattens source into the item's results by round-tripping it through
// JSON, so a step can contribute a whole struct in one call. Existing keys are
// overwritten.
func (i *Item[T]) Merge(source any) error {
	data, err := json.Marshal(source)
	if err != nil {
		return fmt.Errorf("marshal enrichment result: %w", err)
	}

	var flattened map[string]any
	if err := json.Unmarshal(data, &flattened); err != nil {
		return fmt.Errorf("flatten enrichment result: %w", err)
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	maps.Copy(i.results, flattened)
	return nil
}

// Set stores a single enrichment value.
func (i *Item[T]) Set(key string, value any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.results[key] = value
}

// Results returns a copy of everything accumulated so far, safe to hand to a
// caller outside the pipeline.
func (i *Item[T]) Results() map[string]any {
	i.mu.Lock()
	defer i.mu.Unlock()
	return maps.Clone(i.results)
}

// String returns the result at key as a string.
func (i *Item[T]) String(key string) (string, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	s, ok := i.results[key].(string)
	return s, ok
}

// Int64 returns the result at key as an int64.
//
// Values that arrived via Merge have been through JSON, so whole numbers are
// float64 rather than any integer type; asserting directly to int is the kind
// of thing that panics at runtime. All the plausible numeric types are handled
// here instead.
func (i *Item[T]) Int64(key string) (int64, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()

	switch v := i.results[key].(type) {
	case float64:
		return int64(v), true
	case float32:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case int32:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

// done invokes OnDone exactly once. The pipeline calls it after the last stage,
// whatever the outcome: an item whose enrichment failed is still finished with,
// and holding its acknowledgement back would stall the source.
func (i *Item[T]) done() {
	i.mu.Lock()
	callback := i.OnDone
	i.OnDone = nil
	i.mu.Unlock()

	if callback != nil {
		callback()
	}
}
