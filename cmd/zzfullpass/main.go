package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"museum/internal/models"
	"museum/internal/search"
	"museum/pkg/overture"
)

type summary struct {
	Release      string         `json:"release"`
	Elapsed      string         `json:"elapsed"`
	Museums      int            `json:"museums"`
	Distinct     int            `json:"distinct"`
	WithWebsite  int            `json:"with_website"`
	WithCoords   int            `json:"with_coords"`
	UnknownCount int            `json:"unknown_country"`
	Countries    map[string]int `json:"countries"`
	Classes      map[string]int `json:"classes"`
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()

	svc := overture.NewService(nil)
	release, err := svc.LatestRelease(ctx)
	if err != nil {
		log.Fatal(err)
	}

	start := time.Now()
	s := summary{
		Release:   release,
		Countries: map[string]int{},
		Classes:   map[string]int{},
	}
	seen := map[string]struct{}{}
	var sample []models.Museum

	last := time.Now()
	for m := range svc.Museums(ctx, release) {
		s.Museums++
		s.Countries[m.Country]++
		for _, c := range m.Classes {
			s.Classes[c]++
		}
		if m.Website != "" {
			s.WithWebsite++
		}
		if m.HasCoordinates() {
			s.WithCoords++
		}
		if m.Country == "unknown" {
			s.UnknownCount++
		}

		key := search.Normalize(m.Name) + "|" + m.Country + "|" + m.Locality
		if _, dup := seen[key]; !dup {
			seen[key] = struct{}{}
			s.Distinct++
		}
		if len(sample) < 20 && m.Website != "" && m.Country != "unknown" {
			sample = append(sample, m)
		}

		if time.Since(last) > 60*time.Second {
			log.Printf("PROGRESS %d museums, %d countries, %s elapsed",
				s.Museums, len(s.Countries), time.Since(start).Round(time.Second))
			last = time.Now()
		}
	}

	s.Elapsed = time.Since(start).Round(time.Second).String()

	out, _ := json.MarshalIndent(s, "", "  ")
	_ = os.WriteFile("/tmp/claude-0/-home-user-museumscraper/f919e5f0-8c0c-520f-8581-0be78298cbfb/scratchpad/overture_full.json", out, 0o644)

	fmt.Printf("\n===== FULL PASS =====\nrelease %s in %s\n", s.Release, s.Elapsed)
	fmt.Printf("%d museums (%d distinct by name+country+locality)\n", s.Museums, s.Distinct)
	fmt.Printf("%d with a website (%.0f%%), %d with coordinates (%.0f%%), %d with no country\n",
		s.WithWebsite, 100*float64(s.WithWebsite)/float64(s.Museums),
		s.WithCoords, 100*float64(s.WithCoords)/float64(s.Museums), s.UnknownCount)

	type kv struct {
		k string
		n int
	}
	top := func(m map[string]int, n int) []kv {
		var l []kv
		for k, v := range m {
			l = append(l, kv{k, v})
		}
		sort.Slice(l, func(i, j int) bool { return l[i].n > l[j].n })
		if len(l) > n {
			l = l[:n]
		}
		return l
	}

	fmt.Printf("\n%d countries. Top 25:\n", len(s.Countries))
	for _, e := range top(s.Countries, 25) {
		fmt.Printf("  %-26s %6d\n", e.k, e.n)
	}

	fmt.Println("\nclasses:")
	for _, e := range top(s.Classes, 25) {
		fmt.Printf("  %-26s %6d\n", e.k, e.n)
	}

	africa := []string{"Nigeria", "Kenya", "South Africa", "Egypt", "Ghana", "Ethiopia",
		"Tanzania", "Uganda", "Morocco", "Senegal", "Cameroon", "Zimbabwe", "Zambia", "Somalia"}
	fmt.Println("\nAfrica (Wikidata's number in brackets):")
	wikidata := map[string]int{"Nigeria": 119, "Kenya": 23, "South Africa": 155, "Egypt": 155,
		"Ghana": 26, "Ethiopia": 12, "Uganda": 63, "Morocco": 72, "Senegal": 24, "Cameroon": 83, "Somalia": 1}
	for _, c := range africa {
		fmt.Printf("  %-16s %5d   [wikidata %d]\n", c, s.Countries[c], wikidata[c])
	}

	fmt.Println("\nsamples:")
	for _, m := range sample[:min(10, len(sample))] {
		fmt.Printf("  %-40s %-16s %-18s %s\n", trunc(m.Name, 38), m.Country, trunc(m.Locality, 16), m.Website)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n-1]) + "…"
}
