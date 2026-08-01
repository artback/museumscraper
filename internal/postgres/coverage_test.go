package postgres

import (
	"context"
	"testing"
	"time"

	"museum/internal/models"
	"museum/internal/sweep"
)

// TestTargetsNear_CarriesStateAndSkipsParked checks that the on-demand path
// gets the same state the scheduled sweep does — without it, an area scrape
// would restart every site's learned interval from scratch and would keep
// retrying hosts the sweep has already given up on.
func TestTargetsNear_CarriesStateAndSkipsParked(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "Known", Website: "https://known.example/", Latitude: 48.86, Longitude: 2.35, WikidataID: "Q1"},
		{Name: "Dead", Website: "https://dead.example/", Latitude: 48.86, Longitude: 2.35, WikidataID: "Q2"},
		{Name: "Far away", Website: "https://faraway.example/", Latitude: -33.86, Longitude: 151.2, WikidataID: "Q3"},
	}); err != nil {
		t.Fatalf("save museums: %v", err)
	}
	if _, err := store.DiscoverSites(ctx); err != nil {
		t.Fatalf("discover: %v", err)
	}

	if err := store.RecordScrape(ctx, sweep.Record{
		Site:    "known.example",
		Outcome: sweep.Unchanged,
		Plan: sweep.Plan{
			Interval: 21 * 24 * time.Hour,
			DueAt:    now.Add(21 * 24 * time.Hour),
		},
		ListingURL: "https://known.example/whats-on", ETag: `"v9"`, Fingerprint: "digest",
	}, now); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := store.RecordScrape(ctx, sweep.Record{
		Site:    "dead.example",
		Outcome: sweep.Failed,
		Plan: sweep.Plan{
			Interval: 7 * 24 * time.Hour, DueAt: now.Add(60 * 24 * time.Hour),
			Park: true, Reason: "parked after 6 consecutive failures", ConsecutiveFailures: 6,
		},
	}, now); err != nil {
		t.Fatalf("record: %v", err)
	}

	targets, err := store.TargetsNear(ctx, 48.8566, 2.3522, 5, 20)
	if err != nil {
		t.Fatalf("targets near: %v", err)
	}

	if len(targets) != 1 {
		t.Fatalf("got %d targets, want only the live one nearby: %+v", len(targets), targets)
	}
	got := targets[0]
	if got.Site != "known.example" {
		t.Fatalf("Site = %q", got.Site)
	}
	// Not due for three weeks, but an area scrape asks anyway — someone is
	// looking at it. What matters is that it arrives with its learned state.
	if got.State.Interval != 21*24*time.Hour {
		t.Errorf("Interval = %s, want the learned 504h", got.State.Interval)
	}
	if got.ETag != `"v9"` || got.Fingerprint != "digest" {
		t.Errorf("state not carried: etag=%q fingerprint=%q", got.ETag, got.Fingerprint)
	}
}

// TestExhibitionCoverage_CountsAnOnDemandScrape is the regression this
// unification fixes. Coverage reads the attempt record; when the on-demand
// path did not write one, an area the API had just scraped itself still told
// callers that nobody had ever looked at it.
func TestExhibitionCoverage_CountsAnOnDemandScrape(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now()

	if _, err := store.SaveMuseums(ctx, []models.Museum{
		{Name: "M", Website: "https://ondemand.example/", Latitude: 48.86, Longitude: 2.35, WikidataID: "Q1"},
	}); err != nil {
		t.Fatalf("save museums: %v", err)
	}
	if _, err := store.DiscoverSites(ctx); err != nil {
		t.Fatalf("discover: %v", err)
	}

	targets, err := store.TargetsNear(ctx, 48.8566, 2.3522, 5, 20)
	if err != nil {
		t.Fatalf("targets near: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(targets))
	}

	// What the runner does after reading a site that published nothing.
	if err := store.RecordScrape(ctx, sweep.Record{
		Site:    targets[0].Site,
		Outcome: sweep.Unchanged,
		Plan:    sweep.Plan{Interval: 7 * 24 * time.Hour, DueAt: now.Add(7 * 24 * time.Hour)},
	}, now); err != nil {
		t.Fatalf("record: %v", err)
	}

	coverage, err := store.ExhibitionCoverage(ctx, 48.8566, 2.3522, 5)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if coverage.LastScraped == nil {
		t.Fatal("the API scraped this area, but coverage still reports nobody has looked")
	}
}
