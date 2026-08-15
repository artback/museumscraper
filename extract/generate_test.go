package extract

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// scriptedModel answers with a prepared sequence, recording what it was asked.
// The prompts matter as much as the answers: the point of several of these
// tests is that a failed attempt is fed back rather than merely retried.
type scriptedModel struct {
	answers []string
	err     error
	prompts []string
}

func (m *scriptedModel) Name() string { return "scripted" }

func (m *scriptedModel) Complete(_ context.Context, _, user string) (string, error) {
	m.prompts = append(m.prompts, user)
	if m.err != nil {
		return "", m.err
	}
	if len(m.prompts) > len(m.answers) {
		return "", errors.New("scriptedModel ran out of answers")
	}
	return m.answers[len(m.prompts)-1], nil
}

// envelope renders a script as the JSON the contract asks for.
func envelope(t *testing.T, script string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"script": script, "notes": "test"})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(encoded)
}

func testGenerator(model Model) *Generator {
	return &Generator{
		Model: model,
		Now:   func() time.Time { return time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC) },
	}
}

// listingSource matches the fixture in sandbox_test.go.
func listingSource() Source {
	return Source{
		Name:   "example-source",
		URL:    "https://example.org/whats-on",
		Expect: Expectation{MinRecords: 1},
		Schema: Schema{
			Name:   "listings",
			Intent: "the events currently listed",
			Fields: []Field{
				{Name: "title", Kind: KindString, Required: true,
					Rules: Rules{MinLength: 2, Placeholders: []string{"Find out more"}}},
				{Name: "url", Kind: KindURL, Required: true},
				{Name: "opens", Kind: KindDate},
			},
		},
	}
}

func TestGenerate(t *testing.T) {
	model := &scriptedModel{answers: []string{envelope(t, listingScript)}}

	artifact, report, err := testGenerator(model).Generate(
		context.Background(), listingSource(), testPage(t, listingPage))
	if err != nil {
		t.Fatalf("Generate() error = %v (attempts: %+v)", err, report.Attempts)
	}

	if artifact.Version != 1 {
		t.Errorf("Generate() version = %d, want 1", artifact.Version)
	}
	if artifact.Script != listingScript {
		t.Errorf("Generate() stored a different script than the model produced")
	}
	if artifact.Fingerprint == "" {
		t.Error("Generate() stored no fingerprint, so drift could never be detected")
	}

	// Provenance is what makes a surprising extraction traceable.
	switch p := artifact.Provenance; {
	case p.Model != "scripted":
		t.Errorf("Provenance.Model = %q, want %q", p.Model, "scripted")
	case p.Prompt != PromptVersion:
		t.Errorf("Provenance.Prompt = %q, want %q", p.Prompt, PromptVersion)
	case p.PageDigest != Digest(listingPage):
		t.Error("Provenance.PageDigest does not identify the page it was generated from")
	case p.Attempts != 1:
		t.Errorf("Provenance.Attempts = %d, want 1", p.Attempts)
	}

	if report.Reduction.Ratio() <= 1 {
		t.Errorf("Report.Reduction = %s, want the page to have reduced", report.Reduction)
	}
}

// TestGenerateNeverStoresAnArtifactThatFailsItsOwnPage is the guarantee that
// makes generation safe to run unattended.
func TestGenerateNeverStoresAnArtifactThatFailsItsOwnPage(t *testing.T) {
	// Compiles, runs, returns records — and every title is the button label
	// rather than the heading, which is the classic selector-one-element-too-
	// high mistake.
	const wrong = `function extract(document) {
	  return [...document.querySelectorAll('li.listing')].map(row => ({
	    title: row.querySelector('a.more').innerText,
	    url: row.querySelector('a.more').href,
	  }));
	}`

	model := &scriptedModel{answers: []string{
		envelope(t, wrong), envelope(t, wrong), envelope(t, wrong),
	}}

	_, report, err := testGenerator(model).Generate(
		context.Background(), listingSource(), testPage(t, listingPage))

	if !errors.Is(err, ErrGeneration) {
		t.Fatalf("Generate(placeholder-only artifact) error = %v, want ErrGeneration", err)
	}
	if len(report.Attempts) != DefaultAttempts {
		t.Errorf("Generate() made %d attempts, want %d", len(report.Attempts), DefaultAttempts)
	}

	findings := strings.Join(report.Attempts[0].Findings, "; ")
	if !strings.Contains(findings, "placeholder") {
		t.Errorf("attempt findings = %q, want the placeholder named", findings)
	}

	// The failure has to reach the next prompt, or the retry is just a reroll.
	if len(model.prompts) < 2 || !strings.Contains(model.prompts[1], "rejected") {
		t.Error("the second prompt did not carry the first attempt's failure back to the model")
	}
}

