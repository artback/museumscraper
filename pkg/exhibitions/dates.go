package exhibitions

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DateRange is the period an exhibition runs for. Either end may be missing:
// listings frequently give only "Until 3 Jan 2027" or "From 5 March".
type DateRange struct {
	Start *time.Time
	End   *time.Time

	// Permanent is set when the display is always on rather than running to a
	// closing date. The opening date may still be known and is kept; the
	// closing date is absent because there is not one.
	//
	// Reading "Permanent" as a start date of today, which is what this
	// replaced, re-dated every such display to the day the scrape happened to
	// run: the Zeppelin Museum's technical tour and the Technisches Museum's
	// "Wissenschaft im Wandel" were both recorded as having opened this
	// morning, every morning.
	Permanent bool
}

// farFutureYears is how far ahead a closing date may be before it stops being
// a date and starts being a way of writing "no end".
//
// Ten years matches the horizon the catalogue audit already calls implausible,
// so the two agree on what a real closing date looks like.
const farFutureYears = 10

// resolveOpenEnd turns a closing date too distant to be meant literally into
// permanence.
//
// A content system needs something in the end-date field, so a permanent
// exhibition is given one. The Technisches Museum Wien's schema.org data
// closes medien.welten on 1 September 3000 and its energy hall on 31 December
// 3000. Read literally those are exhibitions running for a thousand years,
// which sorts them in front of everything that genuinely closes next month —
// the exact opposite of what a visitor deciding what to catch needs.
//
// The convention costs nothing to read and holds in every language, because
// the year is a number wherever the page is written.
func (r DateRange) resolveOpenEnd(now time.Time) DateRange {
	if r.Permanent || r.End == nil {
		return r
	}
	if r.End.Sub(now).Hours()/24/365.25 <= farFutureYears {
		return r
	}
	return DateRange{Start: r.Start, Permanent: true}
}

// Known reports whether anything at all could be read about when the display
// is on, whether that is a date or the fact that it has none.
func (r DateRange) Known() bool { return !r.IsZero() || r.Permanent }

// Runs reports whether the range covers the given day. A missing bound is
// treated as open — "Until 3 Jan 2027" runs from now until then.
func (r DateRange) Runs(on time.Time) bool {
	if r.Permanent {
		return true
	}
	day := time.Date(on.Year(), on.Month(), on.Day(), 0, 0, 0, 0, time.UTC)
	if r.Start != nil && day.Before(*r.Start) {
		return false
	}
	if r.End != nil && day.After(*r.End) {
		return false
	}
	return r.Start != nil || r.End != nil
}

// Upcoming reports whether the range starts after the given day.
func (r DateRange) Upcoming(on time.Time) bool {
	day := time.Date(on.Year(), on.Month(), on.Day(), 0, 0, 0, 0, time.UTC)
	return r.Start != nil && r.Start.After(day)
}

// IsZero reports whether no date at all could be read.
func (r DateRange) IsZero() bool { return r.Start == nil && r.End == nil }

// months maps month names to numbers. English is listed first because these
// are English-language listings by default, followed by the languages that
// appear most in European museum sites; matching is on the first three letters
// so both "January" and "Jan" resolve.
var months = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,

	// French
	"janv": time.January, "fév": time.February, "fev": time.February,
	"avr": time.April, "mai": time.May, "juin": time.June,
	"juil": time.July, "aoû": time.August, "aou": time.August,
	"déc": time.December,

	// German
	"jän": time.January, "mär": time.March, "mrz": time.March,
	"okt": time.October, "dez": time.December,

	// Spanish / Italian / Portuguese / Dutch
	"ene": time.January, "abr": time.April, "ago": time.August,
	"dic": time.December, "gen": time.January, "mag": time.May,
	"giu": time.June, "lug": time.July, "set": time.September,
	"ott": time.October, "out": time.October,
	"mei": time.May, "okt2": time.October,
}

