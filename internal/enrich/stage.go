// Package enrich provides a small, generic pipeline abstraction that allows
// running independent enrichment steps in parallel within a stage, while
// enforcing sequential execution between stages.
package enrich

import (
	"context"
)

// Step represents a single enrichment operation that mutates the given item.
// Implementations should be safe to run concurrently with other steps in the
// same stage operating on the same item. If a step fails it should return an
// error; the pipeline will log the error and continue.
// The context can be used to observe cancellation or timeouts.
//
// The item pointer allows steps to modify the entity in-place to accumulate
// enrichment data over the pipeline run.
//
// Example:
//
//	func addTitle(ctx context.Context, m *MyType) error { m.Title = "..."; return nil }
type Step[T any] func(ctx context.Context, item *T) error

// Stage groups a set of steps that are safe to execute in parallel for a
// single item. All steps in a stage are started together, and the pipeline waits
// for them to complete before moving to the next stage.
//
// Note: Step functions must coordinate on shared fields if they might write to
// the same location concurrently.
type Stage[T any] struct {
	steps []Step[T]
}

// NewStage constructs a Stage from the provided steps.
// Steps in a stage are executed concurrently for each item.
func NewStage[T any](steps ...Step[T]) Stage[T] {
	return Stage[T]{steps: steps}
}
