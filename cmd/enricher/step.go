package main

import (
	"context"
	"fmt"
	"museum/internal/models"
	"museum/internal/service"
	"museum/pkg/location"
)

type PipelineItem[T any] struct {
	Object  T
	Results map[string]any
}

func NewPipelineItem[T any](obj T) *PipelineItem[T] {
	return &PipelineItem[T]{
		Object:  obj,
		Results: make(map[string]any),
	}
}

func StepLocation(_ context.Context, item *PipelineItem[*models.Museum]) error {
	locationTerm := fmt.Sprintf("%s %s", item.Object.Name, item.Object.Country)
	location, err := location.Geocode(locationTerm)
	item.Results["location"] = location
	return err
}

func StepLocationDetails(_ context.Context, item *PipelineItem[*models.Museum]) error {
	osmType := item.Results["osmType"].(string)
	osmID := item.Results["osmID"].(int)
	details, err := location.PlaceDetails(osmType, osmID)
	item.Results["details"] = details
	return err
}

func initializePipelineItems(in <-chan *service.FetchedObject[*models.Museum]) <-chan *PipelineItem[*models.Museum] {
	out := make(chan *PipelineItem[*models.Museum])

	go func() {
		defer close(out)

		for obj := range in {
			item := &PipelineItem[*models.Museum]{Object: obj.Data}
			out <- item
		}
	}()

	return out
}
