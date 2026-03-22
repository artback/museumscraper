package models

import (
	"fmt"
	"strings"
)

type Museum struct {
	Country      string            `json:"country"`
	Name         string            `json:"name"`
	City         string            `json:"city,omitempty"`
	State        string            `json:"state,omitempty"`
	Address      string            `json:"address,omitempty"`
	Lat          float64           `json:"lat,omitempty"`
	Lon          float64           `json:"lon,omitempty"`
	OsmID        int64             `json:"osm_id,omitempty"`
	OsmType      string            `json:"osm_type,omitempty"`
	Category     string            `json:"category,omitempty"`
	MuseumType   string            `json:"museum_type,omitempty"`
	WikipediaURL string            `json:"wikipedia_url,omitempty"`
	Website      string            `json:"website,omitempty"`
	RawTags      map[string]string `json:"raw_tags,omitempty"`
}

func (m Museum) StorageKey() string {
	return fmt.Sprintf("raw_data/%s/%s.json", sanitizeKey(m.Country), sanitizeKey(m.Name))
}

func sanitizeKey(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, " ", "-"))
}
