package scraper

import (
	"testing"
)

func TestExtractJSONLD_Museum(t *testing.T) {
	html := `<html><head>
	<script type="application/ld+json">
	{
		"@context": "https://schema.org",
		"@type": "Museum",
		"name": "Louvre Museum",
		"description": "The world's largest art museum",
		"url": "https://www.louvre.fr",
		"image": "https://www.louvre.fr/image.jpg",
		"openingHours": "Mo-Su 09:00-18:00",
		"offers": {
			"@type": "Offer",
			"price": "17",
			"priceCurrency": "EUR",
			"name": "General Admission"
		}
	}
	</script>
	</head><body></body></html>`

	data := ExtractJSONLD(html)
	if data == nil {
		t.Fatal("expected non-nil ParsedWebData")
	}
	if data.Description != "The world's largest art museum" {
		t.Errorf("Description = %q", data.Description)
	}
	if data.OpeningHours != "Mo-Su 09:00-18:00" {
		t.Errorf("OpeningHours = %q", data.OpeningHours)
	}
	if data.Image != "https://www.louvre.fr/image.jpg" {
		t.Errorf("Image = %q", data.Image)
	}
	if len(data.Offers) != 1 {
		t.Fatalf("expected 1 offer, got %d", len(data.Offers))
	}
	if data.Offers[0].Price != "17" {
		t.Errorf("Offer.Price = %q", data.Offers[0].Price)
	}
	if data.Offers[0].Currency != "EUR" {
		t.Errorf("Offer.Currency = %q", data.Offers[0].Currency)
	}
}

func TestExtractJSONLD_ExhibitionEvent(t *testing.T) {
	html := `<html><head>
	<script type="application/ld+json">
	[
		{
			"@context": "https://schema.org",
			"@type": "ExhibitionEvent",
			"name": "Vermeer and the Masters",
			"description": "A stunning exhibition of Dutch masters",
			"startDate": "2024-02-01",
			"endDate": "2024-06-15",
			"url": "https://museum.example.com/vermeer",
			"image": "https://museum.example.com/vermeer.jpg",
			"offers": [
				{
					"@type": "Offer",
					"price": "25",
					"priceCurrency": "EUR",
					"name": "Adult"
				},
				{
					"@type": "Offer",
					"price": "15",
					"priceCurrency": "EUR",
					"name": "Student"
				}
			]
		},
		{
			"@context": "https://schema.org",
			"@type": "ExhibitionEvent",
			"name": "Modern Impressions",
			"startDate": "2024-03-01"
		}
	]
	</script>
	</head><body></body></html>`

	data := ExtractJSONLD(html)
	if data == nil {
		t.Fatal("expected non-nil ParsedWebData")
	}
	if len(data.Exhibitions) != 2 {
		t.Fatalf("expected 2 exhibitions, got %d", len(data.Exhibitions))
	}

	ex := data.Exhibitions[0]
	if ex.Name != "Vermeer and the Masters" {
		t.Errorf("Name = %q", ex.Name)
	}
	if ex.StartDate != "2024-02-01" {
		t.Errorf("StartDate = %q", ex.StartDate)
	}
	if ex.EndDate != "2024-06-15" {
		t.Errorf("EndDate = %q", ex.EndDate)
	}
	if len(ex.Prices) != 2 {
		t.Fatalf("expected 2 prices, got %d", len(ex.Prices))
	}
	if ex.Prices[0].Price != "25" || ex.Prices[0].Name != "Adult" {
		t.Errorf("Price[0] = %+v", ex.Prices[0])
	}
	if ex.Prices[1].Price != "15" || ex.Prices[1].Name != "Student" {
		t.Errorf("Price[1] = %+v", ex.Prices[1])
	}
}

func TestExtractJSONLD_Graph(t *testing.T) {
	html := `<html><head>
	<script type="application/ld+json">
	{
		"@context": "https://schema.org",
		"@graph": [
			{
				"@type": "Museum",
				"name": "Test Museum",
				"description": "A test museum",
				"openingHours": "Tu-Su 10:00-17:00"
			},
			{
				"@type": "ExhibitionEvent",
				"name": "Test Exhibition",
				"startDate": "2024-01-01"
			}
		]
	}
	</script>
	</head><body></body></html>`

	data := ExtractJSONLD(html)
	if data == nil {
		t.Fatal("expected non-nil ParsedWebData")
	}
	if data.Description != "A test museum" {
		t.Errorf("Description = %q", data.Description)
	}
	if data.OpeningHours != "Tu-Su 10:00-17:00" {
		t.Errorf("OpeningHours = %q", data.OpeningHours)
	}
	if len(data.Exhibitions) != 1 {
		t.Fatalf("expected 1 exhibition, got %d", len(data.Exhibitions))
	}
	if data.Exhibitions[0].Name != "Test Exhibition" {
		t.Errorf("Exhibition.Name = %q", data.Exhibitions[0].Name)
	}
}

func TestExtractJSONLD_MultipleScripts(t *testing.T) {
	html := `<html><head>
	<script type="application/ld+json">{"@type":"Museum","description":"Museum desc"}</script>
	<script type="application/ld+json">{"@type":"ExhibitionEvent","name":"Show A","startDate":"2024-05-01"}</script>
	</head><body></body></html>`

	data := ExtractJSONLD(html)
	if data == nil {
		t.Fatal("expected non-nil ParsedWebData")
	}
	if data.Description != "Museum desc" {
		t.Errorf("Description = %q", data.Description)
	}
	if len(data.Exhibitions) != 1 || data.Exhibitions[0].Name != "Show A" {
		t.Errorf("Exhibitions = %+v", data.Exhibitions)
	}
}

func TestExtractJSONLD_NoJSONLD(t *testing.T) {
	html := `<html><head><title>Museum</title></head><body>No JSON-LD here</body></html>`
	data := ExtractJSONLD(html)
	if data != nil {
		t.Errorf("expected nil for page without JSON-LD, got %+v", data)
	}
}

func TestExtractJSONLD_MuseumWithNestedEvents(t *testing.T) {
	html := `<html><head>
	<script type="application/ld+json">
	{
		"@type": "Museum",
		"name": "National Gallery",
		"description": "Art museum",
		"event": [
			{"@type": "ExhibitionEvent", "name": "Monet Exhibition", "startDate": "2024-06-01"},
			{"@type": "ExhibitionEvent", "name": "Picasso Retrospective", "startDate": "2024-09-01"}
		]
	}
	</script>
	</head><body></body></html>`

	data := ExtractJSONLD(html)
	if data == nil {
		t.Fatal("expected non-nil ParsedWebData")
	}
	if len(data.Exhibitions) != 2 {
		t.Fatalf("expected 2 exhibitions, got %d", len(data.Exhibitions))
	}
	if data.Exhibitions[0].Name != "Monet Exhibition" {
		t.Errorf("Exhibition[0].Name = %q", data.Exhibitions[0].Name)
	}
}

func TestExtractJSONLD_FloatPrice(t *testing.T) {
	html := `<html><head>
	<script type="application/ld+json">
	{
		"@type": "Museum",
		"name": "Test",
		"offers": {"@type": "Offer", "price": 15.50, "priceCurrency": "USD"}
	}
	</script>
	</head><body></body></html>`

	data := ExtractJSONLD(html)
	if data == nil {
		t.Fatal("expected non-nil")
	}
	if len(data.Offers) != 1 {
		t.Fatalf("expected 1 offer, got %d", len(data.Offers))
	}
	if data.Offers[0].Price != "15.5" {
		t.Errorf("Price = %q, want 15.5", data.Offers[0].Price)
	}
}
