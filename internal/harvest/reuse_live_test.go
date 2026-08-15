package harvest

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"museum/pkg/exhibitions"
	"museum/pkg/extract"
)

// artifactDir points at a directory of .js extractors to cross-test.
var artifactDir = flag.String("harvest.artifacts", "",
	"directory of generated .js extractors to try against every live museum")

// TestLiveCrossReuse measures whether an extractor generated for one museum
// works on another.
//
// This is the measurement behind "less and less model involvement". If a
// stored extractor validates on a site it was never written for, that site can
// be onboarded for the cost of a fetch instead of a generation — and the
// validator is already the oracle that says whether it worked. Nothing here
// invokes a model.
func TestLiveCrossReuse(t *testing.T) {
	if testing.Short() || !*live {
		t.Skip("live test; run with -harvest.live and without -short")
	}
	if *artifactDir == "" {
		t.Skip("pass -harvest.artifacts=DIR with generated .js extractors")
	}

	scripts := loadScripts(t, *artifactDir)
	if len(scripts) == 0 {
		t.Fatalf("no .js extractors in %s", *artifactDir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// Fetch each page once; politeness is per host, so this is the slow part.
	fetcher := exhibitions.NewFetcher()
	pages := make(map[string]*extract.Page)
	for _, museum := range liveMuseums {
		body, final, err := fetcher.Get(ctx, museum.url)
		if err != nil {
			t.Logf("skipping %s: %v", museum.name, err)
			continue
		}
		if final == "" {
			final = museum.url
		}
		page, err := extract.ParsePage(final, body)
		if err != nil {
			t.Logf("skipping %s: %v", museum.name, err)
			continue
		}
		pages[museum.name] = page
	}

	names := make([]string, 0, len(scripts))
	for name := range scripts {
		names = append(names, name)
	}
	sort.Strings(names)

	sandbox := extract.NewSandbox()
	sandbox.Library = ExhibitionLibrary()
	validator := &extract.Validator{}

	var (
		reusable int
		trials   int
	)

	t.Log("rows are the extractor, columns are the page it was run against")
	t.Logf("%-22s %s", "", strings.Join(padded(names), ""))

	for _, owner := range names {
		row := make([]string, 0, len(names))

		for _, target := range names {
			page, ok := pages[target]
			if !ok {
				row = append(row, pad("-"))
				continue
			}

			source := extract.Source{
				Name:   target,
				URL:    page.URL,
				Schema: ExhibitionSchema(),
				Expect: extract.Expectation{MinRecords: 1, Tolerance: 0.75},
			}

			out, err := sandbox.Run(ctx, scripts[owner], page)
			if err != nil {
				row = append(row, pad("err"))
				continue
			}

			assessment := validator.Validate(ctx, source, out.Records, extract.History{Complete: true})
			cell := fmt.Sprintf("%s/%d", shortVerdict(assessment.Verdict), len(assessment.Records))
			row = append(row, pad(cell))

			if owner != target {
				trials++
				if assessment.Verdict == extract.Pass {
					reusable++
				}
			}
		}
		t.Logf("%-22s %s", owner, strings.Join(row, ""))
	}

	// Structural similarity between every pair, to see whether it separates
	// safe reuse from reuse that merely validates.
	t.Logf("")
	t.Log("structural similarity between pages (Jaccard over fingerprint paths)")
	t.Logf("%-22s %s", "", strings.Join(padded(names), ""))
	for _, a := range names {
		row := make([]string, 0, len(names))
		for _, b := range names {
			pa, oka := pages[a]
			pb, okb := pages[b]
			if !oka || !okb {
				row = append(row, pad("-"))
				continue
			}
			row = append(row, pad(fmt.Sprintf("%.2f", extract.Similarity(pa, pb))))
		}
		t.Logf("%-22s %s", a, strings.Join(row, ""))
	}

	if trials > 0 {
		t.Logf("")
		t.Logf("cross-site reuse: %d of %d extractor/page pairs validated (%.0f%%)",
			reusable, trials, 100*float64(reusable)/float64(trials))
	}
}

func loadScripts(t *testing.T, dir string) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	scripts := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".js" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		scripts[strings.TrimSuffix(entry.Name(), ".js")] = string(body)
	}
	return scripts
}

func shortVerdict(v extract.Verdict) string {
	switch v {
	case extract.Pass:
		return "PASS"
	case extract.Suspect:
		return "susp"
	default:
		return "fail"
	}
}

func pad(s string) string { return fmt.Sprintf("%-12s", s) }
func padded(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if len(s) > 11 {
			s = s[:11]
		}
		out = append(out, pad(s))
	}
	return out
}
