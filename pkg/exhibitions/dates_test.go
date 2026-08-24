package exhibitions

import (
	"testing"
	"time"
)

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func TestParseDateRange(t *testing.T) {
	now := date(2026, time.July, 27)

	cases := []struct {
		name      string
		text      string
		wantStart *time.Time
		wantEnd   *time.Time
		wantZero  bool
		// wantPermanent expects a range that says the display is always on
		// rather than giving it dates.
		wantPermanent bool
	}{
		{
			name:      "explicit range with shared year",
			text:      "12 March – 7 September 2026",
			wantStart: ptr(date(2026, time.March, 12)),
			wantEnd:   ptr(date(2026, time.September, 7)),
		},
		{
			name:      "range with both years",
			text:      "3 October 2026 – 11 January 2027",
			wantStart: ptr(date(2026, time.October, 3)),
			wantEnd:   ptr(date(2027, time.January, 11)),
		},
		{
			// The real Tate listing that motivated this parser.
			name:    "until closing date only",
			text:    "Exhibition Frida: The Making of an Icon Tate Modern Until 3 Jan 2027",
			wantEnd: ptr(date(2027, time.January, 3)),
		},
		{
			name:      "from opening date only",
			text:      "From 5 March 2026",
			wantStart: ptr(date(2026, time.March, 5)),
		},
		{
			name:      "US month-first form",
			text:      "January 12, 2026 – April 5, 2026",
			wantStart: ptr(date(2026, time.January, 12)),
			wantEnd:   ptr(date(2026, time.April, 5)),
		},
		{
			name:      "numeric European form",
			text:      "12.03.2026 – 07.09.2026",
			wantStart: ptr(date(2026, time.March, 12)),
			wantEnd:   ptr(date(2026, time.September, 7)),
		},
		{
			name:      "ISO form",
			text:      "2026-03-12 to 2026-09-07",
			wantStart: ptr(date(2026, time.March, 12)),
			wantEnd:   ptr(date(2026, time.September, 7)),
		},
		{
			name:      "German bis",
			text:      "15. Mai bis 30. August 2026",
			wantStart: ptr(date(2026, time.May, 15)),
			wantEnd:   ptr(date(2026, time.August, 30)),
		},
		{
			name:      "French jusqu'au",
			text:      "jusqu'au 7 septembre 2026",
			wantEnd:   ptr(date(2026, time.September, 7)),
			wantStart: nil,
		},
		{
			// Not "starts today": a permanent display carries no dates, so
			// that reading re-dated it on every scrape.
			name:          "ongoing carries no dates",
			text:          "Ongoing",
			wantPermanent: true,
		},
		{
			name:          "permanent display",
			text:          "Permanent collection",
			wantPermanent: true,
		},
		{
			name:     "no dates at all",
			text:     "Visit our galleries",
			wantZero: true,
		},
		{
			name:     "impossible date is rejected",
			text:     "31 February 2026",
			wantZero: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseDateRange(tc.text, now)

			if got.Permanent != tc.wantPermanent {
				t.Fatalf("Permanent = %v, want %v", got.Permanent, tc.wantPermanent)
			}
			if tc.wantPermanent {
				if !got.IsZero() {
					t.Fatalf("a permanent range should carry no dates, got start=%v end=%v", got.Start, got.End)
				}
				if !got.Runs(now) {
					t.Error("a permanent range should be running")
				}
				return
			}
			if tc.wantZero {
				if !got.IsZero() {
					t.Fatalf("expected no dates, got start=%v end=%v", got.Start, got.End)
				}
				return
			}
			if got.IsZero() {
				t.Fatal("expected dates, got none")
			}
			assertDate(t, "start", got.Start, tc.wantStart)
			assertDate(t, "end", got.End, tc.wantEnd)
		})
	}
}

