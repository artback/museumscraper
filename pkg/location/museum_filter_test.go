package location

import "testing"

func TestIsMuseum(t *testing.T) {
	tests := []struct {
		name   string
		result GeoResult
		want   bool
	}{
		{
			name:   "tourism/museum class",
			result: GeoResult{Class: "tourism", Type: "museum", Name: "Louvre"},
			want:   true,
		},
		{
			name:   "tourism/gallery class",
			result: GeoResult{Class: "tourism", Type: "gallery", Name: "Tate"},
			want:   true,
		},
		{
			name:   "amenity/arts_centre class",
			result: GeoResult{Class: "amenity", Type: "arts_centre", Name: "Art Centre"},
			want:   true,
		},
		{
			name:   "building/museum class",
			result: GeoResult{Class: "building", Type: "museum", Name: "Some Building"},
			want:   true,
		},
		{
			name:   "name contains museum",
			result: GeoResult{Class: "place", Type: "city", Name: "National Museum of Art"},
			want:   true,
		},
		{
			name:   "name contains gallery in display name",
			result: GeoResult{Class: "place", Type: "building", Name: "Tate", DisplayName: "Tate Gallery, London"},
			want:   true,
		},
		{
			name:   "name contains museo",
			result: GeoResult{Class: "place", Type: "building", Name: "Museo del Prado"},
			want:   true,
		},
		{
			name:   "not a museum - restaurant",
			result: GeoResult{Class: "amenity", Type: "restaurant", Name: "The Good Eats"},
			want:   false,
		},
		{
			name:   "not a museum - park",
			result: GeoResult{Class: "leisure", Type: "park", Name: "Central Park"},
			want:   false,
		},
		{
			name:   "not a museum - generic place",
			result: GeoResult{Class: "place", Type: "city", Name: "Paris"},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.result.IsMuseum()
			if got != tt.want {
				t.Errorf("IsMuseum() = %v, want %v (class=%s, type=%s, name=%s)",
					got, tt.want, tt.result.Class, tt.result.Type, tt.result.Name)
			}
		})
	}
}
