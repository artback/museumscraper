package scraper

import "testing"

func TestMergeWebData(t *testing.T) {
	t.Run("both nil", func(t *testing.T) {
		result := mergeWebData(nil, nil)
		if result != nil {
			t.Error("expected nil")
		}
	})

	t.Run("only jsonLD", func(t *testing.T) {
		jsonLD := &ParsedWebData{Description: "from jsonld"}
		result := mergeWebData(jsonLD, nil)
		if result.Description != "from jsonld" {
			t.Errorf("Description = %q", result.Description)
		}
	})

	t.Run("only htmlMeta", func(t *testing.T) {
		html := &ParsedWebData{Description: "from html"}
		result := mergeWebData(nil, html)
		if result.Description != "from html" {
			t.Errorf("Description = %q", result.Description)
		}
	})

	t.Run("jsonLD takes precedence", func(t *testing.T) {
		jsonLD := &ParsedWebData{
			Description:  "jsonLD desc",
			Exhibitions:  []Exhibition{{Name: "Ex1"}},
			OpeningHours: "Mo-Su 10:00-17:00",
		}
		html := &ParsedWebData{
			Description: "html desc",
			Image:       "image.jpg",
			Offers:      []Offer{{Price: "15"}},
			Exhibitions: []Exhibition{{Name: "Ex2"}},
		}
		result := mergeWebData(jsonLD, html)
		if result.Description != "jsonLD desc" {
			t.Errorf("Description = %q, want jsonLD desc", result.Description)
		}
		if result.Image != "image.jpg" {
			t.Errorf("Image not filled from HTML: %q", result.Image)
		}
		if len(result.Offers) != 1 {
			t.Errorf("expected HTML offers to fill gap, got %d", len(result.Offers))
		}
		if len(result.Exhibitions) != 2 {
			t.Errorf("expected 2 exhibitions (merged), got %d", len(result.Exhibitions))
		}
	})

	t.Run("deduplicates exhibitions by name", func(t *testing.T) {
		jsonLD := &ParsedWebData{
			Exhibitions: []Exhibition{{Name: "Same Exhibition"}},
		}
		html := &ParsedWebData{
			Exhibitions: []Exhibition{
				{Name: "Same Exhibition"},
				{Name: "Different Exhibition"},
			},
		}
		result := mergeWebData(jsonLD, html)
		if len(result.Exhibitions) != 2 {
			t.Errorf("expected 2 exhibitions (deduped), got %d", len(result.Exhibitions))
		}
	})
}
