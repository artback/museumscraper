package postgres

import (
	"context"
	"testing"
	"time"

	"museum/internal/models"
	"museum/internal/sweep"
	"museum/pkg/exhibitions"
)

// saveAt stores listings as though the read happened at a given moment, which
// is what first_seen_at and last_seen_at are taken from.
func saveAt(t *testing.T, store *Store, at time.Time, found ...exhibitions.Exhibition) {
	t.Helper()
	for i := range found {
		found[i].ScrapedAt = at
	}
	if _, err := store.SaveExhibitions(context.Background(), found); err != nil {
		t.Fatalf("save: %v", err)
	}
}

// listing builds a scraped exhibition on a site, at a fixed position.
func listing(site, slug, title string, end *time.Time) exhibitions.Exhibition {
	return exhibitions.Exhibition{
		URL:        "https://" + site + "/exhibitions/" + slug,
		SourcePage: "https://" + site + "/exhibitions",
		Title:      title,
		Museum:     "M",
		End:        end,
		Latitude:   48.86,
		Longitude:  2.35,
	}
}

func TestDueSites_NeverReadLeadThenMostOverdue(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "Fresh", Website: "https://fresh.example/", Latitude: 48.86, Longitude: 2.35, WikidataID: "Q1"},
		{Name: "Overdue", Website: "https://overdue.example/", Latitude: 48.86, Longitude: 2.35, WikidataID: "Q2"},
		{Name: "Untouched", Website: "https://untouched.example/", Latitude: 48.86, Longitude: 2.35, WikidataID: "Q3"},
		{Name: "Parked", Website: "https://parked.example/", Latitude: 48.86, Longitude: 2.35, WikidataID: "Q4"},
	}); err != nil {
		t.Fatalf("save museums: %v", err)
	}
	if _, err := store.DiscoverSites(ctx); err != nil {
		t.Fatalf("discover: %v", err)
	}

	// Read recently and not due for a month.
	mustRecord(t, store, "fresh.example", sweep.Plan{
		Interval: 30 * 24 * time.Hour, DueAt: now.Add(30 * 24 * time.Hour),
	}, sweep.Unchanged, now)
	// Due a week ago.
	mustRecord(t, store, "overdue.example", sweep.Plan{
		Interval: 7 * 24 * time.Hour, DueAt: now.Add(-7 * 24 * time.Hour),
	}, sweep.Unchanged, now)
	// Failed too often to keep paying for.
	mustRecord(t, store, "parked.example", sweep.Plan{
		Interval: 7 * 24 * time.Hour, DueAt: now.Add(-100 * 24 * time.Hour),
		Park: true, Reason: "parked after 6 consecutive failures",
	}, sweep.Failed, now)

	due, err := store.DueSites(ctx, now, 10)
	if err != nil {
		t.Fatalf("due sites: %v", err)
	}

	var got []string
	for _, site := range due {
		got = append(got, site.Site)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want the never-read one and the overdue one", got)
	}
	if got[0] != "untouched.example" {
		t.Errorf("first due is %q, want the site never read", got[0])
	}
	if got[1] != "overdue.example" {
		t.Errorf("second due is %q, want the overdue one", got[1])
	}
	if !due[0].NeverRead {
		t.Error("the never-read site should say so")
	}
	if due[1].NeverRead {
		t.Error("a site with a record is not never-read")
	}
}

