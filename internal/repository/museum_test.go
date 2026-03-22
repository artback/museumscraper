package repository

import (
	"context"
	"testing"
	"time"
)

// MuseumQuerier defines the interface for museum query operations, enabling
// testability without a real database connection.
type MuseumQuerier interface {
	UpsertMuseum(ctx context.Context, m Museum) (int64, error)
	UpsertMuseumBatch(ctx context.Context, museums []Museum) ([]int64, error)
	FindByCity(ctx context.Context, city string) ([]Museum, error)
	FindNearby(ctx context.Context, lat, lon, radiusMeters float64, limit int) ([]Museum, error)
	FindByCountry(ctx context.Context, country string) ([]Museum, error)
	SearchByName(ctx context.Context, query string, limit int) ([]Museum, error)
	GetByID(ctx context.Context, id int64) (Museum, error)
	List(ctx context.Context, limit, offset int) ([]Museum, int, error)
}

// ExhibitionQuerier defines the interface for exhibition query operations.
type ExhibitionQuerier interface {
	Create(ctx context.Context, e Exhibition) (int64, error)
	FindActiveByMuseum(ctx context.Context, museumID int64) ([]Exhibition, error)
	FindActiveInCity(ctx context.Context, city string) ([]ExhibitionWithMuseum, error)
	FindActiveNearby(ctx context.Context, lat, lon, radiusMeters float64, limit int) ([]ExhibitionWithMuseum, error)
}

// Compile-time interface satisfaction checks.
var _ MuseumQuerier = (*MuseumRepository)(nil)
var _ ExhibitionQuerier = (*ExhibitionRepository)(nil)

// --- Museum struct tests ---

func strPtr(s string) *string       { return &s }
func float64Ptr(f float64) *float64 { return &f }
func int64Ptr(i int64) *int64       { return &i }
func timePtr(t time.Time) *time.Time { return &t }

func TestMuseumStruct_Fields(t *testing.T) {
	tests := []struct {
		name   string
		museum Museum
		check  func(t *testing.T, m Museum)
	}{
		{
			name: "all fields populated",
			museum: Museum{
				ID:           1,
				Name:         "Louvre",
				Country:      "France",
				City:         strPtr("Paris"),
				State:        strPtr("Ile-de-France"),
				Address:      strPtr("Rue de Rivoli"),
				Lat:          float64Ptr(48.8606),
				Lon:          float64Ptr(2.3376),
				OsmID:        int64Ptr(123456),
				OsmType:      strPtr("way"),
				Category:     strPtr("art"),
				MuseumType:   strPtr("art_museum"),
				WikipediaURL: strPtr("https://en.wikipedia.org/wiki/Louvre"),
				Website:      strPtr("https://www.louvre.fr"),
				RawTags:      map[string]string{"tourism": "museum"},
			},
			check: func(t *testing.T, m Museum) {
				if m.Name != "Louvre" {
					t.Errorf("expected Name=Louvre, got %s", m.Name)
				}
				if m.Country != "France" {
					t.Errorf("expected Country=France, got %s", m.Country)
				}
				if *m.City != "Paris" {
					t.Errorf("expected City=Paris, got %s", *m.City)
				}
				if *m.Lat != 48.8606 {
					t.Errorf("expected Lat=48.8606, got %f", *m.Lat)
				}
				if *m.Lon != 2.3376 {
					t.Errorf("expected Lon=2.3376, got %f", *m.Lon)
				}
				if m.RawTags["tourism"] != "museum" {
					t.Errorf("expected raw_tags tourism=museum, got %v", m.RawTags)
				}
			},
		},
		{
			name: "nullable fields are nil",
			museum: Museum{
				Name:    "Unknown Museum",
				Country: "Unknown",
			},
			check: func(t *testing.T, m Museum) {
				if m.City != nil {
					t.Errorf("expected City=nil, got %v", m.City)
				}
				if m.Lat != nil {
					t.Errorf("expected Lat=nil, got %v", m.Lat)
				}
				if m.RawTags != nil {
					t.Errorf("expected RawTags=nil, got %v", m.RawTags)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, tc.museum)
		})
	}
}

func TestExhibitionStruct_Fields(t *testing.T) {
	tests := []struct {
		name       string
		exhibition Exhibition
		check      func(t *testing.T, e Exhibition)
	}{
		{
			name: "permanent exhibition",
			exhibition: Exhibition{
				ID:          1,
				MuseumID:    10,
				Title:       "Egyptian Antiquities",
				Description: strPtr("Ancient Egyptian artifacts"),
				IsPermanent: true,
			},
			check: func(t *testing.T, e Exhibition) {
				if !e.IsPermanent {
					t.Error("expected IsPermanent=true")
				}
				if e.EndDate != nil {
					t.Errorf("expected EndDate=nil for permanent, got %v", e.EndDate)
				}
			},
		},
		{
			name: "temporary exhibition with dates",
			exhibition: Exhibition{
				ID:        2,
				MuseumID:  10,
				Title:     "Impressionist Summer",
				StartDate: timePtr(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)),
				EndDate:   timePtr(time.Date(2025, 9, 30, 0, 0, 0, 0, time.UTC)),
			},
			check: func(t *testing.T, e Exhibition) {
				if e.IsPermanent {
					t.Error("expected IsPermanent=false")
				}
				expectedStart := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
				if !e.StartDate.Equal(expectedStart) {
					t.Errorf("expected StartDate=2025-06-01, got %v", *e.StartDate)
				}
				expectedEnd := time.Date(2025, 9, 30, 0, 0, 0, 0, time.UTC)
				if !e.EndDate.Equal(expectedEnd) {
					t.Errorf("expected EndDate=2025-09-30, got %v", *e.EndDate)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, tc.exhibition)
		})
	}
}