func TestGenerateRetriesOnBadEnvelope(t *testing.T) {
	model := &scriptedModel{answers: []string{
		"Sure! Here's a script that extracts the entries:\n\n```js\nfunction extract(d){}\n```",
		envelope(t, listingScript),
	}}

	artifact, report, err := testGenerator(model).Generate(
		context.Background(), listingSource(), testPage(t, listingPage))
	if err != nil {
		t.Fatalf("Generate() error = %v (attempts: %+v)", err, report.Attempts)
	}

	if artifact.Provenance.Attempts != 2 {
		t.Errorf("Provenance.Attempts = %d, want 2", artifact.Provenance.Attempts)
	}
	if len(report.Attempts) != 2 || report.Attempts[0].Problem == "" {
		t.Errorf("Report.Attempts = %+v, want the first attempt recorded as rejected", report.Attempts)
	}
}

func TestGenerateStopsOnModelFailure(t *testing.T) {
	model := &scriptedModel{err: errors.New("connection refused")}

	_, report, err := testGenerator(model).Generate(
		context.Background(), listingSource(), testPage(t, listingPage))

	if err == nil {
		t.Fatal("Generate() with an unreachable model returned no error")
	}
	// An unreachable model is not a source that cannot be compiled, and
	// retrying a connection refusal only delays reporting it.
	if len(report.Attempts) != 1 {
		t.Errorf("Generate() made %d attempts against an unreachable model, want 1", len(report.Attempts))
	}
}

func TestGenerateRejectsInvalidSource(t *testing.T) {
	source := listingSource()
	source.URL = "file:///etc/passwd"

	if _, _, err := testGenerator(&scriptedModel{}).Generate(
		context.Background(), source, testPage(t, listingPage)); err == nil {
		t.Error("Generate() accepted a source with a non-http URL")
	}
}

func TestHeal(t *testing.T) {
	// The site has moved its listing from <li class="listing"> to
	// <article class="card">, which is what a real redesign looks like.
	const redesigned = `<html><body><main><div class="programme">
	  <article class="card"><h2 class="card__heading">Bronze Age Britain</h2>
	    <a class="card__link" href="/listings/bronze-age">Find out more</a>
	    <time datetime="2026-09-01">1 September</time></article>
	  <article class="card"><h2 class="card__heading">Silk Roads</h2>
	    <a class="card__link" href="/listings/silk-roads">Find out more</a>
	    <time datetime="2026-10-12">12 October</time></article>
	</div></main></body></html>`

	const healed = `function extract(document) {
	  return [...document.querySelectorAll('article.card')].map(row => ({
	    title: row.querySelector('h2.card__heading').innerText,
	    url: row.querySelector('a.card__link').href,
	    opens: row.querySelector('time').getAttribute('datetime'),
	  }));
	}`

	previous := Artifact{
		Source:      "example-source",
		Version:     4,
		Script:      listingScript,
		Fingerprint: "old",
	}

	model := &scriptedModel{answers: []string{envelope(t, healed)}}
	artifact, _, err := testGenerator(model).Heal(
		context.Background(), listingSource(), testPage(t, redesigned),
		previous, "run failed validation: extraction produced no records at all")
	if err != nil {
		t.Fatalf("Heal() error = %v", err)
	}

	if artifact.Version != 5 {
		t.Errorf("Heal() version = %d, want 5", artifact.Version)
	}
	if artifact.Parent != 4 {
		t.Errorf("Heal() parent = %d, want 4", artifact.Parent)
	}
	if artifact.Reason == "" {
		t.Error("Heal() recorded no reason, so the version diff would not explain itself")
	}
	if artifact.Fingerprint == previous.Fingerprint {
		t.Error("Heal() kept the old fingerprint rather than the rebuilt page's")
	}

	// The broken script has to be in the prompt: a partial break is repaired
	// by keeping what still works, not by rediscovering the page.
	if !strings.Contains(model.prompts[0], listingScript) {
		t.Error("Heal() did not show the model the artifact that broke")
	}
	if !strings.Contains(model.prompts[0], "produced no records") {
		t.Error("Heal() did not tell the model why the artifact broke")
	}
}