// TestDueSites_CarriesValidatorsForward is what makes the conditional request
// possible at all: without these the sweep has nothing to ask the site about.
func TestDueSites_CarriesValidatorsForward(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "M", Website: "https://tagged.example/", Latitude: 48.86, Longitude: 2.35, WikidataID: "Q1"},
	}); err != nil {
		t.Fatalf("save museums: %v", err)
	}

	if err := store.RecordScrape(ctx, sweep.Record{
		Site:         "tagged.example",
		Plan:         sweep.Plan{Interval: time.Hour, DueAt: now.Add(-time.Hour)},
		Outcome:      sweep.Changed,
		FoundCount:   3,
		Fingerprint:  "abc123",
		ListingURL:   "https://tagged.example/whats-on",
		ETag:         `W/"deadbeef"`,
		LastModified: "Wed, 01 Jul 2026 10:00:00 GMT",
	}, now); err != nil {
		t.Fatalf("record: %v", err)
	}

	due, err := store.DueSites(ctx, now, 10)
	if err != nil {
		t.Fatalf("due sites: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("got %d due, want 1", len(due))
	}
	if due[0].ListingURL != "https://tagged.example/whats-on" {
		t.Errorf("ListingURL = %q", due[0].ListingURL)
	}
	if due[0].ETag != `W/"deadbeef"` {
		t.Errorf("ETag = %q", due[0].ETag)
	}
	if due[0].Fingerprint != "abc123" {
		t.Errorf("Fingerprint = %q", due[0].Fingerprint)
	}
}

// TestRecordScrape_KeepsTheLastGoodAnswerThroughAFailure: a site that fails
// must not lose the listing URL that worked, or the next sweep after it
// recovers has to rediscover it from scratch.
func TestRecordScrape_KeepsTheLastGoodAnswerThroughAFailure(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := store.RecordScrape(ctx, sweep.Record{
		Site: "flaky.example", Outcome: sweep.Changed, FoundCount: 5,
		Plan:       sweep.Plan{Interval: 7 * 24 * time.Hour, DueAt: now},
		ListingURL: "https://flaky.example/whats-on", ETag: `"v1"`, Fingerprint: "one",
	}, now); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := store.RecordScrape(ctx, sweep.Record{
		Site: "flaky.example", Outcome: sweep.Failed,
		Plan: sweep.Plan{Interval: 7 * 24 * time.Hour, DueAt: now.Add(-time.Hour), ConsecutiveFailures: 1},
	}, now); err != nil {
		t.Fatalf("record failure: %v", err)
	}

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "M", Website: "https://flaky.example/", Latitude: 48.86, Longitude: 2.35, WikidataID: "Q1"},
	}); err != nil {
		t.Fatalf("save museums: %v", err)
	}

	due, err := store.DueSites(ctx, now, 10)
	if err != nil {
		t.Fatalf("due sites: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("got %d due, want 1", len(due))
	}
	if due[0].ListingURL != "https://flaky.example/whats-on" {
		t.Errorf("ListingURL = %q, the failure erased where the listings live", due[0].ListingURL)
	}
	if due[0].State.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", due[0].State.ConsecutiveFailures)
	}
}

// TestRetireUnseen is the leak this whole file exists to close: before it,
// nothing removed a listing that vanished from a museum's site, and a
// permanent display — which carries no closing date — leaked forever.
func TestRetireUnseen(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	firstRead := time.Now().Add(-time.Hour)
	end := time.Now().AddDate(0, 2, 0)

	saveAt(t, store, firstRead,
		listing("museum.example", "kept", "Still listed", &end),
		listing("museum.example", "gone", "Taken down", &end),
		listing("other.example", "untouched", "Another site", &end))

	// The next read finds only one of them.
	secondRead := time.Now()
	saveAt(t, store, secondRead, listing("museum.example", "kept", "Still listed", &end))

	gone, err := store.RetireUnseen(ctx, "museum.example", secondRead)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if gone != 1 {
		t.Fatalf("retired %d, want only the one that vanished", gone)
	}

	live, err := store.ExhibitionsNearby(ctx, 48.8566, 2.3522, 2, false, 10)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	titles := map[string]bool{}
	for _, hit := range live {
		titles[hit.Title] = true
	}
	if titles["Taken down"] {
		t.Error("a retired listing is still being served")
	}
	if !titles["Still listed"] || !titles["Another site"] {
		t.Errorf("retirement reached too far: %v", titles)
	}
}

