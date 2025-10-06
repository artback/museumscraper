package main

import (
	"museum/internal/models"
)

type PipelineItem struct {
	Object  *models.MuseumObject
	Results map[string]any
}

func NewPipelineItem(obj *models.MuseumObject) *PipelineItem {
	return &PipelineItem{
		Object:  obj,
		Results: make(map[string]any),
	}
}
