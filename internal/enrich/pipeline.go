package enrich

import (
	"context"
	"log"
	"sync"
)

// doneSignaller is implemented by item types that want to be told when the
// pipeline has finished with them. enrich.Item satisfies it.
type doneSignaller interface{ done() }

// signalDone notifies an item that every stage has run, if it cares.
func signalDone[T any](item *T) {
	if signaller, ok := any(item).(doneSignaller); ok {
		signaller.done()
	}
}

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

// Process consumes items from in until the channel is closed or ctx is
// cancelled. For each item:
//   - All steps in a stage are started concurrently and must complete before
//     the next stage begins (a stage barrier).
//   - Errors returned by steps are logged and ignored so that one failed
//     enrichment does not discard the rest of the item's results.
//   - A cancelled context stops the pipeline between items; steps already
//     running are given the same context and are expected to return promptly.
//
// Process returns the number of items it processed.
func (p *Pipeline[T]) Process(ctx context.Context, in <-chan *T) int {
	processed := 0

	for {
		// Checked before the select because select picks at random among ready
		// cases: with a cancelled context and a buffered input channel, both
		// are ready and the pipeline would otherwise keep taking items.
		if err := ctx.Err(); err != nil {
			log.Printf("Pipeline stopping: %v", err)
			return processed
		}

		select {
		case <-ctx.Done():
			log.Printf("Pipeline stopping: %v", ctx.Err())
			return processed
		case item, ok := <-in:
			if !ok {
				return processed
			}
			p.processItem(ctx, item)
			processed++
		}
	}
}

// processItem runs every stage against a single item, then signals completion.
func (p *Pipeline[T]) processItem(ctx context.Context, item *T) {
	defer signalDone(item)

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
		wg.Wait() // stage barrier: all steps finish before the next stage starts
	}
}
