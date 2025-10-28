package enrich

import (
	"context"
	"log"
	"sync"
)

// Pipeline coordinates the execution of a sequence of stages for items flowing
// through a channel. For each incoming item, steps within the same stage run in
// parallel, and stages themselves run sequentially. Any step errors are logged
// and do not stop processing of the current item.
//
// Pipeline is generic over the item type T.
type Pipeline[T any] struct {
	stages []Stage[T]
}

// NewPipeline constructs a Pipeline from the provided stages. Stages will be
// applied to each item in order.
func NewPipeline[T any](stages ...Stage[T]) *Pipeline[T] {
	return &Pipeline[T]{stages: stages}
}

// Process consumes items from the input channel and returns a channel that will
// emit the same items after all stages have been applied. For each item:
//   - All steps in a stage are started concurrently and must complete before
//     moving to the next stage (a stage barrier).
//   - Errors returned by steps are logged and ignored so the pipeline can
//     continue processing.
//   - The provided context can be observed by steps for cancellation; the
//     pipeline itself keeps running until the input channel is closed.
func (p *Pipeline[T]) Process(ctx context.Context, in <-chan *T) {
	for item := range in {
		// Execute each stage sequentially. Within a stage, run each step in its
		// own goroutine
		for _, stage := range p.stages {
			var wg sync.WaitGroup
			for _, step := range stage.steps {
				wg.Add(1)
				go func(step Step[T]) {
					defer wg.Done()
					if err := step(ctx, item); err != nil {
						log.Printf("Step failed: %v", err)
					}
				}(step)
			}
			wg.Wait() // stage barrier: ensure all steps finished before the next stage
		}
	}
}
