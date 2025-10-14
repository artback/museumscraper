package keys

import (
	"fmt"
	"museum/internal/models"
	"strings"
)

// sanitizeKey replaces spaces with hyphens and lowercases the string.
// You could expand this to strip other characters if needed.
func sanitizeKey(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, " ", "-"))
}

// Museum returns the canonical S3 key for a Museum object.
func Museum(m models.Museum) string {
	return fmt.Sprintf("raw_data/%s/%s.json",
		sanitizeKey(m.Country),
		sanitizeKey(m.Name),
	)
}
