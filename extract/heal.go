package extract

import (
	"fmt"
	"time"
)

// Healing policy defaults.
const (
	// DefaultHealWindow is how far back repeated heals are counted.
	DefaultHealWindow = 24 * time.Hour

	// DefaultHealLimit is how many heals a source may have inside that window
	// before it is escalated to a human instead of healed again.
	//
	// Three, because the second heal is where a genuine layout change has
	// already been absorbed and the third is evidence that whatever is wrong is
	// not something regeneration fixes — a site that has started serving a
	// consent wall, or a schema that no longer describes what the page holds.
	// Past that point each attempt is a model invocation spent to produce the
	// same failure.
	DefaultHealLimit = 3
)

// HealPolicy decides when to stop healing a source and ask a human.
type HealPolicy struct {
	// Window is how far back heals are counted. Zero means DefaultHealWindow.
	Window time.Duration
	// Limit is how many heals inside the window are tolerated. Zero means
	// DefaultHealLimit.
	Limit int
}

// ShouldHeal reports whether a run's outcome authorises regenerating the
// artifact, and why.
//
// A failure is authority enough on its own. A suspect result is not, with one
// exception: when the page's structure has also changed. That combination —
// output that still validates but is out of character, on a page that has been
// rebuilt — is exactly a partial break, where the artifact still matches some
// of the page and has quietly stopped matching the rest. Without this, a
// listing that dropped from two hundred rows to three because the markup moved
// would be held back forever and never repaired.
//
// The converse is the reason the fingerprint is worth computing at all: a
// suspect or failing run on a page whose structure has not changed is more
// likely transient than structural, and regenerating would spend a model
// invocation on a network blip or a genuinely quiet week.
func ShouldHeal(assessment Assessment, drifted bool) (bool, string) {
	switch {
	case assessment.Verdict == Fail:
		return true, fmt.Sprintf("run failed validation: %s", firstFinding(assessment))

	case assessment.Verdict == Suspect && drifted:
		return true, fmt.Sprintf("run was suspect and the page structure changed: %s",
			firstFinding(assessment))

	case assessment.Verdict == Suspect:
		return false, "run was suspect but the page structure is unchanged, so the cause is more likely transient than structural"

	default:
		return false, ""
	}
}

func firstFinding(assessment Assessment) string {
	if len(assessment.Findings) == 0 {
		return "no reason recorded"
	}
	return assessment.Findings[0]
}

// Escalate reports whether a source has been healed too often lately to be
// healed again, and why.
//
// This is the cap that stops a permanently dead source burning the budget on
// every schedule tick. It counts heals rather than failures on purpose: a
// source failing hourly and not being healed costs nothing, and it is the
// regeneration that is expensive.
func (p HealPolicy) Escalate(runs []Run, now time.Time) (bool, string) {
	window := p.Window
	if window <= 0 {
		window = DefaultHealWindow
	}
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultHealLimit
	}

	since := now.Add(-window)
	heals := 0
	for _, run := range runs {
		if run.Healed && run.At.After(since) {
			heals++
		}
	}

	if heals >= limit {
		return true, fmt.Sprintf("healed %d times in the last %s, which is the limit; "+
			"regenerating again would spend a model invocation to reach the same failure",
			heals, window)
	}
	return false, ""
}
