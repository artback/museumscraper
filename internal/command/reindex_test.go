package command

import (
	"testing"

	"museum/internal/models"
)

func TestMergeEnriched(t *testing.T) {
	cases := []struct {
		name     string
		enriched models.EnrichedMuseum
		want     models.Museum
	}{
		{
			// The whole point of folding enrichment back in: a museum the
			// sources could not place gets its position from the geocoder.
			name: "coordinates fill a gap",
			enriched: models.EnrichedMuseum{
				Museum: models.Museum{Name: "Jihad Museum", Country: "Afghanistan"},
				Data:   map[string]any{"lat": "34.3458129", "lon": "62.1877057"},
			},
			want: models.Museum{Name: "Jihad Museum", Country: "Afghanistan",
				Latitude: 34.3458129, Longitude: 62.1877057},
		},
		{
			// A coordinate stated by Wikidata is better evidence than one
			// inferred from a name search, so it must survive.
			name: "existing coordinates are not overwritten",
			enriched: models.EnrichedMuseum{
				Museum: models.Museum{Name: "Louvre", Latitude: 48.8606, Longitude: 2.3376},
				Data:   map[string]any{"lat": "1.0", "lon": "2.0"},
			},
			want: models.Museum{Name: "Louvre", Latitude: 48.8606, Longitude: 2.3376},
		},
		{
			name: "website and locality fill gaps",
			enriched: models.EnrichedMuseum{
				Museum: models.Museum{Name: "Herat National Museum"},
				Data:   map[string]any{"website": "https://example.org", "locality": "Herat"},
			},
			want: models.Museum{Name: "Herat National Museum",
				Website: "https://example.org", Locality: "Herat"},
		},
		{
			name: "existing website wins",
			enriched: models.EnrichedMuseum{
				Museum: models.Museum{Name: "X", Website: "https://official.example"},
				Data:   map[string]any{"website": "https://osm.example"},
			},
			want: models.Museum{Name: "X", Website: "https://official.example"},
		},
		{
			name: "a half-parsed coordinate is not used",
			enriched: models.EnrichedMuseum{
				Museum: models.Museum{Name: "X"},
				Data:   map[string]any{"lat": "34.3"},
			},
			want: models.Museum{Name: "X"},
		},
		{
			name: "unparseable coordinates are ignored",
			enriched: models.EnrichedMuseum{
				Museum: models.Museum{Name: "X"},
				Data:   map[string]any{"lat": "north", "lon": "west"},
			},
			want: models.Museum{Name: "X"},
		},
		{
			name:     "no enrichment data at all",
			enriched: models.EnrichedMuseum{Museum: models.Museum{Name: "X", Country: "France"}},
			want:     models.Museum{Name: "X", Country: "France"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeEnriched(tc.enriched)

			if got.Name != tc.want.Name || got.Country != tc.want.Country {
				t.Errorf("identity = %q/%q, want %q/%q", got.Name, got.Country, tc.want.Name, tc.want.Country)
			}
			if got.Latitude != tc.want.Latitude || got.Longitude != tc.want.Longitude {
				t.Errorf("coordinates = (%v, %v), want (%v, %v)",
					got.Latitude, got.Longitude, tc.want.Latitude, tc.want.Longitude)
			}
			if got.Website != tc.want.Website {
				t.Errorf("website = %q, want %q", got.Website, tc.want.Website)
			}
			if got.Locality != tc.want.Locality {
				t.Errorf("locality = %q, want %q", got.Locality, tc.want.Locality)
			}
		})
	}
}

func TestFloatFrom(t *testing.T) {
	cases := []struct {
		in   any
		want float64
		ok   bool
	}{
		// Nominatim returns coordinates as strings; everything else numeric
		// arrives as float64 after a JSON round trip.
		{in: "48.8566", want: 48.8566, ok: true},
		{in: " 48.8566 ", want: 48.8566, ok: true},
		{in: "-1.2414", want: -1.2414, ok: true},
		{in: 48.8566, want: 48.8566, ok: true},
		{in: "north", ok: false},
		{in: "", ok: false},
		{in: nil, ok: false},
		{in: []string{"48"}, ok: false},
	}

	for _, tc := range cases {
		got, ok := floatFrom(tc.in)
		if ok != tc.ok {
			t.Errorf("floatFrom(%#v) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("floatFrom(%#v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCommandRegistry(t *testing.T) {
	// Every command must be reachable by the name help advertises, or the
	// binary documents a command it cannot run.
	for _, name := range Names() {
		cmd, ok := Lookup(name)
		if !ok {
			t.Errorf("Lookup(%q) failed for a name from Names()", name)
			continue
		}
		if cmd.Summary == "" {
			t.Errorf("command %q has no summary", name)
		}
		if cmd.Run == nil {
			t.Errorf("command %q has no implementation", name)
		}
	}

	if _, ok := Lookup("nonexistent"); ok {
		t.Error("Lookup accepted an unknown command")
	}
}

func TestRequireNoArgs(t *testing.T) {
	if err := requireNoArgs("serve", nil); err != nil {
		t.Errorf("no arguments should be accepted: %v", err)
	}
	// Silently ignoring stray arguments hides typos like "museum serve :8090",
	// where the user meant -addr.
	if err := requireNoArgs("serve", []string{":8090"}); err == nil {
		t.Error("stray positional arguments should be rejected")
	}
}
