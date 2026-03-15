package scraper

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// jsonLDPattern matches <script type="application/ld+json">...</script> blocks.
var jsonLDPattern = regexp.MustCompile(`(?si)<script[^>]*type\s*=\s*["']application/ld\+json["'][^>]*>(.*?)</script>`)

// SchemaEvent represents a Schema.org Event or ExhibitionEvent.
type SchemaEvent struct {
	Type        string `json:"@type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	StartDate   string `json:"startDate"`
	EndDate     string `json:"endDate"`
	URL         string `json:"url"`
	Image       string `json:"image"`
	Location    any    `json:"location"`
	Offers      any    `json:"offers"`
}

// SchemaOffer represents a Schema.org Offer (ticket pricing).
type SchemaOffer struct {
	Type         string `json:"@type"`
	Price        string `json:"price"`
	PriceCurr    string `json:"priceCurrency"`
	URL          string `json:"url"`
	Availability string `json:"availability"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	ValidFrom    string `json:"validFrom"`
}

// ParsedWebData holds all structured data extracted from a museum website.
type ParsedWebData struct {
	Exhibitions []Exhibition `json:"exhibitions,omitempty"`
	Offers      []Offer      `json:"offers,omitempty"`
	OpeningHours string      `json:"opening_hours_web,omitempty"`
	Description  string      `json:"description_web,omitempty"`
	Image        string      `json:"image_web,omitempty"`
	URL          string      `json:"url,omitempty"`
}

// Exhibition represents a parsed exhibition with all available metadata.
type Exhibition struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	StartDate   string  `json:"start_date,omitempty"`
	EndDate     string  `json:"end_date,omitempty"`
	URL         string  `json:"url,omitempty"`
	Image       string  `json:"image,omitempty"`
	Prices      []Offer `json:"prices,omitempty"`
}

// Offer represents pricing information.
type Offer struct {
	Name     string `json:"name,omitempty"`
	Price    string `json:"price"`
	Currency string `json:"currency,omitempty"`
	URL      string `json:"url,omitempty"`
}

// ExtractJSONLD parses all JSON-LD blocks from HTML and extracts
// museum-relevant structured data (events, exhibitions, offers, museum info).
func ExtractJSONLD(html string) *ParsedWebData {
	matches := jsonLDPattern.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		return nil
	}

	data := &ParsedWebData{}
	for _, m := range matches {
		raw := strings.TrimSpace(m[1])
		if raw == "" {
			continue
		}
		parseJSONLDBlock(raw, data)
	}

	if len(data.Exhibitions) == 0 && data.Description == "" && len(data.Offers) == 0 && data.OpeningHours == "" {
		return nil
	}
	return data
}

func parseJSONLDBlock(raw string, data *ParsedWebData) {
	// Try as single object first
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err == nil {
		processJSONLDObject(obj, data)
		return
	}

	// Try as array
	var arr []map[string]any
	if err := json.Unmarshal([]byte(raw), &arr); err == nil {
		for _, item := range arr {
			processJSONLDObject(item, data)
		}
		return
	}
}

func processJSONLDObject(obj map[string]any, data *ParsedWebData) {
	typ, _ := obj["@type"].(string)

	// Handle @graph arrays (common pattern)
	if graph, ok := obj["@graph"].([]any); ok {
		for _, item := range graph {
			if m, ok := item.(map[string]any); ok {
				processJSONLDObject(m, data)
			}
		}
		return
	}

	switch typ {
	case "ExhibitionEvent", "Event", "VisualArtsEvent", "TheaterEvent", "ScreeningEvent":
		ex := extractExhibition(obj)
		if ex.Name != "" {
			data.Exhibitions = append(data.Exhibitions, ex)
		}

	case "Museum", "ArtGallery", "TouristAttraction", "LocalBusiness", "Place":
		if desc, ok := obj["description"].(string); ok && data.Description == "" {
			data.Description = desc
		}
		if img := extractImage(obj); img != "" && data.Image == "" {
			data.Image = img
		}
		if hours, ok := obj["openingHours"].(string); ok && data.OpeningHours == "" {
			data.OpeningHours = hours
		}
		// openingHoursSpecification may be an array
		if specs, ok := obj["openingHoursSpecification"].([]any); ok && data.OpeningHours == "" {
			data.OpeningHours = formatOpeningHoursSpec(specs)
		}
		if url, ok := obj["url"].(string); ok && data.URL == "" {
			data.URL = url
		}
		// Extract events nested under the museum
		if events, ok := obj["event"].([]any); ok {
			for _, ev := range events {
				if em, ok := ev.(map[string]any); ok {
					ex := extractExhibition(em)
					if ex.Name != "" {
						data.Exhibitions = append(data.Exhibitions, ex)
					}
				}
			}
		}
		// Extract offers nested under the museum
		extractOffers(obj, data)

	case "Offer", "AggregateOffer":
		offer := extractOffer(obj)
		if offer.Price != "" {
			data.Offers = append(data.Offers, offer)
		}

	case "ItemList", "CollectionPage":
		// Some museums list exhibitions as an ItemList
		if items, ok := obj["itemListElement"].([]any); ok {
			for _, item := range items {
				if m, ok := item.(map[string]any); ok {
					if nested, ok := m["item"].(map[string]any); ok {
						processJSONLDObject(nested, data)
					} else {
						processJSONLDObject(m, data)
					}
				}
			}
		}
	}
}