func TestDateRange_RunsAndUpcoming(t *testing.T) {
	now := date(2026, time.July, 27)

	cases := []struct {
		name         string
		r            DateRange
		wantRuns     bool
		wantUpcoming bool
	}{
		{
			name:     "currently on",
			r:        DateRange{Start: ptr(date(2026, time.March, 1)), End: ptr(date(2026, time.September, 1))},
			wantRuns: true,
		},
		{
			name: "already closed",
			r:    DateRange{Start: ptr(date(2025, time.March, 1)), End: ptr(date(2025, time.September, 1))},
		},
		{
			name:         "not open yet",
			r:            DateRange{Start: ptr(date(2026, time.October, 1)), End: ptr(date(2026, time.December, 1))},
			wantUpcoming: true,
		},
		{
			name:     "open-ended closing date in the future",
			r:        DateRange{End: ptr(date(2027, time.January, 3))},
			wantRuns: true,
		},
		{
			name: "open-ended closing date in the past",
			r:    DateRange{End: ptr(date(2025, time.January, 3))},
		},
		{
			name:     "starts today",
			r:        DateRange{Start: ptr(now)},
			wantRuns: true,
		},
		{
			name: "no dates runs never",
			r:    DateRange{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.Runs(now); got != tc.wantRuns {
				t.Errorf("Runs = %v, want %v", got, tc.wantRuns)
			}
			if got := tc.r.Upcoming(now); got != tc.wantUpcoming {
				t.Errorf("Upcoming = %v, want %v", got, tc.wantUpcoming)
			}
		})
	}
}

func assertDate(t *testing.T, label string, got, want *time.Time) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s = %v, want none", label, got.Format("2006-01-02"))
	case want != nil && got == nil:
		t.Errorf("%s = none, want %v", label, want.Format("2006-01-02"))
	case want != nil && got != nil && !got.Equal(*want):
		t.Errorf("%s = %v, want %v", label, got.Format("2006-01-02"), want.Format("2006-01-02"))
	}
}

func ptr(t time.Time) *time.Time { return &t }

// TestParseDateRangeAcrossTheYear covers the commonest shape a museum's
// programme takes and the way it used to be read backwards.
//
// A winter run is written without years — "24 sep - 28 feb" — so both bounds
// were dated to the current year, the end landed before the start, and they
// were swapped. The result was an exhibition recorded as running the wrong
// seven months: 28 February to 24 September, of a year it was never on.
func TestParseDateRangeAcrossTheYear(t *testing.T) {
	now := date(2026, time.August, 15)

	tests := []struct {
		name       string
		text       string
		start, end time.Time
	}{
		{
			// Verbatim from Kalmar läns museum.
			name:  "Swedish, no years, over the winter",
			text:  "24 sep - 28 feb",
			start: date(2026, time.September, 24), end: date(2027, time.February, 28),
		},
		{
			name:  "English, no years, over the winter",
			text:  "12 November – 3 March",
			start: date(2026, time.November, 12), end: date(2027, time.March, 3),
		},
		{
			// Within one year, no rolling.
			name:  "no years, same year",
			text:  "3 March – 12 November",
			start: date(2026, time.March, 3), end: date(2026, time.November, 12),
		},
		{
			// Years written out and in order: unchanged.
			name:  "explicit years over the turn",
			text:  "24 september 2026 – 28 februari 2027",
			start: date(2026, time.September, 24), end: date(2027, time.February, 28),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseDateRange(tt.text, now)
			if got.Start == nil || got.End == nil {
				t.Fatalf("ParseDateRange(%q) = %+v, want both bounds", tt.text, got)
			}
			if !got.Start.Equal(tt.start) || !got.End.Equal(tt.end) {
				t.Errorf("ParseDateRange(%q) = %s to %s, want %s to %s",
					tt.text, got.Start.Format(time.DateOnly), got.End.Format(time.DateOnly),
					tt.start.Format(time.DateOnly), tt.end.Format(time.DateOnly))
			}
			if got.End.Before(*got.Start) {
				t.Errorf("ParseDateRange(%q) ended before it started", tt.text)
			}
		})
	}
}

