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

// months maps month names to numbers.
//
// Matching is on the whole word first and then on the first four and three
// letters, so "January", "januari" and "Jan" all resolve from one entry. Where
// a language's forms would collide with another's under that prefix rule, the
// full inflected forms are listed instead and no prefix is registered — see
// lookupMonth and ambiguousMonths.
//
// This table is the single place the catalogue knows what a month is called.
// Generated extractors reach it through museum.dates, so a language added here
// is a language every stored extractor can suddenly read, without regenerating
// any of them.
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

	// Nordic. lookupMonth already matches three- and four-letter prefixes, so
	// "mars", "juni" and "augusti" resolved through the English entries and
	// only these two were genuinely missing — but "maj" is Swedish, Danish and
	// Norwegian for May, so the shared parser silently failed on a twelfth of
	// every Scandinavian listing. That gap is why each extractor generated for
	// a Swedish museum wrote its own month table instead of using this one.
	"maj": time.May, "des": time.December,

	// Finnish. "marraskuu" is November and would resolve to March on the
	// three-letter rule, so the four-letter prefixes carry this language and
	// none of its three-letter ones are registered.
	"tamm": time.January, "helm": time.February, "maal": time.March,
	"huht": time.April, "touk": time.May, "kesä": time.June,
	"kesa": time.June, "hein": time.July, "elok": time.August,
	"syys": time.September, "loka": time.October, "marr": time.November,
	"joul": time.December,

	// Hungarian. Only the accented forms are missing; "january", "február",
	// "augusztus", "október", "november" and "december" already resolve.
	"már": time.March, "ápr": time.April, "máj": time.May,
	"jún": time.June, "júl": time.July, "szep": time.September,

	// Romanian
	"ian": time.January, "iun": time.June, "iul": time.July,
	"noi": time.November,

	// Polish, in the genitive a date is written in. "lipca" (July) and
	// "listopada" (November) are spelled exactly as Croatian months meaning
	// something else; see ambiguousMonths for what happens to those.
	"styc": time.January, "lut": time.February, "kwie": time.April,
	"czer": time.June, "lipca": time.July, "lipiec": time.July,
	"sier": time.August, "wrze": time.September, "paźd": time.October,
	"pazd": time.October, "grud": time.December,

	// Czech. "červen" (June) and "červenec" (July) share four letters, so
	// both are listed in full and no červ- prefix is registered — a prefix
	// here would date half the Czech summer a month early.
	"led": time.January, "únor": time.February, "unor": time.February,
	"února": time.February, "unora": time.February,
	"břez": time.March, "brez": time.March, "dub": time.April,
	"květ": time.May, "kvet": time.May,
	"červen": time.June, "června": time.June,
	"cerven": time.June, "cervna": time.June,
	"červenec": time.July, "července": time.July,
	"cervenec": time.July, "cervence": time.July,
	"srpen": time.August, "srpna": time.August,
	"září": time.September, "zari": time.September,
	"říjen": time.October, "října": time.October,
	"rijen": time.October, "rijna": time.October,
	"listopadu": time.November,
	"pros":      time.December,

	// Croatian. Its July and August are spelled almost as Czech's August and
	// September, so like Czech these are listed in full rather than by prefix.
	"sije": time.January, "siječ": time.January, "velj": time.February,
	"ožuj": time.March, "ozuj": time.March, "trav": time.April,
	"svib": time.May, "lipanj": time.June, "lipnja": time.June,
	"srpanj": time.July, "srpnja": time.July, "kolo": time.August,
	"rujan": time.September, "rujna": time.September,
	"stud": time.November,

	// Russian, in the genitive a date is written in.
	"янва": time.January, "янв": time.January, "февр": time.February,
	"март": time.March, "мар": time.March, "апре": time.April,
	"апр": time.April, "мая": time.May, "май": time.May,
	"июня": time.June, "июнь": time.June, "июля": time.July,
	"июль": time.July, "авгу": time.August, "авг": time.August,
	"сент": time.September, "октя": time.October, "окт": time.October,
	"нояб": time.November, "ноя": time.November, "дека": time.December,
	"дек": time.December,

	// Greek, in the genitive a date is written in. "Ιουνίου" and "Ιουλίου"
	// share three letters, so this language is carried on four-letter
	// prefixes only.
	"ιανο": time.January, "φεβρ": time.February, "μαρτ": time.March,
	"απρι": time.April, "μαΐο": time.May, "μαιο": time.May,
	"ιουν": time.June, "ιουλ": time.July, "αυγο": time.August,
	"σεπτ": time.September, "οκτω": time.October, "νοεμ": time.November,
	"δεκε": time.December,

	// Turkish
	"oca": time.January, "şub": time.February, "sub": time.February,
	"nisa": time.April, "mayı": time.May, "mayi": time.May,
	"hazi": time.June, "temm": time.July, "ağus": time.August,
	"agus": time.August, "eylü": time.September, "eylu": time.September,
	"eki": time.October, "kası": time.November, "kasi": time.November,
	"aral": time.December,
}