func extractExhibition(obj map[string]any) Exhibition {
	ex := Exhibition{
		Name:        getString(obj, "name"),
		Description: getString(obj, "description"),
		StartDate:   getString(obj, "startDate"),
		EndDate:     getString(obj, "endDate"),
		URL:         getString(obj, "url"),
		Image:       extractImage(obj),
	}

	// Extract prices from nested offers
	if offers, ok := obj["offers"].([]any); ok {
		for _, o := range offers {
			if om, ok := o.(map[string]any); ok {
				offer := extractOffer(om)
				if offer.Price != "" {
					ex.Prices = append(ex.Prices, offer)
				}
			}
		}
	} else if om, ok := obj["offers"].(map[string]any); ok {
		offer := extractOffer(om)
		if offer.Price != "" {
			ex.Prices = append(ex.Prices, offer)
		}
	}

	return ex
}

func extractOffer(obj map[string]any) Offer {
	price := getString(obj, "price")
	if price == "" {
		if p, ok := obj["price"].(float64); ok {
			if p == 0 {
				price = "0"
			} else {
				price = strings.TrimRight(strings.TrimRight(
					strings.Replace(fmt.Sprintf("%.2f", p), ".", ".", 1), "0"), ".")
			}
		}
	}
	return Offer{
		Name:     getString(obj, "name"),
		Price:    price,
		Currency: getString(obj, "priceCurrency"),
		URL:      getString(obj, "url"),
	}
}

func extractOffers(obj map[string]any, data *ParsedWebData) {
	if offers, ok := obj["offers"].([]any); ok {
		for _, o := range offers {
			if om, ok := o.(map[string]any); ok {
				offer := extractOffer(om)
				if offer.Price != "" {
					data.Offers = append(data.Offers, offer)
				}
			}
		}
	} else if om, ok := obj["offers"].(map[string]any); ok {
		offer := extractOffer(om)
		if offer.Price != "" {
			data.Offers = append(data.Offers, offer)
		}
	}
}

func extractImage(obj map[string]any) string {
	switch v := obj["image"].(type) {
	case string:
		return v
	case map[string]any:
		if url, ok := v["url"].(string); ok {
			return url
		}
	case []any:
		if len(v) > 0 {
			if s, ok := v[0].(string); ok {
				return s
			}
			if m, ok := v[0].(map[string]any); ok {
				if url, ok := m["url"].(string); ok {
					return url
				}
			}
		}
	}
	return ""
}

func formatOpeningHoursSpec(specs []any) string {
	var parts []string
	for _, spec := range specs {
		if m, ok := spec.(map[string]any); ok {
			day := ""
			if d, ok := m["dayOfWeek"].(string); ok {
				day = d
			}
			opens := getString(m, "opens")
			closes := getString(m, "closes")
			if day != "" && opens != "" {
				parts = append(parts, fmt.Sprintf("%s %s-%s", day, opens, closes))
			}
		}
	}
	return strings.Join(parts, "; ")
}

func getString(obj map[string]any, key string) string {
	if v, ok := obj[key].(string); ok {
		return v
	}
	return ""
}

