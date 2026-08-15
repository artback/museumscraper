// Package extract compiles a web page and a declared schema into durable,
// executable extraction logic, and keeps that logic working as the page
// changes.
//
// The problem it answers is the one pkg/exhibitions demonstrates at length:
// hand-written extraction is tedious to write, worse to maintain, and fails
// silently. A site rearranges its markup, the selectors stop matching, and the
// result is an empty list — indistinguishable, downstream, from a museum with
// nothing on show. The alternative of asking a language model to read every
// page on every run is slow, expensive, non-deterministic and unauditable.
//
// So the model is used as a compiler rather than as a runtime. It inspects a
// page once and emits a script; every subsequent run executes that script with
// no model involved, deterministically and for free. The model is re-invoked
// only when a run has produced evidence that the script has stopped working.
//
// Three properties make that safe enough to run unattended:
//
// The generated script is JavaScript executed in a bare goja interpreter with a
// read-only DOM and nothing else. There is no fetch, no XMLHttpRequest, no
// require, no process, no timers and no console, because none of those exist in
// an empty interpreter — they are absent by construction rather than forbidden
// by instruction, which is the only form of that guarantee worth having when
// the code was written by a model.
//
// Nothing is stored until it has been tried. A generated script is run against
// the very page that produced it and validated before it is allowed to replace
// anything, so an artifact that cannot extract its own source page never
// reaches the store.
//
// Every run is graded, and a grade of anything but pass withholds the data.
// Structural validity alone is never enough: a page that silently drops from
// two hundred rows to three yields three perfectly well-formed records, and
// only a count compared against this source's own history catches it.
package extract

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Verdict is the validator's judgement on one run's output.
//
// The three-way grade exists because "not good enough to publish" and "broken"
// call for different responses. A fail is evidence the artifact no longer fits
// the page and is what authorises a heal; a suspect result is withheld and
// reported but does not spend a model invocation, because the commonest cause
// of one is the source having genuinely less to show this week.
type Verdict string

const (
	// Pass means the output is trustworthy and may be published.
	Pass Verdict = "pass"
	// Suspect means the output is plausible but out of character for this
	// source. It is held back and surfaced, never published.
	Suspect Verdict = "suspect"
	// Fail means the output is unusable. This is what triggers healing.
	Fail Verdict = "fail"
)

// Publishable reports whether output with this verdict may be delivered
// downstream. Only Pass is, which is the point of the type.
func (v Verdict) Publishable() bool { return v == Pass }

// Kind is the declared type of a schema field.
//
// The set is deliberately small. Every kind here is one the validator can check
// cheaply and the runner can coerce without guessing, which is the only reason
// for a kind to exist: a field whose type cannot be checked buys nothing over a
// string.
type Kind string

const (
	// KindString is free text.
	KindString Kind = "string"
	// KindURL is a link, resolved against the page it was read from.
	KindURL Kind = "url"
	// KindDate is a calendar date or timestamp.
	KindDate Kind = "date"
	// KindNumber is a numeric quantity.
	KindNumber Kind = "number"
	// KindBool is a flag.
	KindBool Kind = "bool"
)

