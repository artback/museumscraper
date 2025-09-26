package main

type PipelineItem struct {
	Object  *MuseumObject
	Results map[string]any
}

func NewPipelineItem(obj *MuseumObject) *PipelineItem {
	return &PipelineItem{
		Object:  obj,
		Results: make(map[string]any),
	}
}
