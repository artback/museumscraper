// Package keys derives the S3 object keys used across the pipeline, so the
// parser and the enricher cannot drift apart on naming.
package keys

import (
	"fmt"
	"regexp"
	"strings"

	"museum/internal/models"
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

// unsafeKeyChars matches everything that is not a letter, digit or dash. Keys
// keep non-ASCII letters (museum names are frequently accented) but drop
// punctuation that complicates tooling further down the line.
var unsafeKeyChars = regexp.MustCompile(`[^\p{L}\p{N}-]+`)

// sanitizeKey lowercases s and reduces it to a dash-separated slug.
func sanitizeKey(s string) string {
	slug := unsafeKeyChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "unknown"
	}
	return slug
}

// Museum returns the S3 key for a raw museum record.
func Museum(m models.Museum) string {
	return fmt.Sprintf("%s/%s/%s.json", RawPrefix, sanitizeKey(m.Country), sanitizeKey(m.Name))
}

// EnrichedMuseum returns the S3 key for an enriched museum record.
func EnrichedMuseum(m models.EnrichedMuseum) string {
	return fmt.Sprintf("%s/%s/%s.json", EnrichedPrefix, sanitizeKey(m.Museum.Country), sanitizeKey(m.Museum.Name))
}