// TestParseDateRange_Languages covers the month table across the scripts the
// catalogue actually meets. Each case is a range as a museum in that country
// writes it, and every one of them read as nothing before the table was
// widened — which meant the entry was dropped, since a listing that cannot be
// placed in time is not kept.
func TestParseDateRange_Languages(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	tests := []struct{ name, text, start, end string }{
		{"polish", "3 września 2026 - 15 stycznia 2027", "2026-09-03", "2027-01-15"},
		{"czech", "3. září 2026 - 15. ledna 2027", "2026-09-03", "2027-01-15"},
		{"hungarian", "2026. szeptember 3. - 2027. január 15.", "2026-09-03", "2027-01-15"},
		{"finnish", "3. syyskuuta 2026 - 15. tammikuuta 2027", "2026-09-03", "2027-01-15"},
		{"russian", "3 сентября 2026 - 15 января 2027", "2026-09-03", "2027-01-15"},
		{"greek", "3 Σεπτεμβρίου 2026 - 15 Ιανουαρίου 2027", "2026-09-03", "2027-01-15"},
		{"turkish", "3 Eylül 2026 - 15 Ocak 2027", "2026-09-03", "2027-01-15"},
		{"romanian", "3 septembrie 2026 - 15 ianuarie 2027", "2026-09-03", "2027-01-15"},
		{"croatian", "3. rujna 2026. - 15. siječnja 2027.", "2026-09-03", "2027-01-15"},
		{"japanese", "2026年9月3日 - 2027年1月15日", "2026-09-03", "2027-01-15"},
		{"korean", "2026년 9월 3일 - 2027년 1월 15일", "2026-09-03", "2027-01-15"},

		// The traps. Each of these resolves to a different month under the
		// three-letter prefix rule that carries English, and each is why its
		// language is registered the way it is.
		//
		// Finnish "marraskuuta" is November, not March.
		{"finnish november", "3. marraskuuta 2026 - 15. tammikuuta 2027", "2026-11-03", "2027-01-15"},
		// Czech "června" and "července" share four letters and a month apart.
		{"czech june and july", "3. června 2026 - 15. července 2026", "2026-06-03", "2026-07-15"},
		// Polish July against Croatian July, which are different months in
		// the other's language and are spelled differently in both.
		{"polish july", "3 lipca 2026 - 15 sierpnia 2026", "2026-07-03", "2026-08-15"},
		{"croatian july", "3. srpnja 2026. - 15. kolovoza 2026.", "2026-07-03", "2026-08-15"},
		// Greek "Ιουνίου" and "Ιουλίου" share three letters.
		{"greek june", "3 Ιουνίου 2026 - 15 Ιουλίου 2026", "2026-06-03", "2026-07-15"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseDateRange(tt.text, now)
			if got.Start == nil || got.Start.Format("2006-01-02") != tt.start {
				t.Errorf("Start = %v, want %s", got.Start, tt.start)
			}
			if got.End == nil || got.End.Format("2006-01-02") != tt.end {
				t.Errorf("End = %v, want %s", got.End, tt.end)
			}
		})
	}
}

// TestParseDateRange_AmbiguousMonthIsRefused holds the line on the one
// spelling that names two different months. Reading it either way dates half
// the listings that use it a month wrong, and does so silently.
func TestParseDateRange_AmbiguousMonthIsRefused(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	// Croatian for "3 October 2026 to 15 November 2026"; the same word is
	// November in Polish.
	got := ParseDateRange("3. listopada 2026. - 15. studenoga 2026.", now)

	if got.Start != nil {
		t.Errorf("Start = %v, want nothing: \"listopada\" cannot be resolved without knowing the language", got.Start)
	}
	if got.End == nil || got.End.Format("2006-01-02") != "2026-11-15" {
		t.Errorf("End = %v, want 2026-11-15: the bound that was readable should survive", got.End)
	}
}

// TestParseDateRange_HalfReadRangeStaysOpen is the bug the widening exposed.
//
// One bound read and the other in a month this table does not know is a run
// that is open at one end, not a one-day event. Recording it as a single day
// invents a closing date and then loses the entry entirely, because a single
// day is read as an event rather than an exhibition.
func TestParseDateRange_HalfReadRangeStaysOpen(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		text      string
		wantStart string
		wantEnd   string
	}{
		{
			// Ukrainian, which is not in the table: the opening date reads
			// and the closing one does not.
			name:      "unknown month closes the run",
			text:      "3 вересня 2026 - 15 sichnya 2027",
			wantStart: "",
			wantEnd:   "",
		},
		{
			name:      "unknown month opens the run",
			text:      "3 sichnya 2026 - 15 September 2026",
			wantStart: "",
			wantEnd:   "2026-09-15",
		},
		{
			name:      "unknown month closes a run that opened readably",
			text:      "3 September 2026 - 15 sichnya 2027",
			wantStart: "2026-09-03",
			wantEnd:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseDateRange(tt.text, now)

			switch {
			case tt.wantStart == "" && got.Start != nil:
				t.Errorf("Start = %v, want nothing", got.Start)
			case tt.wantStart != "" && (got.Start == nil || got.Start.Format("2006-01-02") != tt.wantStart):
				t.Errorf("Start = %v, want %s", got.Start, tt.wantStart)
			}
			switch {
			case tt.wantEnd == "" && got.End != nil:
				t.Errorf("End = %v, want nothing: a half-read range must not invent the other bound", got.End)
			case tt.wantEnd != "" && (got.End == nil || got.End.Format("2006-01-02") != tt.wantEnd):
				t.Errorf("End = %v, want %s", got.End, tt.wantEnd)
			}
		})
	}
}
