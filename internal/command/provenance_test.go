package command

import (
	"strings"
	"testing"

	"museum/pkg/exhibitions"
)

func TestProvenanceSummary(t *testing.T) {
	tests := []struct {
		name  string
		tally map[exhibitions.Provenance]int
		total int
		want  string
	}{
		{
			name:  "nothing found says so rather than dividing by zero",
			total: 0,
			want:  "nothing found",
		},
		{
			// The run the fallback is built for: it read the sites nothing
			// else could, and its share is the number worth watching.
			name: "every rung contributed",
			tally: map[exhibitions.Provenance]int{
				exhibitions.ProvenanceDeclared:    10,
				exhibitions.ProvenanceHeuristic:   70,
				exhibitions.ProvenanceGenerated:   15,
				exhibitions.ProvenanceDescription: 5,
			},
			total: 100,
			want:  "declared 10 (10.0%), heuristic 70 (70.0%), generated 15 (15.0%), description 5 (5.0%)",
		},
		{
			// A rung at zero is still reported, because a silent rung and a
			// rung nobody ran look the same on a log line.
			name: "an unused rung is still named",
			tally: map[exhibitions.Provenance]int{
				exhibitions.ProvenanceHeuristic: 4,
			},
			total: 4,
			want:  "declared 0 (0.0%), heuristic 4 (100.0%), generated 0 (0.0%), description 0 (0.0%)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := provenanceSummary(tt.tally, tt.total); got != tt.want {
				t.Errorf("provenanceSummary() =\n %q\nwant\n %q", got, tt.want)
			}
		})
	}
}

// TestProvenanceRungsCoverEveryProvenance guards the summary against a rung
// added to the scraper and forgotten here, which would quietly drop a share
// from every run's report.
func TestProvenanceRungsCoverEveryProvenance(t *testing.T) {
	all := []exhibitions.Provenance{
		exhibitions.ProvenanceDeclared,
		exhibitions.ProvenanceHeuristic,
		exhibitions.ProvenanceGenerated,
		exhibitions.ProvenanceDescription,
	}

	for _, p := range all {
		if !strings.Contains(provenanceSummary(map[exhibitions.Provenance]int{p: 1}, 1), string(p)) {
			t.Errorf("provenance %q is never reported by provenanceSummary", p)
		}
	}
}
