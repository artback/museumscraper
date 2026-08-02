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

// The date vocabulary has to reach past English, because a missing word does
// not merely lose a qualifier — it changes the answer.
//
// With one date and nothing saying which end it is, the parser reads a
// single-day event. "À partir du 23 mai 2026" therefore recorded an exhibition
// as opening and closing on the same day, and it vanished from "what is on"
// the next morning, while the English and German phrasings were read correctly.
func TestParseDateRange_BeyondEnglish(t *testing.T) {
	now := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		text       string
		start, end string
		permanent  bool
		why        string
	}{
		{text: "À partir du 23 mai 2026", start: "2026-05-23", why: "French open start"},
		{text: "Från 23 maj 2026", start: "2026-05-23", why: "Swedish open start, and a Swedish May"},
		{text: "vanaf 12 maart 2026", start: "2026-03-12", why: "Dutch open start, and a Dutch March"},
		{text: "Du 23 mai au 30 août 2026", start: "2026-05-23", end: "2026-08-30", why: "French range"},
		{text: "Jusqu'au 5 octobre 2026", end: "2026-10-05", why: "French open end"},
		// "permanent" carries most of Europe, but only if it may take an ending.
		{text: "Exposition permanente", permanent: true, why: "French"},
		{text: "esposizione permanente", permanent: true, why: "Italian"},
		{text: "colección permanente", permanent: true, why: "Spanish"},
		{text: "Permanent exhibition", permanent: true, why: "English, which always worked"},
	}

	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			got := ParseDateRange(tc.text, now)
			if got.Permanent != tc.permanent {
				t.Errorf("permanent = %v, want %v (%s)", got.Permanent, tc.permanent, tc.why)
			}
			if day(got.Start) != tc.start {
				t.Errorf("start = %q, want %q (%s)", day(got.Start), tc.start, tc.why)
			}
			if day(got.End) != tc.end {
				t.Errorf("end = %q, want %q (%s)", day(got.End), tc.end, tc.why)
			}
		})
	}
}

// day formats a bound for comparison, with "" for absent.
func day(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02")
}
