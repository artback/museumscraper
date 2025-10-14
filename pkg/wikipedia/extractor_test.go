package wikipedia

import "testing"

func TestMuseumExtractor_ExtractMuseums(t *testing.T) {
	tests := []struct {
		name        string
		blacklist   []string
		content     string
		wantMuseums []string
	}{
		{
			name:      "basic list with bullet and hash lines",
			blacklist: nil,
			content: `* [[Museum of Art]]
# [[History Museum]]
Plain line without link
* [[Science Museum|Science]]`,
			wantMuseums: []string{"Museum of Art", "History Museum", "Science Museum"},
		},
		{
			name:        "filters blocklisted prefixes (Category)",
			blacklist:   []string{"Category"},
			content:     `* [[Category:Museums in France]]\n* [[Louvre]]`,
			wantMuseums: []string{"Louvre"},
		},
		{
			name:        "ignores lines without proper link markers",
			blacklist:   nil,
			content:     `* No brackets here\n# also missing brackets`,
			wantMuseums: nil,
		},
		{
			name:        "handles namespace and pipe, keeps title part before pipe",
			blacklist:   []string{"File"},
			content:     `* [[File:Picture.jpg|thumb]]\n* [[National Museum of Art|National Museum]]`,
			wantMuseums: []string{"National Museum of Art"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex := NewMuseumExtractor(tt.blacklist)
			got := ex.ExtractMuseums(tt.content)
			if len(got) != len(tt.wantMuseums) {
				t.Fatalf("unexpected count: got %d want %d (values: %v)", len(got), len(tt.wantMuseums), got)
			}
			for i := range tt.wantMuseums {
				if got[i] != tt.wantMuseums[i] {
					t.Errorf("idx %d: got %q want %q", i, got[i], tt.wantMuseums[i])
				}
			}
		})
	}
}