var (
	// dayMonthYear matches "3 Jan 2027", "12 March 2026", "3rd January 2026".
	dayMonthYear = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?[\s.]+([\p{L}]{3,12})\.?[\s,]+(\d{4})\b`)

	// dayMonth matches "12 March" with the year supplied by the other bound.
	dayMonth = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?[\s.]+([\p{L}]{3,12})\b`)

	// monthDayYear matches the US form "January 12, 2026".
	monthDayYear = regexp.MustCompile(`(?i)\b([\p{L}]{3,12})\.?\s+(\d{1,2})(?:st|nd|rd|th)?[\s,]+(\d{4})\b`)

	// numericDate matches "12.03.2026" and "12/03/2026" (day first, the
	// dominant convention outside the US).
	numericDate = regexp.MustCompile(`\b(\d{1,2})[./](\d{1,2})[./](\d{4})\b`)

	// isoDate matches "2026-03-12".
	isoDate = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)

	// separators are the dashes and words that divide the two ends of a range.
	separators = regexp.MustCompile(`(?i)\s*(?:–|—|-|‒|to|until|till|through|bis|jusqu'au|hasta|fino al|t/m)\s*`)

	// openEnded marks a listing that gives only a closing date.
	openEnded = regexp.MustCompile(`(?i)\b(until|till|through|ends?|bis|jusqu'au|hasta|fino al|t/m|closes)\b`)

	// openStart marks a listing that gives only an opening date.
	openStart = regexp.MustCompile(`(?i)\b(from|opens?|starting|ab|dès|desde|dal)\b`)

	// ongoing marks permanent or indefinite displays.
	ongoing = regexp.MustCompile(`(?i)\b(ongoing|permanent|indefinite|long[- ]term|dauerausstellung)\b`)
)

// qualifierWindow is how much of the text before a date is read for the word
// that says whether it opens or closes the run. Wide enough for "Until ",
// "Opening on " and their translations; far too narrow to reach a sentence
// about something else.
const qualifierWindow = 24

// ParseDateRange reads the run dates out of a listing's text.
//
// Museum listings are written for humans, so the same page may carry
// "12 March – 7 September 2026", "Until 3 Jan 2027" and "Ongoing". All three
// shapes are recognised; anything unrecognised yields a zero range, which the
// caller can treat as "dates unknown" rather than as a parse failure.
func ParseDateRange(text string, now time.Time) DateRange {
	text = strings.TrimSpace(text)
	if text == "" {
		return DateRange{}
	}

	dates := findDates(text, now)

	if ongoing.MatchString(text) {
		// The opening date is kept when the text gives one. "Permanent
		// exhibition Opens May 23 2026" says both that it has no end and when
		// it began, and the second half was being discarded — which left the
		// entry with nothing but a claim of permanence, indistinguishable from
		// a card that merely had the word "Dauerausstellung" somewhere in the
		// furniture around it.
		permanent := DateRange{Permanent: true}
		if len(dates) > 0 {
			start := dates[0].when
			permanent.Start = &start
		}
		return permanent
	}
	switch len(dates) {
	case 0:
		return DateRange{}

	case 1:
		single := dates[0].when
		// Read only the text just before the date, not the whole card.
		//
		// A qualifier means something because of where it sits. Searching the
		// whole card for it turned "From the Hasselblad Foundation Collection
		// March 23 – May 19, 2013" into an exhibition that opened in 2013 and
		// never closes: the "From" belongs to the collection, forty characters
		// away, and the run had ended twelve years earlier.
		lead := text[max(0, dates[0].at-qualifierWindow):dates[0].at]
		switch {
		case openEnded.MatchString(lead):
			return DateRange{End: &single}
		case openStart.MatchString(lead):
			return DateRange{Start: &single}
		default:
			// A bare date on a listing is its closing date more often than not,
			// but without a qualifier the safe reading is a single-day event.
			return DateRange{Start: &single, End: &single}
		}

	default:
		start, end := dates[0].when, dates[len(dates)-1].when
		if end.Before(start) {
			start, end = end, start
		}
		return DateRange{Start: &start, End: &end}
	}
}

// located is a date and where in the text it was written.
type located struct {
	at   int
	when time.Time
}

