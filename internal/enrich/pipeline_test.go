package enrich

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type PipelineItem struct {
	Results map[string]any
}

func NewPipelineItem() *PipelineItem {
	return &PipelineItem{Results: make(map[string]any)}
}

func StepAddFoo(_ context.Context, item *PipelineItem) error {
	item.Results["foo"] = "bar"
	return nil
}

func StepAddValue(key string, val any) Step[PipelineItem] {
	return func(ctx context.Context, item *PipelineItem) error {
		item.Results[key] = val
		return nil
	}
}

func StepError(_ context.Context, _ *PipelineItem) error {
	return errors.New("mock step failed")
}
func TestPipeline_Process(t *testing.T) {
	tests := []struct {
		name     string
		stages   []Stage[PipelineItem]
		input    *PipelineItem
		expected map[string]any
	}{
		{
			name:   "single step adds foo",
			stages: []Stage[PipelineItem]{NewStage(StepAddFoo)},
			input:  NewPipelineItem(),
			expected: map[string]any{
				"foo": "bar",
			},
		},
		{
			name: "two steps in one stage run in parallel",
			stages: []Stage[PipelineItem]{
				NewStage(
					StepAddValue("x", 1),
					StepAddValue("y", 2),
				),
			},
			input: NewPipelineItem(),
			expected: map[string]any{
				"x": 1,
				"y": 2,
			},
		},
		{
			name: "multi-stage sequential dependency",
			stages: []Stage[PipelineItem]{
				NewStage(StepAddValue("a", "first")),
				NewStage(StepAddValue("b", "second")),
			},
			input: NewPipelineItem(),
			expected: map[string]any{
				"a": "first",
				"b": "second",
			},
		},
		{
			name: "step error does not break pipeline",
			stages: []Stage[PipelineItem]{
				NewStage(StepError),
				NewStage(StepAddValue("ok", true)),
			},
			input: NewPipelineItem(),
			expected: map[string]any{
				"ok": true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			in := make(chan *PipelineItem, 1)
			in <- tt.input
			close(in)

			p := NewPipeline(tt.stages...)
			p.Process(ctx, in)

			if !reflect.DeepEqual(tt.input.Results, tt.expected) {
				t.Errorf("got %+v, expected %+v", tt.input.Results, tt.expected)
			}
		})
	}
}
