// Package search normalises text so names can be compared and matched.
//
// The database does the searching — trigram similarity for near-misses, prefix
// and exact matching for correctly typed queries — but it can only compare what
// it is given. A catalogue drawn from Wikidata, Wikipedia and OpenStreetMap
// holds names in dozens of scripts and spellings, and someone searching for
// "musee d orsay" or "kunstmuseum zurich" expects to reach "Musée d'Orsay" and
// "Kunstmuseum Zürich". Both the stored form and the query go through here, so
// the two meet.
package search

import (
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// transliterations map letters that Unicode decomposition leaves alone.
//
// NFD splits a letter into a base and a combining mark, which is how "é"
// becomes "e". Letters that are their own base — Maltese Ħ, Polish Ł, Nordic Ø
// and Æ, Icelandic Þ — have nothing to strip, so they survive normalisation
// unchanged and a search typed on an English keyboard never reaches them.
var transliterations = map[rune]string{
	'ħ': "h", 'ł': "l", 'ø': "o", 'æ': "ae", 'œ': "oe",
	'ß': "ss", 'đ': "d", 'ð': "d", 'þ': "th", 'ı': "i",
	'ŧ': "t", 'ĸ': "k", 'ŋ': "n", 'ə': "e",
	// Cyrillic and Greek are left as they are: transliterating them well needs
	// language context, and a reader searching in those scripts types them.
}

// folders hands out transformers that strip the combining marks decomposition
// exposes.
//
// A transform.Transformer carries internal state and is not safe for concurrent
// use. Sharing one package-level instance worked in every test and every
// sequential benchmark, then panicked in the API under concurrent requests —
// two goroutines transforming at once corrupt the same buffer. Pooling gives
// each caller its own while still reusing the allocation.
var folders = sync.Pool{
	New: func() any {
		return transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	},
}

// Normalize reduces text to its comparable form: lowercase, without accents,
// with punctuation turned into separators.
//
// "Musée d'Orsay" and "MUSEE D ORSAY" both become "musee d orsay", so a query
// matches a record however either was typed.
func Normalize(s string) string {
	folder := folders.Get().(transform.Transformer)
	folder.Reset()
	folded, _, err := transform.String(folder, s)
	folders.Put(folder)
	if err != nil {
		// Transformation only fails on malformed input; the original is still
		// better than nothing.
		folded = s
	}

	var b strings.Builder
	b.Grow(len(folded))

	for _, r := range strings.ToLower(folded) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if replacement, ok := transliterations[r]; ok {
				b.WriteString(replacement)
				continue
			}
			b.WriteRune(r)
		default:
			// Punctuation, symbols and whitespace all separate words. "M+"
			// becomes "m", which is what someone typing it would search for.
			b.WriteRune(' ')
		}
	}

	return strings.Join(strings.Fields(b.String()), " ")
}

// Tokens splits text into its distinct terms, in order of first appearance.
func Tokens(s string) []string {
	fields := strings.Fields(Normalize(s))

	tokens := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if _, dup := seen[field]; dup {
			continue
		}
		seen[field] = struct{}{}
		tokens = append(tokens, field)
	}
	return tokens
}
