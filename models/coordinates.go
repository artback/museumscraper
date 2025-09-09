package models

type Coordinates struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}
type Location struct {
	Name        string      `json:"name"`
	Coordinates Coordinates `json:"coordinates"`
	Source      string      `json:"source,omitempty"` // e.g., "OpenStreetMap"
}
