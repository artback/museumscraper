package models

import (
	"fmt"
	"strings"
)

type Museum struct {
	Country string
	Name    string
}

func (m Museum) StorageKey() string {
	return fmt.Sprintf("raw_data/%s/%s.json", sanitizeKey(m.Country), sanitizeKey(m.Name))
}

func sanitizeKey(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, " ", "-"))
}