// Rules are the per-field plausibility checks the validator applies at the
// semantic rung of its ladder.
//
// They are declared by the operator alongside the schema and never written by
// the model, because their whole purpose is to be an independent statement of
// what correct output looks like. A model that both extracted the data and
// declared what counted as plausible would grade its own homework.
type Rules struct {
	// Pattern is a regular expression the value must match, for fields with a
	// recognisable shape.
	Pattern string `json:"pattern,omitempty"`

	// OneOf enumerates the permitted values.
	OneOf []string `json:"one_of,omitempty"`

	// Min and Max bound a KindNumber field, or a KindDate field's offset in
	// days from the day the run happened — negative into the past.
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`

	// MinLength and MaxLength bound a string's length. A zero MaxLength means
	// unbounded.
	MinLength int `json:"min_length,omitempty"`
	MaxLength int `json:"max_length,omitempty"`

	// Placeholders are values that parse correctly and mean nothing — the
	// "Find out more" button labels and "#" hrefs that a selector aimed one
	// element too high collects instead of the data. They are matched case
	// insensitively after trimming.
	//
	// This rung catches the failure the structural rung cannot: output that is
	// complete, well-typed, and entirely furniture.
	Placeholders []string `json:"placeholders,omitempty"`
}

// Field is one declared element of a source's output.
type Field struct {
	// Name is the key the artifact must emit.
	Name string `json:"name"`
	// Kind is the declared type.
	Kind Kind `json:"kind"`
	// Description tells the model what the field means. It is the only part of
	// the schema written for the model rather than for the validator, and it
	// is worth writing well: it is what distinguishes an exhibition's opening
	// date from the date the page was published.
	Description string `json:"description,omitempty"`
	// Required means a record missing this field, or carrying it empty, is not
	// a record at all and is dropped.
	Required bool `json:"required,omitempty"`
	// Rules are the semantic checks applied to the value.
	Rules Rules `json:"rules,omitempty"`
}

// Schema is the declared shape of one source's output.
//
// It is written by the operator and never by the model. That division is the
// spine of the whole design: the model decides how to read a page, and the
// schema decides what a correct reading looks like, so a model that has
// misunderstood the page produces output that fails a check it did not write.
type Schema struct {
	// Name identifies the schema in logs and prompts.
	Name string `json:"name"`
	// Intent is a sentence describing what the extraction is for. The
	// model-judged rung of the validator asks whether output plausibly answers
	// it, so it should describe the goal rather than the mechanism.
	Intent string `json:"intent,omitempty"`
	// Fields are the declared fields, in the order a prompt should present
	// them.
	Fields []Field `json:"fields"`
}

// Field returns the named field.
func (s Schema) Field(name string) (Field, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Required returns the names of the fields a record cannot omit.
func (s Schema) Required() []string {
	var names []string
	for _, f := range s.Fields {
		if f.Required {
			names = append(names, f.Name)
		}
	}
	return names
}

// Validate reports whether the schema is usable, so a malformed one is
// rejected when it is defined rather than when it is first run.
func (s Schema) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("schema has no name")
	}
	if len(s.Fields) == 0 {
		return fmt.Errorf("schema %q declares no fields", s.Name)
	}

	seen := make(map[string]bool, len(s.Fields))
	required := 0
	for _, f := range s.Fields {
		switch {
		case strings.TrimSpace(f.Name) == "":
			return fmt.Errorf("schema %q has a field with no name", s.Name)
		case seen[f.Name]:
			return fmt.Errorf("schema %q declares field %q twice", s.Name, f.Name)
		}
		seen[f.Name] = true

		switch f.Kind {
		case KindString, KindURL, KindDate, KindNumber, KindBool:
		default:
			return fmt.Errorf("schema %q field %q has unknown kind %q", s.Name, f.Name, f.Kind)
		}
		// Compiled here so a bad pattern is reported once, when the schema is
		// defined, rather than once per value per run — and so that a rule the
		// operator believes they wrote cannot silently never match.
		if f.Rules.Pattern != "" {
			if _, err := regexp.Compile(f.Rules.Pattern); err != nil {
				return fmt.Errorf("schema %q field %q has an invalid pattern %q: %w",
					s.Name, f.Name, f.Rules.Pattern, err)
			}
		}
		if f.Required {
			required++
		}
	}

	// A schema in which nothing is required cannot fail the structural rung of
	// the validator, which makes an empty-looking result indistinguishable from
	// a good one. That is precisely the silent failure this package exists to
	// prevent, so it is refused at definition time.
	if required == 0 {
		return fmt.Errorf("schema %q marks no field required: nothing would ever fail structural validation", s.Name)
	}
	return nil
}

// Expectation is what this source's output should look like in bulk, and is
// the input to the volumetric rung of the validator.
type Expectation struct {
	// MinRecords is the floor below which output is wrong regardless of
	// history. A listing page that yields nothing has failed, even on the
	// first run when there is no trailing average to compare against.
	MinRecords int `json:"min_records,omitempty"`

	// Tolerance is how far the record count may stray from this source's
	// trailing average before the run is graded suspect, as a fraction. 0.5
	// admits half to one-and-a-half times the usual count.
	//
	// It is per-source because sources differ in how much they legitimately
	// move: a national gallery's programme changes by one or two entries a
	// month, a listings aggregator's by hundreds a day.
	Tolerance float64 `json:"tolerance,omitempty"`
}

// DefaultTolerance is the volumetric band used when a source declares none.
const DefaultTolerance = 0.5

// Band returns the permitted record count around a trailing average.
func (e Expectation) Band(average float64) (low, high int) {
	tolerance := e.Tolerance
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	low = int(average * (1 - tolerance))
	high = int(average*(1+tolerance)) + 1

	if low < e.MinRecords {
		low = e.MinRecords
	}
	// A tolerance of 1 or more drives the floor to zero or below, and a zero
	// floor makes the rung vacuous: every count, including none at all, sits
	// inside the band. Source.Validate refuses such a tolerance; this is the
	// second line of defence, for sources stored before it did.
	if low < 1 {
		low = 1
	}
	return low, high
}

// Duration is a time.Duration that reads and writes as a string.
//
// A stored source is meant to be reviewed by a person, and time.Duration
// marshals to a count of nanoseconds: an operator opening a source definition
// to check its cadence would find "every": 86400000000000 rather than "24h".
// The store is the single source of truth, so it is worth it being legible.
type Duration time.Duration

// Every returns the duration.
func (d Duration) Every() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	// Numbers are accepted as nanoseconds so that a definition written before
	// this type existed still loads.
	var nanoseconds int64
	if err := json.Unmarshal(data, &nanoseconds); err == nil {
		*d = Duration(nanoseconds)
		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("duration must be a string such as \"24h\": %w", err)
	}
	if text == "" {
		*d = 0
		return nil
	}

	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("duration %q: %w", text, err)
	}
	*d = Duration(parsed)
	return nil
}

// Source is a named extraction target: where to read, and what to expect back.
type Source struct {
	// Name identifies the source everywhere — in the store's keys, in the CLI,
	// and in logs.
	Name string `json:"name"`
	// URL is the page to read.
	URL string `json:"url"`
	// Schema declares the output.
	Schema Schema `json:"schema"`
	// Expect declares the bulk shape of the output.
	Expect Expectation `json:"expect,omitempty"`

	// Every is how often the scheduler should run this source. Zero means it
	// is run by hand only.
	Every Duration `json:"every,omitempty"`

	// Paused stops the scheduler picking the source up, without deleting
	// anything. Quarantine sets it, and so does the operator.
	Paused bool `json:"paused,omitempty"`
	// PausedReason records why, so a quarantined source explains itself.
	PausedReason string `json:"paused_reason,omitempty"`

	// CreatedAt is when the source was defined.
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// Validate reports whether the source is usable.
func (s Source) Validate() error {
	switch {
	case strings.TrimSpace(s.Name) == "":
		return fmt.Errorf("source has no name")
	case strings.ContainsAny(s.Name, "/ \t\n"):
		// The name becomes a key prefix in object storage, so a slash in it
		// would silently nest one source's history inside another's.
		return fmt.Errorf("source name %q may not contain slashes or spaces", s.Name)
	case strings.TrimSpace(s.URL) == "":
		return fmt.Errorf("source %q has no URL", s.Name)
	case !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://"):
		return fmt.Errorf("source %q URL %q is not http or https", s.Name, s.URL)
	case internalHost(s.URL):
		return fmt.Errorf("source %q URL %q points at a private or loopback address; "+
			"sources are public web pages", s.Name, s.URL)
	case s.Expect.Tolerance >= 1:
		// A tolerance of 1 puts the bottom of the band at zero, so every count
		// including none at all sits inside it and the volumetric rung — the
		// only one that catches a page which has silently stopped listing
		// things — stops meaning anything.
		return fmt.Errorf("source %q has a tolerance of %g, which admits any record count; use less than 1",
			s.Name, s.Expect.Tolerance)
	}
	return s.Schema.Validate()
}

// internalHost reports whether a URL names an address on the host or its
// private network by literal IP.
//
// Museum websites arrive from OpenStreetMap and Wikidata, both world-editable,
// and the Pi this runs on shares a network with sixteen other services. A
// source URL is the one field of a museum record that is dereferenced, so a
// literal private address in it is worth refusing at definition time.
//
// It is deliberately narrow, and does not resolve names. Resolving would make
// validation depend on DNS at definition time, non-deterministic, and offline
// tests impossible — and it would still not close the hole, since a name can
// resolve differently later and a redirect is not checked here at all. The
// complete fix is a dialler control on the fetcher, which is shared with the
// catalogue's own crawler and is a wider change than this; see the README.
//
// Carrier-grade NAT (100.64.0.0/10) is deliberately NOT refused: that is
// Tailscale's range, and the operator reaches their own deployment over it.
func internalHost(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}

	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}

	address, err := netip.ParseAddr(host)
	if err != nil {
		// A name, not a literal. Not judged here.
		return false
	}
	return address.IsLoopback() ||
		address.IsPrivate() ||
		address.IsLinkLocalUnicast() ||
		address.IsUnspecified()
}

// Record is one extracted item. Values are the JSON types the sandbox is
// allowed to return: string, float64, bool, or nil.
type Record map[string]any

// String returns the named value as text, reporting whether it was present and
// was in fact text.
func (r Record) String(name string) (string, bool) {
	s, ok := r[name].(string)
	return s, ok
}

// Provenance records how an artifact came to exist, so a surprising extraction
// can be traced to the model, prompt and page that produced it.
type Provenance struct {
	// Model is the model that wrote the script.
	Model string `json:"model"`
	// Prompt is the version of the prompt it was given, so a change in
	// generation quality can be attributed to a change in the prompt.
	Prompt string `json:"prompt"`
	// PageDigest identifies the page snapshot the script was written against.
	PageDigest string `json:"page_digest"`
	// Library names the standard library the script was generated and trialled
	// against, so a script that throws "museum is not defined" on a runner
	// configured without it says why rather than looking like a broken
	// extractor and spending a heal to rediscover itself.
	Library string `json:"library,omitempty"`
	// Attempts is how many generations were tried before one passed its trial.
	Attempts int `json:"attempts"`
	// GeneratedAt is when generation finished.
	GeneratedAt time.Time `json:"generated_at,omitzero"`
}

// Artifact is the generated extraction logic for one source, at one version.
//
// It is stored as JSON with the script as a plain string field so that a
// version-to-version diff reads as a diff of the script, which is what an
// operator reviewing a heal actually wants to see.
type Artifact struct {
	// Source is the name of the source this extracts.
	Source string `json:"source"`
	// Version starts at 1 and increments with every heal. Versions are never
	// overwritten: rollback is just reading an older one.
	Version int `json:"version"`
	// Script is the JavaScript, as generated.
	Script string `json:"script"`
	// Fingerprint is the structural signature of the page the script was
	// written against. Comparing it against a fresh page is how drift is
	// detected without spending a model invocation.
	Fingerprint string `json:"fingerprint"`
	// Provenance says where the script came from.
	Provenance Provenance `json:"provenance,omitzero"`

	// Parent is the version this one healed from, and Reason is why. Both are
	// empty on a first generation.
	Parent int    `json:"parent,omitempty"`
	Reason string `json:"reason,omitempty"`

	// CreatedAt is when this version was stored.
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// Run is the record of one execution of one artifact against one source.
//
// Run history is not a log. The volumetric rung of the validator reads it to
// compute the trailing average a count is judged against, and the healer reads
// it to decide whether this source has been healed too often lately, so it is
// load-bearing state rather than diagnostics.
type Run struct {
	// Source is the source that was run.
	Source string `json:"source"`
	// At is when the run started.
	At time.Time `json:"at"`
	// Version is the artifact version that executed.
	Version int `json:"version"`

	// Verdict is the grade. Records is how many records the run produced.
	Verdict Verdict `json:"verdict"`
	Records int     `json:"records"`

	// Fingerprint is the fetched page's structural signature, and Drifted
	// reports whether it differed from the artifact's.
	Fingerprint string `json:"fingerprint"`
	Drifted     bool   `json:"drifted,omitempty"`

	// Healed reports whether this run regenerated the artifact, and
	// HealedTo the version it produced.
	Healed   bool `json:"healed,omitempty"`
	HealedTo int  `json:"healed_to,omitempty"`

	// Findings are the validator's reasons, in the order the ladder produced
	// them. A passing run carries none.
	Findings []string `json:"findings,omitempty"`

	// Err is the error that ended the run, for failures that never reached the
	// validator at all — a fetch that timed out, a script that would not
	// compile.
	Err string `json:"error,omitempty"`

	// Duration is how long the run took end to end.
	Duration time.Duration `json:"duration,omitempty"`
}

// Published reports whether this run's output was delivered downstream.
func (r Run) Published() bool { return r.Verdict.Publishable() }