// ambiguousMonths are spellings that name different months in different
// languages, and are refused rather than guessed at.
//
// "listopada" is November in Polish and October in Croatian — the same nine
// letters, and nothing in a listing's text says which language it is in.
// Resolving it either way would date one of the two a month wrong, silently,
// on every listing that used it. A missing date drops the entry, which is
// visible as an absence; a wrong one is published as fact.
//
// The other near-collisions are not here because the languages spell their
// inflected forms differently — Polish "lipca" against Croatian "lipnja",
// Czech "srpna" against Croatian "srpnja" — and are listed in months in full.
var ambiguousMonths = map[string]bool{
	"listopad":  true,
	"listopada": true,
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

	// cjkDate matches "2026年9月3日" and the Korean "2026년 9월 3일".
	//
	// No month table can help here: the month is a number and the character
	// after it says which field it is. That makes this the one date form that
	// needs no language knowledge at all, and it covers Japanese, Chinese and
	// Korean listings together.
	cjkDate = regexp.MustCompile(`(\d{4})\s*[年년]\s*(\d{1,2})\s*[月월]\s*(\d{1,2})\s*[日일]`)

	// yearMonthDay matches "2026. szeptember 3.", the Hungarian order, which
	// no other pattern here reads: the year leads and the day trails.
	//
	// Safe despite how loose it looks, because the middle group has to resolve
	// to a month name for the match to be used at all — any other word between
	// a year and a number fails the lookup and the match is discarded.
	yearMonthDay = regexp.MustCompile(`\b(\d{4})\.?\s+([\p{L}]{3,12})\.?\s+(\d{1,2})\b`)

	// separators are the dashes and words that divide the two ends of a range.
	separators = regexp.MustCompile(`(?i)\s*(?:–|—|-|‒|〜|～|ー|to|until|till|through|bis|jusqu'au|hasta|fino al|t/m)\s*`)

	// openEnded marks a listing that gives only a closing date.
	//
	// The Scandinavian forms are written with dots, so they cannot carry a
	// trailing word boundary — "t.o.m." ends on punctuation. Their absence is
	// why extractors generated for Swedish museums wrote their own range
	// parsers: the shared one read "t.o.m. 15 januari" as an opening date and
	// listed every closing show as though it were about to start.
	openEnded = regexp.MustCompile(`(?i)(\bt\.?\s?o\.?\s?m\.?|\btill och med\b|\bfram till\b|\bsenast\b|\b(until|till|through|ends?|bis|jusqu'au|hasta|fino al|t/m|closes)\b)`)

	// openStart marks a listing that gives only an opening date.
	openStart = regexp.MustCompile(`(?i)(\bfr\.?\s?o\.?\s?m\.?|\b(from|opens?|starting|ab|dès|desde|dal|från|fra|öppnar|premiär)\b)`)

	// explicitYear reports that the text dated itself, so a reversed range is
	// the site's mistake rather than a run over the turn of the year.
	explicitYear = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)

	// ongoing marks permanent or indefinite displays.
	ongoing = regexp.MustCompile(`(?i)\b(ongoing|permanent|indefinite|long[- ]term|dauerausstellung|tills vidare|basutställning|fast utställning|fasta utställningar)\b`)
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

	dates, unread := findDates(text, now)

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
		case len(unread) > 0:
			// Half a range: one bound read, and a second date-shaped run whose
			// month this table does not know. Collapsing that into a
			// single-day event is the worst of the three readings available —
			// it invents a closing date, and reads as a one-day event, which
			// the caller then discards as not being an exhibition at all.
			//
			// A Romanian listing showed the shape. "3 septembrie 2026 -
			// 15 ianuarie 2027" resolved September through the "sep" prefix
			// and nothing for "ianuarie", and a four-month run was recorded as
			// one day in September. Every language with partial coverage here
			// does the same, which is why the unread bound is reported rather
			// than dropped: the run is open at the end this parser could not
			// read, and open is true.
			if dates[0].at < unread[0] {
				return DateRange{Start: &single}
			}
			return DateRange{End: &single}

		default:
			// A bare date on a listing is its closing date more often than not,
			// but without a qualifier the safe reading is a single-day event.
			return DateRange{Start: &single, End: &single}
		}

	default:
		start, end := dates[0].when, dates[len(dates)-1].when
		if end.Before(start) {
			// A range whose end falls before its start is one of two things,
			// and they need opposite treatment.
			//
			// With no year in the text — "24 sep - 28 feb", which is how a
			// museum writes a run over the winter — both bounds were dated to
			// the current year, so the end belongs to the next one. Swapping
			// them instead produced "28 February to 24 September": an
			// exhibition running the wrong seven months of the wrong year,
			// silently, for every autumn-to-spring show in the catalogue.
			//
			// With years written out, a reversed pair is the site's own error
			// and there is nothing better to do than order them.
			if explicitYear.MatchString(text) {
				start, end = end, start
			} else {
				end = end.AddDate(1, 0, 0)
			}
		}
		return DateRange{Start: &start, End: &end}
	}
}

