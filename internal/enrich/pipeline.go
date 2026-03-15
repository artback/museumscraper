package enrich

import (
	"context"
	"log"
	"sync"
)

// Pipeline coordinates the execution of a sequence of stages for items flowing
// through a channel. For each incoming item, steps within the same stage run in
// parallel, and stages themselves run sequentially. Multiple items are processed
// concurrently using a bounded worker pool.
//
// Pipeline is generic over the item type T.
type Pipeline[T any] struct {
	stages  []Stage[T]
	workers int
}

// NewPipeline constructs a Pipeline from the provided stages. Stages will be
// applied to each item in order. By default, items are processed with 1 worker
// (sequential). Use WithWorkers to increase concurrency.
func NewPipeline[T any](stages ...Stage[T]) *Pipeline[T] {
	return &Pipeline[T]{stages: stages, workers: 1}
}

// WithWorkers sets the number of concurrent workers that process items through
// the pipeline. Each worker pulls items from the input channel and runs all
// stages sequentially for that item. This allows multiple items to be enriched
// in parallel when stages involve I/O (e.g., API calls).
func (p *Pipeline[T]) WithWorkers(n int) *Pipeline[T] {
	if n > 0 {
		p.workers = n
	}
	return p
}

// Process consumes items from the input channel and runs all stages on each
// item. Workers process items concurrently (up to p.workers), but for each
// item, stages are applied sequentially. Steps within a stage run in parallel.
//
// Process blocks until the input channel is closed and all workers finish.
func (p *Pipeline[T]) Process(ctx context.Context, in <-chan *T) {
	var wg sync.WaitGroup
	for range p.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range in {
				p.processItem(ctx, item)
			}
		}()
	}
	wg.Wait()
}

// processItem runs all stages sequentially for a single item. Within a stage,
// all steps are started concurrently and must complete before the next stage.
func (p *Pipeline[T]) processItem(ctx context.Context, item *T) {
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
		wg.Wait()
	}
}