func TestParseEnvelope(t *testing.T) {
	good := `function extract(document) { return []; }`

	tests := []struct {
		name    string
		answer  string
		want    string
		wantErr bool
	}{
		{
			name:   "plain envelope",
			answer: `{"script": "function extract(document) { return []; }", "notes": "n"}`,
			want:   good,
		},
		{
			name:   "fenced envelope",
			answer: "```json\n{\"script\": \"function extract(document) { return []; }\"}\n```",
			want:   good,
		},
		{
			name:    "prose is rejected rather than mined for code",
			answer:  "Here's the script:\n\n```js\nfunction extract(d) { return []; }\n```",
			wantErr: true,
		},
		{
			name:    "empty script",
			answer:  `{"script": "  ", "notes": "n"}`,
			wantErr: true,
		},
		{
			name:    "script that does not compile",
			answer:  `{"script": "function extract( { ???"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script, _, err := parseEnvelope(tt.answer)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("parseEnvelope(%s) error = %v, want error presence = %t", tt.name, err, tt.wantErr)
			}
			if !tt.wantErr && script != tt.want {
				t.Errorf("parseEnvelope(%s) = %q, want %q", tt.name, script, tt.want)
			}
		})
	}
}

func TestShouldHeal(t *testing.T) {
	tests := []struct {
		name     string
		verdict  Verdict
		drifted  bool
		wantHeal bool
	}{
		{name: "a failure is authority enough", verdict: Fail, wantHeal: true},
		{name: "a failure on a drifted page", verdict: Fail, drifted: true, wantHeal: true},
		{
			// The case that would otherwise never be repaired.
			name: "suspect plus drift is a partial break", verdict: Suspect, drifted: true, wantHeal: true,
		},
		{
			// The case that would otherwise heal on every quiet week.
			name: "suspect without drift is probably transient", verdict: Suspect, wantHeal: false,
		},
		{name: "a pass is never healed", verdict: Pass, wantHeal: false},
		{name: "a pass on a drifted page is still a pass", verdict: Pass, drifted: true, wantHeal: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assessment := Assessment{Verdict: tt.verdict, Findings: []string{"a finding"}}

			heal, why := ShouldHeal(assessment, tt.drifted)
			if heal != tt.wantHeal {
				t.Errorf("ShouldHeal(%s, drifted=%t) = %t, want %t", tt.verdict, tt.drifted, heal, tt.wantHeal)
			}
			if heal && why == "" {
				t.Error("ShouldHeal() authorised a heal without a reason")
			}
		})
	}
}

func TestHealPolicyEscalate(t *testing.T) {
	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)

	healed := func(ago time.Duration) Run {
		return Run{At: now.Add(-ago), Healed: true, Verdict: Pass}
	}

	tests := []struct {
		name         string
		runs         []Run
		wantEscalate bool
	}{
		{name: "no history", runs: nil},
		{
			name: "two heals today is within the cap",
			runs: []Run{healed(1 * time.Hour), healed(3 * time.Hour)},
		},
		{
			name:         "three heals today is the cap",
			runs:         []Run{healed(1 * time.Hour), healed(3 * time.Hour), healed(6 * time.Hour)},
			wantEscalate: true,
		},
		{
			// A source healed three times over three months is not a source in
			// a heal loop; it is a site that gets redesigned.
			name: "three heals spread over months",
			runs: []Run{healed(1 * time.Hour), healed(40 * 24 * time.Hour), healed(80 * 24 * time.Hour)},
		},
		{
			// Failures are cheap; it is the regeneration that costs.
			name: "many failures without heals",
			runs: []Run{
				{At: now.Add(-time.Hour), Verdict: Fail},
				{At: now.Add(-2 * time.Hour), Verdict: Fail},
				{At: now.Add(-3 * time.Hour), Verdict: Fail},
				{At: now.Add(-4 * time.Hour), Verdict: Fail},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			escalate, why := HealPolicy{}.Escalate(tt.runs, now)
			if escalate != tt.wantEscalate {
				t.Errorf("Escalate(%s) = %t, want %t", tt.name, escalate, tt.wantEscalate)
			}
			if escalate && why == "" {
				t.Error("Escalate() escalated without a reason")
			}
		})
	}
}
