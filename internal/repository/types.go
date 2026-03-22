package repository

import "time"

// Exhibition represents an exhibition record in the database.
type Exhibition struct {
	ID          int64      `json:"id"`
	MuseumID    int64      `json:"museum_id"`
	Title       string     `json:"title"`
	Description *string    `json:"description,omitempty"`
	StartDate   *time.Time `json:"start_date,omitempty"`
	EndDate     *time.Time `json:"end_date,omitempty"`
	IsPermanent bool       `json:"is_permanent"`
	SourceURL   *string    `json:"source_url,omitempty"`
}