// findDates returns every date in the text, in order of appearance.
//
// The trailing year is shared backwards: "12 March – 7 September 2026" gives
// the year only once, and the opening date has to borrow it.
func findDates(text string, now time.Time) []located {
	var found []located
	claimed := make([]bool, len(text)+1)

	claim := func(start, end int) bool {
		for i := start; i < end && i < len(claimed); i++ {
			if claimed[i] {
				return false
			}
		}
		for i := start; i < end && i < len(claimed); i++ {
			claimed[i] = true
		}
		return true
	}

	for _, m := range isoDate.FindAllStringSubmatchIndex(text, -1) {
		if !claim(m[0], m[1]) {
			continue
		}
		year, _ := strconv.Atoi(text[m[2]:m[3]])
		month, _ := strconv.Atoi(text[m[4]:m[5]])
		day, _ := strconv.Atoi(text[m[6]:m[7]])
		if when, ok := makeDate(year, time.Month(month), day); ok {
			found = append(found, located{at: m[0], when: when})
		}
	}

	for _, m := range numericDate.FindAllStringSubmatchIndex(text, -1) {
		if !claim(m[0], m[1]) {
			continue
		}
		day, _ := strconv.Atoi(text[m[2]:m[3]])
		month, _ := strconv.Atoi(text[m[4]:m[5]])
		year, _ := strconv.Atoi(text[m[6]:m[7]])
		if when, ok := makeDate(year, time.Month(month), day); ok {
			found = append(found, located{at: m[0], when: when})
		}
	}

	for _, m := range dayMonthYear.FindAllStringSubmatchIndex(text, -1) {
		if !claim(m[0], m[1]) {
			continue
		}
		day, _ := strconv.Atoi(text[m[2]:m[3]])
		month, ok := lookupMonth(text[m[4]:m[5]])
		year, _ := strconv.Atoi(text[m[6]:m[7]])
		if !ok {
			continue
		}
		if when, ok := makeDate(year, month, day); ok {
			found = append(found, located{at: m[0], when: when})
		}
	}

	for _, m := range monthDayYear.FindAllStringSubmatchIndex(text, -1) {
		if !claim(m[0], m[1]) {
			continue
		}
		month, ok := lookupMonth(text[m[2]:m[3]])
		day, _ := strconv.Atoi(text[m[4]:m[5]])
		year, _ := strconv.Atoi(text[m[6]:m[7]])
		if !ok {
			continue
		}
		if when, ok := makeDate(year, month, day); ok {
			found = append(found, located{at: m[0], when: when})
		}
	}

	// Year-less dates borrow a year: the one from a dated sibling if there is
	// one, otherwise the current year.
	yearHint := now.Year()
	if len(found) > 0 {
		yearHint = found[len(found)-1].when.Year()
	}
	for _, m := range dayMonth.FindAllStringSubmatchIndex(text, -1) {
		if !claim(m[0], m[1]) {
			continue
		}
		day, _ := strconv.Atoi(text[m[2]:m[3]])
		month, ok := lookupMonth(text[m[4]:m[5]])
		if !ok {
			continue
		}
		if when, ok := makeDate(yearHint, month, day); ok {
			found = append(found, located{at: m[0], when: when})
		}
	}

	// Restore document order, which the per-pattern passes above lost.
	for i := 1; i < len(found); i++ {
		for j := i; j > 0 && found[j].at < found[j-1].at; j-- {
			found[j], found[j-1] = found[j-1], found[j]
		}
	}

	return found
}

// firstDateIndex returns the byte offset of the first date-looking run in text,
// or -1. It is used to cut a listing card's title away from its dates.
func firstDateIndex(text string) int {
	best := -1
	for _, re := range []*regexp.Regexp{isoDate, numericDate, dayMonthYear, monthDayYear, dayMonth} {
		if loc := re.FindStringIndex(text); loc != nil {
			if best == -1 || loc[0] < best {
				best = loc[0]
			}
		}
	}
	return best
}

// lookupMonth resolves a month name, matching on its first letters so that both
// full names and abbreviations work.
func lookupMonth(name string) (time.Month, bool) {
	name = strings.ToLower(strings.Trim(name, ". "))
	if month, ok := months[name]; ok {
		return month, true
	}
	for length := 4; length >= 3; length-- {
		if len([]rune(name)) < length {
			continue
		}
		prefix := string([]rune(name)[:length])
		if month, ok := months[prefix]; ok {
			return month, true
		}
	}
	return 0, false
}

// makeDate builds a UTC date, rejecting values that are out of range or
// implausible for an exhibition listing.
func makeDate(year int, month time.Month, day int) (time.Time, bool) {
	if year < 1900 || year > 2200 || month < time.January || month > time.December || day < 1 || day > 31 {
		return time.Time{}, false
	}
	when := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	// Reject overflow such as 31 February, which Go rolls into March.
	if when.Day() != day || when.Month() != month {
		return time.Time{}, false
	}
	return when, true
}