// TestSaveExhibitions_FirstSeenSurvivesAndRetirementIsUndone covers the two
// rules alerts will hang off: what is new stays new-dated, and a listing that
// comes back is on show again.
func TestSaveExhibitions_FirstSeenSurvivesAndRetirementIsUndone(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	end := time.Now().AddDate(0, 2, 0)
	first := time.Now().Add(-48 * time.Hour)

	saveAt(t, store, first, listing("museum.example", "show", "A show", &end))
	if _, err := store.RetireUnseen(ctx, "museum.example", time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if live, _ := store.ExhibitionsNearby(ctx, 48.8566, 2.3522, 2, false, 10); len(live) != 0 {
		t.Fatalf("expected the listing retired, got %d live", len(live))
	}

	// It reappears on the site.
	again := time.Now()
	saveAt(t, store, again, listing("museum.example", "show", "A show", &end))

	live, err := store.ExhibitionsNearby(ctx, 48.8566, 2.3522, 2, false, 10)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("got %d live, want the returned listing", len(live))
	}

	var firstSeen, lastSeen time.Time
	if err := store.pool.QueryRow(ctx,
		`SELECT first_seen_at, last_seen_at FROM exhibitions WHERE url = $1`,
		"https://museum.example/exhibitions/show").Scan(&firstSeen, &lastSeen); err != nil {
		t.Fatalf("read timestamps: %v", err)
	}
	if firstSeen.After(first.Add(time.Second)) {
		t.Errorf("first_seen_at moved forward to %s, want it held at %s", firstSeen, first)
	}
	if lastSeen.Before(again.Add(-time.Second)) {
		t.Errorf("last_seen_at = %s, want it advanced to %s", lastSeen, again)
	}
}

func TestSoonestClose_IgnoresPermanentAndRetired(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	soon := time.Now().AddDate(0, 0, 10).Truncate(24 * time.Hour)
	later := time.Now().AddDate(0, 3, 0)

	permanent := listing("museum.example", "always", "Always on", nil)
	permanent.Permanent = true

	if _, err := store.SaveExhibitions(ctx, []exhibitions.Exhibition{
		listing("museum.example", "later", "Closes later", &later),
		listing("museum.example", "soon", "Closes soon", &soon),
		permanent,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := store.SoonestClose(ctx, "museum.example")
	if err != nil {
		t.Fatalf("soonest: %v", err)
	}
	if got == nil {
		t.Fatal("no closing date found")
	}
	if !got.Truncate(24 * time.Hour).Equal(soon) {
		t.Errorf("soonest = %s, want %s", got, soon)
	}
}

// TestExhibitionCoverage_KnowsAnAreaWasReadAndFoundNothing is the ambiguity
// the scrape record exists to remove: an area whose every museum was read this
// morning and published nothing used to report "nobody has looked here yet".
func TestExhibitionCoverage_KnowsAnAreaWasReadAndFoundNothing(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "Quiet", Website: "https://quiet.example/", Latitude: 48.86, Longitude: 2.35, WikidataID: "Q1"},
	}); err != nil {
		t.Fatalf("save museums: %v", err)
	}

	before, err := store.ExhibitionCoverage(ctx, 48.8566, 2.3522, 2)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if before.LastScraped != nil {
		t.Error("nothing has been read yet, but coverage claims otherwise")
	}

	// Read it; it publishes nothing at all.
	mustRecord(t, store, "quiet.example", sweep.Plan{
		Interval: 7 * 24 * time.Hour, DueAt: now.Add(7 * 24 * time.Hour),
	}, sweep.Unchanged, now)

	after, err := store.ExhibitionCoverage(ctx, 48.8566, 2.3522, 2)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if after.LastScraped == nil {
		t.Fatal("the area was read and found empty, but coverage still says nobody has looked")
	}
	if after.MuseumsWithSite != 1 {
		t.Errorf("MuseumsWithSite = %d, want 1", after.MuseumsWithSite)
	}
}

func mustRecord(t *testing.T, store *Store, site string, plan sweep.Plan, outcome sweep.Outcome, now time.Time) {
	t.Helper()
	if err := store.RecordScrape(context.Background(), sweep.Record{
		Site: site, Plan: plan, Outcome: outcome,
	}, now); err != nil {
		t.Fatalf("record scrape for %s: %v", site, err)
	}
}
