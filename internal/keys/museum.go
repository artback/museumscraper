// Package keys derives the S3 object keys used across the pipeline, so the
// parser and the enricher cannot drift apart on naming.
package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"museum/internal/models"
	"museum/internal/search"
)

// Prefixes under which the two pipeline stages write.
//
// RawPrefix is the one MinIO's bucket notification watches; the enricher writes
// under EnrichedPrefix precisely so that its own writes do not trigger another
// round of events.
const (
	RawPrefix      = "raw_data"
	EnrichedPrefix = "enriched_data"
)

// sanitizeKey reduces s to a lowercase, dash-separated, pure-ASCII slug.
//
// The catalogue is worldwide, so names arrive accented ("Musée de l'Armée"),
// in Cyrillic, and in Han script. A key may legally hold any of them — S3
// stores raw bytes — but the further such a key travels the more likely
// something along the way mangles it: URL encoding, console listings, log
// pipelines, and clients that reject "unsupported characters" all treat a
// percent-escaped key differently from a plain one. Folding to ASCII once,
// here, keeps every consumer downstream working with the same bytes.
//
// Normalize does the folding: accents are decomposed and dropped, letters that
// are their own base (ø, ł, ß) are transliterated, and punctuation becomes a
// separator — the same rules the search index uses, so a key and a search term
// derive from the same text in the same way.
func sanitizeKey(s string) string {
	normalized := search.Normalize(s)

	var b strings.Builder
	b.Grow(len(normalized))
	for _, r := range normalized {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		default:
			// A script ASCII cannot carry: Cyrillic, Han, Arabic. Dropping
			// those runes would collide every such name onto one key, so a
			// name made only of them falls through to the digest below.
		}
	}

	if slug := strings.Trim(b.String(), "-"); slug != "" {
		return slug
	}
	return digest(s)
}

// digest returns a short, stable identifier for text ASCII cannot represent.
// Opaque, but unique, which is the property a key actually needs.
func digest(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	sum := sha256.Sum256([]byte(s))
	return "x-" + hex.EncodeToString(sum[:8])
}

// Museum returns the S3 key for a raw museum record.
func Museum(m models.Museum) string {
	return RawPrefix + "/" + sanitizeKey(m.Country) + "/" + sanitizeKey(m.Name) + ".json"
}

// EnrichedMuseum returns the S3 key for an enriched museum record.
func EnrichedMuseum(m models.EnrichedMuseum) string {
	return EnrichedPrefix + "/" + sanitizeKey(m.Museum.Country) + "/" + sanitizeKey(m.Museum.Name) + ".json"
}
