package command

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
	"museum/internal/quality"
	"museum/pkg/graceful"
)

// verifyCommand audits the stored catalogue.
func verifyCommand() Command {
	return Command{
		Name:    "verify",
		Summary: "Audit the catalogue for wrong, unusable or suspicious records",
		Usage:   "[-samples 5] [-check NAME] [-json] [-fail-on error|warning|never]",
		Run:     runVerify,
	}
}

func runVerify(ctx context.Context, args []string) error {
	fs := newFlagSet("verify", "[-samples 5] [-check NAME] [-json] [-fail-on error|warning|never]", os.Stderr)
	var (
		samples = fs.Int("samples", 5, "example records to show per check (0 for none)")
		only    = fs.String("check", "", "report only this check")
		asJSON  = fs.Bool("json", false, "emit the full report as JSON")
		failOn  = fs.String("fail-on", "never", "exit non-zero when findings reach this severity: error, warning or never")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs("verify", fs.Args()); err != nil {
		return err
	}

	db, err := database(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := graceful.Context(ctx)
	defer cancel()

	start := time.Now()
	log.Print("Reading the catalogue ...")

	// The audit covers every stored museum, including the ones with no
	// position: a record that cannot be placed is exactly the kind of problem
	// worth reporting, and checking only the locatable ones would hide it.
	var museums []models.Museum
	if err := db.EachMuseum(ctx, func(m models.Museum) { museums = append(museums, m) }); err != nil {
		return err
	}

	report := quality.CheckMuseums(museums)

	if *only != "" {
		report = filterReport(report, *only)
	}

	log.Printf("Audited %d museums and %d exhibitions in %s",
		report.Museums, report.Exhibitions, time.Since(start).Round(time.Second))

	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return err
		}
	} else {
		printReport(os.Stdout, report, *samples)
	}

	return exitStatus(report, *failOn)
}

// filterReport narrows a report to one check.
func filterReport(report quality.Report, check string) quality.Report {
	filtered := quality.Report{
		Museums: report.Museums, Exhibitions: report.Exhibitions,
		Counts: map[string]int{},
	}
	for _, f := range report.Findings {
		if f.Check == check {
			filtered.Findings = append(filtered.Findings, f)
			filtered.Counts[f.Check]++
		}
	}
	return filtered
}

// printReport writes the human-readable summary: a count per check, worst
// first, with a few examples of each.
func printReport(w *os.File, report quality.Report, samples int) {
	if len(report.Findings) == 0 {
		fmt.Fprintln(w, "No problems found.")
		return
	}

	bySeverity := map[string][]quality.Finding{}
	for _, f := range report.Findings {
		bySeverity[f.Check] = append(bySeverity[f.Check], f)
	}

	checks := make([]string, 0, len(bySeverity))
	for check := range bySeverity {
		checks = append(checks, check)
	}
	// Order by count so the biggest problems are read first.
	sort.Slice(checks, func(i, j int) bool {
		return len(bySeverity[checks[i]]) > len(bySeverity[checks[j]])
	})

	fmt.Fprintf(w, "\n%d findings across %d museums and %d exhibitions — %d errors, %d warnings\n\n",
		len(report.Findings), report.Museums, report.Exhibitions, report.Errors(), report.Warnings())

	for _, check := range checks {
		findings := bySeverity[check]
		share := 100 * float64(len(findings)) / float64(max(report.Museums+report.Exhibitions, 1))
		fmt.Fprintf(w, "%-34s %7d  %5.2f%%  [%s]\n",
			check, len(findings), share, findings[0].Severity)

		for i, f := range findings {
			if i >= samples {
				break
			}
			fmt.Fprintf(w, "     %-44s %s\n", truncate(f.Subject, 44), truncate(f.Detail, 96))
		}
		if samples > 0 && len(findings) > samples {
			fmt.Fprintf(w, "     ... and %d more\n", len(findings)-samples)
		}
		fmt.Fprintln(w)
	}
}

// exitStatus turns the report into a process result, so verify can gate a
// scheduled pipeline rather than only informing a human.
func exitStatus(report quality.Report, failOn string) error {
	switch strings.ToLower(failOn) {
	case "", "never":
		return nil
	case "error":
		if n := report.Errors(); n > 0 {
			return fmt.Errorf("%d records are definitely wrong", n)
		}
	case "warning":
		if n := report.Errors() + report.Warnings(); n > 0 {
			return fmt.Errorf("%d records are wrong or suspicious", n)
		}
	default:
		return fmt.Errorf("unknown -fail-on value %q, want error, warning or never", failOn)
	}
	return nil
}
