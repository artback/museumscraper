package extract

import (
	"context"
	"strings"
	"testing"
)

func TestModelJudge(t *testing.T) {
	tests := []struct {
		name      string
		answer    string
		wantOK    bool
		wantErr   bool
		wantMatch string
	}{
		{
			name:   "plausible",
			answer: `{"plausible": true, "reason": ""}`,
			wantOK: true,
		},
		{
			name:      "implausible",
			answer:    `{"plausible": false, "reason": "these are gift shop items"}`,
			wantMatch: "gift shop",
		},
		{
			name:   "fenced",
			answer: "```json\n{\"plausible\": true}\n```",
			wantOK: true,
		},
		{
			// A judge that answered with prose has not answered. Reporting it
			// as an error rather than as "implausible" keeps a formatting
			// failure from being read as a fault in the data.
			name:    "prose is an error, not a no",
			answer:  "Yes, these look like exhibitions to me!",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			judge := NewJudge(&scriptedModel{answers: []string{tt.answer}})

			ok, reason, err := judge.Plausible(context.Background(), "exhibitions on show",
				[]Record{{"title": "Bronze Age Britain"}})

			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("Plausible(%s) error = %v, want error presence = %t", tt.name, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if ok != tt.wantOK {
				t.Errorf("Plausible(%s) = %t, want %t", tt.name, ok, tt.wantOK)
			}
			if tt.wantMatch != "" && !strings.Contains(reason, tt.wantMatch) {
				t.Errorf("Plausible(%s) reason = %q, want it to contain %q", tt.name, reason, tt.wantMatch)
			}
		})
	}
}

func TestModelJudgeCapsTheSample(t *testing.T) {
	model := &scriptedModel{answers: []string{`{"plausible": true}`}}
	judge := &ModelJudge{Model: model, Sample: 2}

	records := make([]Record, 50)
	for i := range records {
		records[i] = Record{"title": "Exhibition"}
	}

	if _, _, err := judge.Plausible(context.Background(), "exhibitions", records); err != nil {
		t.Fatalf("Plausible() error = %v", err)
	}

	// The question is whether the output is the right kind of thing, which a
	// couple of records answers as well as fifty would — and fifty in a prompt
	// is what makes a local model slow.
	if strings.Count(model.prompts[0], `"title"`) > 2 {
		t.Errorf("Plausible() sent more than the sample cap:\n%s", model.prompts[0])
	}
}

func TestModelJudgeWithoutModel(t *testing.T) {
	// A misconfigured judge must report that it could not answer, not answer
	// "implausible" — the validator treats the two completely differently.
	if _, _, err := (&ModelJudge{}).Plausible(context.Background(), "x", nil); err == nil {
		t.Error("Plausible() without a model returned no error")
	}
	if _, _, err := NewJudge(nil).Plausible(context.Background(), "x", nil); err == nil {
		t.Error("Plausible() with a nil model returned no error")
	}
}