// located is a date and where in the text it was written.
type located struct {
	at   int
	to   int
	when time.Time
}

// findDates returns every date in the text, in order of appearance.
//
// The trailing year is shared backwards: "12 March – 7 September 2026" gives
// the year only once, and the opening date has to borrow it.
func findDates(text string, now time.Time) (found []located, unread []int) {
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
			found = append(found, located{at: m[0], to: m[1], when: when})
		}
	}

	for _, m := range cjkDate.FindAllStringSubmatchIndex(text, -1) {
		if !claim(m[0], m[1]) {
			continue
		}
		year, _ := strconv.Atoi(text[m[2]:m[3]])
		month, _ := strconv.Atoi(text[m[4]:m[5]])
		day, _ := strconv.Atoi(text[m[6]:m[7]])
		if when, ok := makeDate(year, time.Month(month), day); ok {
			found = append(found, located{at: m[0], to: m[1], when: when})
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
			found = append(found, located{at: m[0], to: m[1], when: when})
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
			unread = append(unread, m[0])
			continue
		}
		if when, ok := makeDate(year, month, day); ok {
			found = append(found, located{at: m[0], to: m[1], when: when})
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
			unread = append(unread, m[0])
			continue
		}
		if when, ok := makeDate(year, month, day); ok {
			found = append(found, located{at: m[0], to: m[1], when: when})
		}
	}

	for _, m := range yearMonthDay.FindAllStringSubmatchIndex(text, -1) {
		year, _ := strconv.Atoi(text[m[2]:m[3]])
		month, ok := lookupMonth(text[m[4]:m[5]])
		day, _ := strconv.Atoi(text[m[6]:m[7]])
		if !ok {
			// Not a date at all — a year, a word and a number that happen to
			// sit together. Claiming the span first would eat text the
			// year-less pass below can still read, so the claim is made only
			// once the month has resolved.
			continue
		}
		if !claim(m[0], m[1]) {
			continue
		}
		if when, ok := makeDate(year, month, day); ok {
			found = append(found, located{at: m[0], to: m[1], when: when})
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
			unread = append(unread, m[0])
			continue
		}
		if when, ok := makeDate(yearHint, month, day); ok {
			found = append(found, located{at: m[0], to: m[1], when: when})
		}
	}

	// Restore document order, which the per-pattern passes above lost.
	for i := 1; i < len(found); i++ {
		for j := i; j > 0 && found[j].at < found[j-1].at; j-- {
			found[j], found[j-1] = found[j-1], found[j]
		}
	}

	return found, unread
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
	if ambiguousMonths[name] {
		return 0, false
	}
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
