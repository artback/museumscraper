package extract

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// History is what the validator needs to know about a source's past, supplied
// by the store.
//
// It is only the record counts of runs that passed. Counts from failing runs
// would poison the average with exactly the numbers the average exists to
// catch: three runs of 200 rows and one broken run of 2 must not shift the
// baseline towards accepting 2.
type History struct {
	// Counts are the record counts of recent passing runs, most recent first.
	Counts []int

	// Fingerprint is the page signature of the most recent passing run, and is
	// the baseline drift is measured against.
	//
	// It cannot come from the artifact. An artifact's fingerprint is written
	// once, at generation, and versions are immutable — so the first cosmetic
	// change a still-working extractor survives makes every later run report
	// drift forever. That matters because drift is what turns an ordinary
	// seasonal dip into grounds for regeneration: a source going from twenty
	// entries to four would be graded suspect, read as a partial break,
	// regenerated, honestly re-extract four, and be quarantined for it.
	Fingerprint string

	// Complete reports that the history was read in full. A read that failed,
	// or that silently skipped objects it could not decode, produces a
	// baseline that is not comparable with a real one.
	Complete bool
}

// minHistory is how many passing runs are needed before the trailing average
// is trusted. Below it there is no baseline, and a count is judged only
// against the source's declared floor.
const minHistory = 3

// Average returns the trailing average record count, reporting whether there
// is enough history to trust it.
func (h History) Average() (float64, bool) {
	if len(h.Counts) < minHistory {
		return 0, false
	}
	total := 0
	for _, count := range h.Counts {
		total += count
	}
	return float64(total) / float64(len(h.Counts)), true
}

// Judge is the optional last rung of the ladder: a model asked whether output
// plausibly answers the source's intent.
//
// It is an interface here, and nil by default, because it is the only rung
// that costs anything. The cheap rungs catch the failures that actually
// happen; this one exists for the residue where output is well-formed,
// well-sized, individually plausible and still wrong.
type Judge interface {
	// Plausible reports whether a sample of records answers the intent, and
	// why not when it does not.
	Plausible(ctx context.Context, intent string, sample []Record) (ok bool, reason string, err error)
}

// judgeSample is how many records the judge is shown. The question is whether
// the output is the right kind of thing, which a handful answers as well as
// hundreds would.
const judgeSample = 5

// Validator grades a run's output.
//
// The rungs are ordered by cost and run cheapest first, stopping at the first
// one that returns a definite failure. Nothing below a rung that has already
// failed is worth computing: there is no point asking a model whether an empty
// list is plausible.
type Validator struct {
	// Judge is the optional model-judged rung. Nil disables it.
	Judge Judge

	// Now supplies the current time, so that date rules relative to "today"
	// are testable. Nil means time.Now.
	Now func() time.Time
}

// Assessment is the validator's verdict and its reasoning.
type Assessment struct {
	// Verdict is the grade.
	Verdict Verdict
	// Findings are the reasons, cheapest rung first. A passing run has none.
	Findings []string
	// Records are the ones that survived structural validation, trimmed. These
	// are what a sink publishes, so a run that passes despite a few malformed
	// rows publishes the good rows and not the bad.
	Records []Record
	// Dropped is how many records structural validation removed.
	Dropped int

	// BaselineMissing reports that this run was graded by a weaker ladder than
	// usual because the source's history could not be read.
	//
	// It does not change the verdict, deliberately. Grading such a run Fail
	// would authorise a heal for every source on the schedule the moment
	// object storage hiccuped — minutes of model time each, to fix nothing.
	// It withholds publication instead: a volumetric collapse is exactly what
	// the missing rung would have caught, and publishing it as a pass is the
	// silent failure this package exists to prevent.
	BaselineMissing bool
}

// Publishable reports whether this output may be delivered downstream.
//
// This, not the verdict alone, is what a sink should be gated on: a pass
// awarded without the volumetric rung is not the same fact as a pass awarded
// with it.
func (a Assessment) Publishable() bool {
	return a.Verdict.Publishable() && !a.BaselineMissing
}

// Thresholds at which a rung's complaints stop being tolerable. They are
// fractions of the records examined.
const (
	// failFraction is the share of bad records above which the artifact is
	// judged to be reading the wrong part of the page rather than tripping
	// over a few odd entries.
	failFraction = 0.5

	// suspectFraction is the share above which bad records are worth
	// reporting even though the run may still pass.
	suspectFraction = 0.1
)

