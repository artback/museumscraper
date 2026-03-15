package scraper

import (
	"testing"
)

func TestExtractFromHTML_MetaTags(t *testing.T) {
	html := `<html><head>
	<meta property="og:description" content="The Metropolitan Museum of Art">
	<meta property="og:image" content="https://met.example.com/image.jpg">
	<meta property="og:url" content="https://met.example.com">
	</head><body><p>Welcome to the museum</p></body></html>`

	data := ExtractFromHTML(html)
	if data == nil {
		t.Fatal("expected non-nil ParsedWebData")
	}
	if data.Description != "The Metropolitan Museum of Art" {
		t.Errorf("Description = %q", data.Description)
	}
	if data.Image != "https://met.example.com/image.jpg" {
		t.Errorf("Image = %q", data.Image)
	}
}

func TestExtractFromHTML_Prices(t *testing.T) {
	tests := []struct {
		name      string
		html      string
		wantCount int
		wantFirst string
	}{
		{
			name:      "dollar price",
			html:      `<p>Admission: $25 for adults</p>`,
			wantCount: 1,
			wantFirst: "$25",
		},
		{
			name:      "euro price",
			html:      `<p>Entry fee: €17.50</p>`,
			wantCount: 1,
			wantFirst: "€17.50",
		},
		{
			name:      "price with currency code",
			html:      `<p>Tickets: 15 EUR per person</p>`,
			wantCount: 1,
			wantFirst: "15 EUR",
		},
		{
			name:      "free admission",
			html:      `<p>Free admission on Sundays</p>`,
			wantCount: 1,
			wantFirst: "0",
		},
		{
			name:      "admission is free",
			html:      `<p>Admission is free for all visitors</p>`,
			wantCount: 1,
			wantFirst: "0",
		},
		{
			name:      "multiple prices",
			html:      `<p>Adults: $25, Students: $15, Children: $10</p>`,
			wantCount: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := ExtractFromHTML(`<html><body>` + tt.html + `</body></html>`)
			if data == nil {
				t.Fatal("expected non-nil ParsedWebData")
			}
			if len(data.Offers) != tt.wantCount {
				t.Fatalf("expected %d offers, got %d: %+v", tt.wantCount, len(data.Offers), data.Offers)
			}
			if tt.wantFirst != "" && data.Offers[0].Price != tt.wantFirst {
				t.Errorf("first offer price = %q, want %q", data.Offers[0].Price, tt.wantFirst)
			}
		})
	}
}

func TestExtractFromHTML_Hours(t *testing.T) {
	tests := []struct {
		name     string
		html     string
		wantHas  bool
	}{
		{
			name:    "standard format",
			html:    `<p>Monday-Friday: 10:00-17:00</p>`,
			wantHas: true,
		},
		{
			name:    "AM/PM format",
			html:    `<p>Open 10am-5pm daily</p>`,
			wantHas: true,
		},
		{
			name:    "no hours",
			html:    `<p>Welcome to our museum!</p>`,
			wantHas: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := ExtractFromHTML(`<html><body>` + tt.html + `</body></html>`)
			hasHours := data != nil && data.OpeningHours != ""
			if hasHours != tt.wantHas {
				hours := ""
				if data != nil {
					hours = data.OpeningHours
				}
				t.Errorf("hasHours = %v, want %v (hours=%q)", hasHours, tt.wantHas, hours)
			}
		})
	}
}

func TestExtractFromHTML_ExhibitionHeaders(t *testing.T) {
	html := `<html><body>
	<h2>Current Exhibitions</h2>
	<div>
		<a href="/vermeer">Vermeer: Master of Light</a>
		<a href="/monet">Monet's Garden</a>
	</div>
	<h2>About Us</h2>
	<p>We are a museum</p>
	</body></html>`

	data := ExtractFromHTML(html)
	if data == nil {
		t.Fatal("expected non-nil ParsedWebData")
	}
	if len(data.Exhibitions) < 2 {
		t.Fatalf("expected at least 2 exhibitions, got %d", len(data.Exhibitions))
	}

	names := make(map[string]bool)
	for _, ex := range data.Exhibitions {
		names[ex.Name] = true
	}
	if !names["Vermeer: Master of Light"] {
		t.Error("missing exhibition 'Vermeer: Master of Light'")
	}
	if !names["Monet's Garden"] {
		t.Error("missing exhibition 'Monet's Garden'")
	}
}

func TestExtractFromHTML_FiltersNavLinks(t *testing.T) {
	html := `<html><body>
	<h2>Exhibitions</h2>
	<div>
		<a href="/exhibit">Real Exhibition Name</a>
		<a href="/more">Read more</a>
		<a href="/tickets">Buy tickets</a>
	</div>
	</body></html>`

	data := ExtractFromHTML(html)
	if data == nil {
		t.Fatal("expected non-nil ParsedWebData")
	}
	for _, ex := range data.Exhibitions {
		if ex.Name == "Read more" || ex.Name == "Buy tickets" {
			t.Errorf("should have filtered out nav link: %q", ex.Name)
		}
	}
}

func TestStripTags(t *testing.T) {
	html := `<p>Hello <strong>world</strong>!</p><br><span>Test</span>`
	text := stripTags(html)
	if text == "" {
		t.Fatal("expected non-empty text")
	}
	if !contains(text, "Hello") || !contains(text, "world") || !contains(text, "Test") {
		t.Errorf("stripTags result = %q", text)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && searchString(s, sub)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
