package search

import (
	"slices"
	"sync"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// The point of the package: a search typed without accents, in any
		// case, with any punctuation, must reach the record.
		{name: "accents folded", in: "Musée d'Orsay", want: "musee d orsay"},
		{name: "already plain", in: "MUSEE D ORSAY", want: "musee d orsay"},
		{name: "umlaut", in: "Kunstmuseum Zürich", want: "kunstmuseum zurich"},
		{name: "cedilla and tilde", in: "Museu de São Paulo", want: "museu de sao paulo"},
		// These four have no combining mark to strip, so decomposition leaves
		// them untouched and only transliteration reaches them.
		{name: "polish stroke", in: "Muzeum Łazienki", want: "muzeum lazienki"},
		{name: "maltese", in: "Ħal Saflieni", want: "hal saflieni"},
		{name: "nordic", in: "Ørsted Æther Museum", want: "orsted aether museum"},
		{name: "eszett", in: "Straßenmuseum", want: "strassenmuseum"},

		{name: "punctuation becomes separators", in: "Beckford's Tower & Museum", want: "beckford s tower museum"},
		{name: "symbol name", in: "M+", want: "m"},
		{name: "digits kept", in: "70.8", want: "70 8"},
		{name: "pipe separator", in: "Kunstmuseum Winterthur | Beim Stadthaus", want: "kunstmuseum winterthur beim stadthaus"},
		{name: "whitespace collapsed", in: "  Museum   of   Art  ", want: "museum of art"},
		{name: "empty", in: "", want: ""},
		// Scripts with no meaningful transliteration are left alone so a reader
		// typing them still matches.
		{name: "cyrillic preserved", in: "Эрмитаж", want: "эрмитаж"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTokens_Deduplicates(t *testing.T) {
	got := Tokens("Museum of the Museum Museum")
	want := []string{"museum", "of", "the"}

	if !slices.Equal(got, want) {
		t.Errorf("Tokens = %v, want %v", got, want)
	}
}

// TestNormalize_Concurrent is a regression test for a crash, not a nicety.
//
// The transformer that folds accents carries internal state. A single shared
// instance passed every sequential test and then panicked in the API the first
// time two requests normalised at once.
func TestNormalize_Concurrent(t *testing.T) {
	inputs := []string{
		"Musée d'Orsay", "Kunstmuseum Zürich", "Museu de São Paulo",
		"Muzeum Łazienki", "Ħal Saflieni", "Straßenmuseum", "Эрмитаж",
		"Louvre", "M+", "70.8",
	}
	want := make([]string, len(inputs))
	for i, in := range inputs {
		want[i] = Normalize(in)
	}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				for i, in := range inputs {
					if got := Normalize(in); got != want[i] {
						t.Errorf("Normalize(%q) = %q under concurrency, want %q", in, got, want[i])
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