// Validate grades records against a source's schema, expectations and history.
func (v *Validator) Validate(ctx context.Context, source Source, records []Record, history History) Assessment {
	assessment := Assessment{Verdict: Pass}

	// A history that could not be read is not a history that is empty. The
	// difference matters: an empty one is a new source, which is expected,
	// while an unreadable one means the volumetric rung did not run at all.
	if !history.Complete {
		assessment.BaselineMissing = true
		assessment.Findings = append(assessment.Findings,
			"graded without a baseline: this source's run history could not be read, "+
				"so the record count was checked only against its declared floor")
	}

	// Rung 1: structural. Output present, conforming to the schema, required
	// fields non-empty.
	valid, structural := v.structural(source.Schema, records)
	assessment.Records = valid
	assessment.Dropped = len(records) - len(valid)
	assessment.add(structural)

	if assessment.Verdict == Fail {
		return assessment
	}

	// Rung 2: volumetric. This is mandatory rather than optional, because it
	// is the only rung that catches a page which still parses, still validates,
	// and has silently stopped listing most of what it used to.
	assessment.add(v.volumetric(source.Expect, len(valid), history))
	if assessment.Verdict == Fail {
		return assessment
	}

	// Rung 3: semantic. Per-field plausibility, declared by the operator.
	assessment.add(v.semantic(source.Schema, valid))
	if assessment.Verdict == Fail {
		return assessment
	}

	// Rung 4: model-judged, and only when the cheap rungs have passed without
	// fully settling the question.
	// Not when the baseline is missing: that output is withheld whatever the
	// judge says, so asking would spend a model invocation to change nothing.
	if v.Judge != nil && !assessment.BaselineMissing && assessment.uncertain(history) {
		assessment.add(v.judged(ctx, source.Schema, valid))
	}
	return assessment
}

// add folds a rung's result into the assessment, keeping the worst verdict.
func (a *Assessment) add(result rung) {
	if result.verdict.worseThan(a.verdict()) {
		a.Verdict = result.verdict
	}
	a.Findings = append(a.Findings, result.findings...)
}

func (a *Assessment) verdict() Verdict {
	if a.Verdict == "" {
		return Pass
	}
	return a.Verdict
}

// uncertain reports whether the cheap rungs left enough doubt to be worth
// spending a model invocation on.
//
// Two things create that doubt: no baseline to compare the count against, and
// records that were dropped or flagged without crossing a failure threshold.
// A source with a settled history producing its usual number of clean records
// is not worth asking about.
func (a *Assessment) uncertain(history History) bool {
	if _, ok := history.Average(); !ok {
		return true
	}
	return a.Dropped > 0 || len(a.Findings) > 0
}

// rung is one ladder step's outcome.
type rung struct {
	verdict  Verdict
	findings []string
}

func pass() rung { return rung{verdict: Pass} }

func (r rung) note(format string, args ...any) rung {
	r.findings = append(r.findings, fmt.Sprintf(format, args...))
	return r
}

func (r rung) grade(v Verdict) rung {
	if v.worseThan(r.verdict) {
		r.verdict = v
	}
	return r
}

// worseThan orders the grades, so folding many rungs together keeps the worst.
func (v Verdict) worseThan(other Verdict) bool {
	rank := map[Verdict]int{Pass: 0, Suspect: 1, Fail: 2}
	return rank[v] > rank[other]
}

// structural checks that each record conforms to the schema, returning the
// ones that do.
func (v *Validator) structural(schema Schema, records []Record) ([]Record, rung) {
	result := pass()

	if len(records) == 0 {
		// Not merely suspicious. An extraction that produced nothing is the
		// exact failure this package exists to stop being silent, and no
		// amount of history makes an empty result publishable.
		return nil, result.grade(Fail).note("extraction produced no records at all")
	}

	declared := make(map[string]bool, len(schema.Fields))
	for _, field := range schema.Fields {
		declared[field.Name] = true
	}

	valid := make([]Record, 0, len(records))
	reasons := make(map[string]int)

	for _, record := range records {
		// Fields the schema never declared are dropped rather than published.
		// Nothing has checked them — they have no kind, no rules, and no
		// meaning to anything downstream — so passing them on would be
		// publishing unexamined model output. Dropped rather than rejected: a
		// model that added a stray field still extracted the declared ones,
		// and failing the run would authorise a heal over a cosmetic fault.
		for name := range record {
			if !declared[name] {
				delete(record, name)
			}
		}

		problem := conforms(schema, record)
		if problem == "" {
			valid = append(valid, record)
			continue
		}
		reasons[problem]++
	}

	dropped := len(records) - len(valid)
	if dropped == 0 {
		return valid, result
	}

	// The reasons are summarised rather than listed. Two hundred rows failing
	// the same way is one fact, and it is the fact that names what changed.
	for _, reason := range sortedByCount(reasons, func(s string) string { return s }) {
		result = result.note("%d of %d records %s", reasons[reason], len(records), reason)
	}

	switch fraction := float64(dropped) / float64(len(records)); {
	case len(valid) == 0:
		result = result.grade(Fail)
	case fraction > failFraction:
		result = result.grade(Fail)
	case fraction > suspectFraction:
		result = result.grade(Suspect)
	}
	return valid, result
}

