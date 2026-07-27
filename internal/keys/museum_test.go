package keys

import (
	"strings"
	"testing"

	"museum/internal/models"
)

func TestMuseumKeyIsASCII(t *testing.T) {
	tests := []struct {
		name    string
		museum  models.Museum
		want    string
		comment string
	}{
		{
			name:   "accents folded",
			museum: models.Museum{Name: "Musée de l'Armée", Country: "France"},
			want:   "raw_data/france/musee-de-l-armee.json",
		},
		{
			name:   "transliterated letters",
			museum: models.Museum{Name: "Røros Museum", Country: "Norway"},
			want:   "raw_data/norway/roros-museum.json",
		},
		{
			name:   "punctuation separates",
			museum: models.Museum{Name: "M+", Country: "China"},
			want:   "raw_data/china/m.json",
		},
		{
			name:   "empty country",
			museum: models.Museum{Name: "Unplaced Museum"},
			want:   "raw_data/unknown/unplaced-museum.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Museum(tt.museum); got != tt.want {
				t.Errorf("Museum() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A name written entirely in a script ASCII cannot carry must still produce a
// key, and two different such names must not share one.
func TestMuseumKeyNonLatinScript(t *testing.T) {
	first := Museum(models.Museum{Name: "故宮博物院", Country: "China"})
	second := Museum(models.Museum{Name: "上海博物館", Country: "China"})

	for _, key := range []string{first, second} {
		if !strings.Contains(key, "/x-") {
			t.Errorf("key %q should fall back to a digest", key)
		}
		for _, r := range key {
			if r > 127 {
				t.Errorf("key %q contains non-ASCII rune %q", key, r)
			}
		}
	}
	if first == second {
		t.Errorf("distinct names collided on %q", first)
	}
}

func TestEnrichedMuseumKeyMirrorsRaw(t *testing.T) {
	m := models.Museum{Name: "Rijksmuseum", Country: "Netherlands"}

	raw := Museum(m)
	enriched := EnrichedMuseum(models.EnrichedMuseum{Museum: m})

	if want := strings.Replace(raw, RawPrefix, EnrichedPrefix, 1); enriched != want {
		t.Errorf("EnrichedMuseum() = %q, want %q", enriched, want)
	}
}
