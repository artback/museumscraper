package scraper

import "testing"

func TestMergeWebData(t *testing.T) {
	t.Run("both nil", func(t *testing.T) {
		result := mergeWebData(nil, nil)
		if result != nil {
			t.Error("expected nil")
		}
	})

	t.Run("only primary", func(t *testing.T) {
		primary := &ParsedWebData{Description: "from primary"}
		result := mergeWebData(primary, nil)
		if result.Description != "from primary" {
			t.Errorf("Description = %q", result.Description)
		}
	})

	t.Run("only secondary", func(t *testing.T) {
		secondary := &ParsedWebData{Description: "from secondary"}
		result := mergeWebData(nil, secondary)
		if result.Description != "from secondary" {
			t.Errorf("Description = %q", result.Description)
		}
	})

	t.Run("primary takes precedence", func(t *testing.T) {
		primary := &ParsedWebData{
			Description:  "primary desc",
			Exhibitions:  []Exhibition{{Name: "Ex1"}},
			OpeningHours: "Mo-Su 10:00-17:00",
		}
		secondary := &ParsedWebData{
			Description: "secondary desc",
			Image:       "image.jpg",
			Offers:      []Offer{{Price: "15", Name: "Adult"}},
			Exhibitions: []Exhibition{{Name: "Ex2"}},
		}
		result := mergeWebData(primary, secondary)
		if result.Description != "primary desc" {
			t.Errorf("Description = %q, want primary desc", result.Description)
		}
		if result.Image != "image.jpg" {
			t.Errorf("Image not filled from secondary: %q", result.Image)
		}
		if len(result.Offers) != 1 {
			t.Errorf("expected secondary offers to fill gap, got %d", len(result.Offers))
		}
		if len(result.Exhibitions) != 2 {
			t.Errorf("expected 2 exhibitions (merged), got %d", len(result.Exhibitions))
		}
	})

	t.Run("deduplicates exhibitions by name", func(t *testing.T) {
		primary := &ParsedWebData{
			Exhibitions: []Exhibition{{Name: "Same Exhibition"}},
		}
		secondary := &ParsedWebData{
			Exhibitions: []Exhibition{
				{Name: "Same Exhibition"},
				{Name: "Different Exhibition"},
			},
		}
		result := mergeWebData(primary, secondary)
		if len(result.Exhibitions) != 2 {
			t.Errorf("expected 2 exhibitions (deduped), got %d", len(result.Exhibitions))
		}
	})

	t.Run("deduplicates offers by price+name", func(t *testing.T) {
		primary := &ParsedWebData{
			Offers: []Offer{{Price: "15", Name: "Adult"}},
		}
		secondary := &ParsedWebData{
			Offers: []Offer{
				{Price: "15", Name: "Adult"},  // duplicate
				{Price: "10", Name: "Student"}, // new
			},
		}
		result := mergeWebData(primary, secondary)
		if len(result.Offers) != 2 {
			t.Errorf("expected 2 offers (deduped), got %d", len(result.Offers))
		}
	})
}

func TestExtractPage(t *testing.T) {
	html := `<html><head>
	<meta property="og:description" content="A great museum">
	<script type="application/ld+json">
	{"@type":"ExhibitionEvent","name":"Test Show","startDate":"2024-01-01"}
	</script>
	</head><body>
	<p>Admission: $25</p>
	</body></html>`

	data := extractPage(html)
	if data == nil {
		t.Fatal("expected non-nil")
	}
	if len(data.Exhibitions) == 0 {
		t.Error("expected exhibitions from JSON-LD")
	}
	if data.Description != "A great museum" {
		t.Errorf("Description = %q", data.Description)
	}
	if len(data.Offers) == 0 {
		t.Error("expected offers from text extraction")
	}
}

func TestExtractFromPlainText(t *testing.T) {
	t.Run("prices", func(t *testing.T) {
		text := "Adults: $25.00  Students: $15  Children under 12: free admission"
		data := extractFromPlainText(text)
		if data == nil {
			t.Fatal("expected non-nil")
		}
		if len(data.Offers) < 3 {
			t.Errorf("expected at least 3 offers, got %d", len(data.Offers))
		}
	})

	t.Run("hours", func(t *testing.T) {
		text := "We are open Monday-Saturday: 10:00-17:00"
		data := extractFromPlainText(text)
		if data == nil {
			t.Fatal("expected non-nil")
		}
		if data.OpeningHours == "" {
			t.Error("expected opening hours")
		}
	})

	t.Run("empty text", func(t *testing.T) {
		data := extractFromPlainText("")
		if data != nil {
			t.Error("expected nil for empty text")
		}
	})

	t.Run("no matches", func(t *testing.T) {
		data := extractFromPlainText("Welcome to our wonderful institution.")
		if data != nil {
			t.Error("expected nil when no patterns match")
		}
	})
}