// conforms reports why a record fails the schema, or "" when it does not.
//
// It trims string values in place: leading and trailing whitespace is an
// artefact of how the markup was indented, never data, and every downstream
// comparison is easier without it.
func conforms(schema Schema, record Record) string {
	for _, field := range schema.Fields {
		raw, present := record[field.Name]

		if text, ok := raw.(string); ok {
			text = strings.TrimSpace(text)
			record[field.Name] = text
			raw = text
		}

		missing := !present || raw == nil || raw == ""
		switch {
		case missing && field.Required:
			return fmt.Sprintf("are missing required field %q", field.Name)
		case missing:
			continue
		}

		if problem := conformsKind(field, raw); problem != "" {
			return problem
		}
	}
	return ""
}

// conformsKind checks one present value against its declared kind.
func conformsKind(field Field, raw any) string {
	switch field.Kind {
	case KindString:
		if _, ok := raw.(string); !ok {
			return fmt.Sprintf("have a non-text %q (%T)", field.Name, raw)
		}

	case KindURL:
		text, ok := raw.(string)
		if !ok {
			return fmt.Sprintf("have a non-text %q (%T)", field.Name, raw)
		}
		parsed, err := url.Parse(text)
		if err != nil || !parsed.IsAbs() || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Sprintf("have a %q that is not an absolute http URL", field.Name)
		}

	case KindDate:
		text, ok := raw.(string)
		if !ok {
			return fmt.Sprintf("have a non-text %q (%T)", field.Name, raw)
		}
		if _, ok := parseDate(text); !ok {
			return fmt.Sprintf("have an unparseable %q", field.Name)
		}

	case KindNumber:
		if _, ok := raw.(float64); !ok {
			return fmt.Sprintf("have a non-numeric %q (%T)", field.Name, raw)
		}

	case KindBool:
		if _, ok := raw.(bool); !ok {
			return fmt.Sprintf("have a non-boolean %q (%T)", field.Name, raw)
		}
	}
	return ""
}

// dateLayouts are the formats a KindDate value may arrive in.
//
// The list is short on purpose. The generator's prompt asks for ISO 8601, and
// a model that has instead emitted "15 January" has told us it is reading the
// human-readable text rather than the machine-readable attribute beside it —
// which is a real defect worth failing on, not a formatting inconvenience to
// paper over here.
var dateLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"2006-01",
	"2006",
}