func TestMuseum_WithExhibitionsAndDistance(t *testing.T) {
	m := Museum{
		ID:      1,
		Name:    "Metropolitan Museum of Art",
		Country: "United States",
		City:    strPtr("New York"),
		Exhibitions: []Exhibition{
			{ID: 1, MuseumID: 1, Title: "Modern Art Wing", IsPermanent: true},
			{ID: 2, MuseumID: 1, Title: "Special Exhibition", StartDate: timePtr(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)), EndDate: timePtr(time.Date(2025, 6, 30, 0, 0, 0, 0, time.UTC))},
		},
		Distance: float64Ptr(500.0),
	}

	if m.Name != "Metropolitan Museum of Art" {
		t.Errorf("expected Name=Metropolitan Museum of Art, got %s", m.Name)
	}
	if len(m.Exhibitions) != 2 {
		t.Errorf("expected 2 exhibitions, got %d", len(m.Exhibitions))
	}
	if *m.Distance != 500.0 {
		t.Errorf("expected Distance=500.0, got %f", *m.Distance)
	}
}

func TestExhibitionWithMuseum_Fields(t *testing.T) {
	ewm := ExhibitionWithMuseum{
		Exhibition: Exhibition{
			ID:       1,
			MuseumID: 5,
			Title:    "Dinosaur Hall",
		},
		MuseumName: "Natural History Museum",
		Distance:   float64Ptr(1200.5),
	}

	if ewm.MuseumName != "Natural History Museum" {
		t.Errorf("expected MuseumName=Natural History Museum, got %s", ewm.MuseumName)
	}
	if *ewm.Distance != 1200.5 {
		t.Errorf("expected Distance=1200.5, got %f", *ewm.Distance)
	}
	if ewm.Title != "Dinosaur Hall" {
		t.Errorf("expected embedded Title=Dinosaur Hall, got %s", ewm.Title)
	}
}

// --- marshalRawTags tests ---

func TestMarshalRawTags(t *testing.T) {
	tests := []struct {
		name    string
		tags    map[string]string
		wantNil bool
		wantErr bool
	}{
		{
			name:    "nil tags returns nil",
			tags:    nil,
			wantNil: true,
		},
		{
			name: "non-nil tags returns json",
			tags: map[string]string{"tourism": "museum", "name": "Louvre"},
		},
		{
			name: "empty map returns json",
			tags: map[string]string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := marshalRawTags(tc.tags)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantNil && b != nil {
				t.Errorf("expected nil, got %s", string(b))
			}
			if !tc.wantNil && tc.tags != nil && b == nil {
				t.Error("expected non-nil bytes, got nil")
			}
		})
	}
}

// --- Batch size constant test ---

func TestBatchSizeConstant(t *testing.T) {
	if batchSize != 100 {
		t.Errorf("expected batchSize=100, got %d", batchSize)
	}
}

// --- NewMuseumRepository / NewExhibitionRepository nil pool tests ---

func TestNewMuseumRepository_NilPool(t *testing.T) {
	repo := NewMuseumRepository(nil)
	if repo == nil {
		t.Fatal("expected non-nil repository")
	}
	if repo.pool != nil {
		t.Error("expected nil pool")
	}
}

func TestNewExhibitionRepository_NilPool(t *testing.T) {
	repo := NewExhibitionRepository(nil)
	if repo == nil {
		t.Fatal("expected non-nil repository")
	}
	if repo.pool != nil {
		t.Error("expected nil pool")
	}
}

// --- SQL query constant tests ---

func TestUpsertQueryContainsRequiredClauses(t *testing.T) {
	tests := []struct {
		name     string
		contains string
	}{
		{"INSERT INTO", "INSERT INTO museums"},
		{"ON CONFLICT", "ON CONFLICT (name, country)"},
		{"RETURNING id", "RETURNING id"},
		{"ST_SetSRID", "ST_SetSRID(ST_MakePoint"},
		{"COALESCE on update", "COALESCE(EXCLUDED.city, museums.city)"},
		{"updated_at NOW", "updated_at   = NOW()"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !containsSubstring(upsertQuery, tc.contains) {
				t.Errorf("upsertQuery missing %q", tc.contains)
			}
		})
	}
}

func TestMuseumColumnsContainsExpectedFields(t *testing.T) {
	expected := []string{"id", "name", "country", "city", "state", "address",
		"lat", "lon", "osm_id", "osm_type", "category", "museum_type",
		"wikipedia_url", "website", "raw_tags"}

	for _, col := range expected {
		if !containsSubstring(museumColumns, col) {
			t.Errorf("museumColumns missing %q", col)
		}
	}
}

func TestExhibitionColumnsContainsExpectedFields(t *testing.T) {
	expected := []string{"id", "museum_id", "title", "description",
		"start_date", "end_date", "is_permanent", "source_url"}

	for _, col := range expected {
		if !containsSubstring(exhibitionColumns, col) {
			t.Errorf("exhibitionColumns missing %q", col)
		}
	}
}

func TestActiveExhibitionCondition(t *testing.T) {
	tests := []struct {
		name     string
		contains string
	}{
		{"is_permanent", "e.is_permanent = true"},
		{"end_date IS NULL", "e.end_date IS NULL"},
		{"end_date >= CURRENT_DATE", "e.end_date >= CURRENT_DATE"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !containsSubstring(activeExhibitionCondition, tc.contains) {
				t.Errorf("activeExhibitionCondition missing %q", tc.contains)
			}
		})
	}
}

// containsSubstring checks if s contains substr.
func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