func parseDate(text string) (time.Time, bool) {
	text = strings.TrimSpace(text)
	for _, layout := range dateLayouts {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// volumetric compares the record count against this source's own history.
func (v *Validator) volumetric(expect Expectation, count int, history History) rung {
	result := pass()

	if count < expect.MinRecords {
		return result.grade(Fail).note(
			"produced %d records, below the declared floor of %d", count, expect.MinRecords)
	}

	average, ok := history.Average()
	if !ok {
		// No baseline yet. The floor is the only check available, and it has
		// already passed.
		return result
	}

	low, high := expect.Band(average)
	if count < low || count > high {
		return result.grade(Suspect).note(
			"produced %d records, outside the usual %d–%d for this source (trailing average %.0f over %d runs)",
			count, low, high, average, len(history.Counts))
	}
	return result
}

// semantic applies the operator's per-field plausibility rules.
func (v *Validator) semantic(schema Schema, records []Record) rung {
	result := pass()
	if len(records) == 0 {
		return result
	}

	now := time.Now
	if v.Now != nil {
		now = v.Now
	}

	// Counted per field, because "every title is a placeholder" and "one title
	// is a placeholder" call for different responses, and the difference is
	// only visible per field.
	violations := make(map[violation]int)
	examples := make(map[violation]string)

	for _, record := range records {
		for _, field := range schema.Fields {
			raw, ok := record[field.Name]
			if !ok || raw == nil || raw == "" {
				continue
			}
			problem := violatesRules(field, raw, now())
			if problem == "" {
				continue
			}
			key := violation{field: field.Name, problem: problem}
			violations[key]++
			if _, seen := examples[key]; !seen {
				examples[key] = fmt.Sprintf("%v", raw)
			}
		}
	}

	for _, key := range sortedByCount(violations, violation.String) {
		count := violations[key]
		result = result.note("%d of %d records %s: %s (e.g. %q)",
			count, len(records), key.field, key.problem, truncateRunes(examples[key], 60))

		declared, _ := schema.Field(key.field)

		switch fraction := float64(count) / float64(len(records)); {
		case declared.Required && fraction > failFraction:
			// A required field implausible in most records means the artifact
			// is pointing at the wrong element, not that the site has a few
			// odd entries.
			result = result.grade(Fail)
		case fraction > suspectFraction:
			result = result.grade(Suspect)
		}
	}
	return result
}

// violatesRules reports how a value breaks its field's rules, or "".
func violatesRules(field Field, raw any, now time.Time) string {
	rules := field.Rules

	if text, ok := raw.(string); ok {
		trimmed := strings.TrimSpace(text)

		for _, placeholder := range rules.Placeholders {
			if strings.EqualFold(trimmed, placeholder) {
				return "are a placeholder value"
			}
		}
		if rules.MinLength > 0 && len([]rune(trimmed)) < rules.MinLength {
			return fmt.Sprintf("are shorter than %d characters", rules.MinLength)
		}
		if rules.MaxLength > 0 && len([]rune(trimmed)) > rules.MaxLength {
			return fmt.Sprintf("are longer than %d characters", rules.MaxLength)
		}
		if rules.Pattern != "" {
			matched, err := regexp.MatchString(rules.Pattern, trimmed)
			if err != nil {
				return fmt.Sprintf("could not be checked against pattern %q", rules.Pattern)
			}
			if !matched {
				return fmt.Sprintf("do not match %q", rules.Pattern)
			}
		}
		if len(rules.OneOf) > 0 && !slices.Contains(rules.OneOf, trimmed) {
			return fmt.Sprintf("are not one of %s", strings.Join(rules.OneOf, ", "))
		}
	}

	// Min and Max mean days from today on a date field and the plain value on
	// a number, which is the only reading that makes a bound on a date useful:
	// "no more than a year out" is a rule that stays true, "before 2027" is one
	// that quietly expires.
	if rules.Min != nil || rules.Max != nil {
		value, ok := boundValue(field, raw, now)
		if !ok {
			return ""
		}
		if rules.Min != nil && value < *rules.Min {
			return fmt.Sprintf("are below the minimum of %g", *rules.Min)
		}
		if rules.Max != nil && value > *rules.Max {
			return fmt.Sprintf("are above the maximum of %g", *rules.Max)
		}
	}
	return ""
}

// boundValue reduces a value to the number its Min and Max bound.
func boundValue(field Field, raw any, now time.Time) (float64, bool) {
	switch field.Kind {
	case KindNumber:
		value, ok := raw.(float64)
		return value, ok

	case KindDate:
		text, ok := raw.(string)
		if !ok {
			return 0, false
		}
		parsed, ok := parseDate(text)
		if !ok {
			return 0, false
		}
		return parsed.Sub(now).Hours() / 24, true

	default:
		return 0, false
	}
}

// judged asks the model whether the output answers the source's intent.
func (v *Validator) judged(ctx context.Context, schema Schema, records []Record) rung {
	result := pass()
	if schema.Intent == "" {
		return result
	}

	sample := records
	if len(sample) > judgeSample {
		sample = sample[:judgeSample]
	}

	ok, reason, err := v.Judge.Plausible(ctx, schema.Intent, sample)
	switch {
	case err != nil:
		// The judge failing is not the data failing. Grading a run down
		// because a local model was unreachable would quarantine healthy
		// sources every time the inference server restarted.
		return result.note("could not be model-judged: %v", err)
	case !ok:
		return result.grade(Suspect).note("model judged the output implausible: %s", reason)
	default:
		return result
	}
}

// violation is one field failing one rule.
//
// It is a struct rather than a formatted string because the field name has to
// be recovered afterwards to look up whether it was required. Recovering it by
// splitting the string on its first colon meant a field named "date:raw"
// resolved to "date" and was graded against the wrong field's rules.
type violation struct {
	field   string
	problem string
}

func (v violation) String() string { return v.field + ": " + v.problem }

// sortedByCount orders map keys by descending count, breaking ties with name
// so that findings are stable between runs and a diff of two run records shows
// only what actually changed.
func sortedByCount[K comparable](counts map[K]int, name func(K) string) []K {
	keys := make([]K, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b K) int {
		if counts[a] != counts[b] {
			return counts[b] - counts[a]
		}
		return strings.Compare(name(a), name(b))
	})
	return keys
}
