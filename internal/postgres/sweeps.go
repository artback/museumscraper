package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"museum/internal/sweep"
)

// This file is the state a freshness sweep runs on: which sites are due, what
// each one cost and yielded last time, and which stored listings have stopped
// appearing on the sites they came from.

// DueSites returns the sites worth reading now, most overdue first.
//
// A site with no record has never been attempted and sorts ahead of everything
// with a due date, because until it is read the catalogue cannot say anything
// about it at all. Within each group the most prominent museum leads, so a
// capped sweep spends its budget where the answers get looked at.
// targetColumns is what every selection returns, in the order scanTargets
// reads them.
const targetColumns = `
       s.site, m.name, coalesce(m.country,''), coalesce(m.locality,''),
       coalesce(m.website,''), coalesce(m.wikidata_id,''),
       ST_Y(m.location::geometry), ST_X(m.location::geometry),
       coalesce(s.listing_url,''), coalesce(s.etag,''), coalesce(s.last_modified,''),
       coalesce(s.fingerprint,''),
       s.interval_hours, s.consecutive_failures,
       (s.last_success_at IS NULL) AS never_read`

// targetJoin attaches the museum a site's listings are attributed to: the most
// prominent one published on it, since museums share websites.
const targetJoin = `
  JOIN LATERAL (
      SELECT * FROM museums m
       WHERE m.site = s.site AND m.location IS NOT NULL
       ORDER BY m.sitelinks DESC, m.id
       LIMIT 1
  ) m ON true`

// DiscoverSites gives every museum website a scrape record, so a site that has
// never been read is a row that is due rather than an absence to be noticed.
//
// Run at the top of each cycle. It is how a museum added by a later crawl
// enters the rotation: no wiring, no separate seeding step, and the sweep's
// selection stays a plain query over one table instead of a regular expression
// across the whole catalogue.
//
// Museums with no coordinates are left out. Their exhibitions could not be
// found by any radius query, so reading them would cost a request per museum
// to store rows nothing can reach; they join by themselves once enrichment
// places them.
func (s *Store) DiscoverSites(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
INSERT INTO site_scrapes (site, next_due_at, due_reason)
-- A minute in the past, not now(): the caller captures its own timestamp and
-- asks for what is due as of then, which is fractionally before the database's
-- now() and would leave every site just discovered sitting one cycle out. The
-- margin also absorbs the clock skew between the sweeper and the database.
SELECT DISTINCT m.site, now() - interval '1 minute', 'never read'
  FROM museums m
 WHERE m.site IS NOT NULL AND m.site <> '' AND m.location IS NOT NULL
ON CONFLICT (site) DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("discover sites: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DueSites returns the sites worth reading now, most overdue first, without
// claiming them. Used by the dry run, which reads nothing.
//
// A site never successfully read leads the queue: until it is read the
// catalogue cannot say anything about it, and an area nobody has looked at
// answers every query with nothing, which reads as "there is nothing here".
func (s *Store) DueSites(ctx context.Context, now time.Time, limit int) ([]sweep.Target, error) {
	rows, err := s.pool.Query(ctx, `
SELECT`+targetColumns+`
  FROM site_scrapes s`+targetJoin+`
 WHERE s.parked_reason IS NULL AND s.next_due_at <= $1
 ORDER BY never_read DESC, s.next_due_at, m.sitelinks DESC
 LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("due sites: %w", err)
	}
	defer rows.Close()

	return scanTargets(rows)
}

// ClaimDueSites takes the next batch of due sites and marks them taken, so a
// second sweeper working the same catalogue picks up different ones.
//
// A continuous sweeper is the kind of process that gets run twice — a second
// replica, a restart overlapping a shutdown, an operator running one by hand
// while the service is up. Without a claim both would read the same sites in
// the same order, doubling the load this crawler puts on museums' own servers.
// That is the one cost the politeness rules exist to bound, so it is worth a
// lease rather than an assumption that nobody will do it.
//
// The claim is a lease, not a lock: the due date is pushed out by leaseFor, and
// a sweeper that dies mid-read leaves its sites to fall due again on their own
// rather than holding them forever. SKIP LOCKED is what makes two sweepers
// take different rows instead of queueing behind each other. The real due date
// is written when the read finishes.
func (s *Store) ClaimDueSites(ctx context.Context, now time.Time, limit int, leaseFor time.Duration) ([]sweep.Target, error) {
	rows, err := s.pool.Query(ctx, `
WITH due AS (
    SELECT site FROM site_scrapes
     WHERE parked_reason IS NULL AND next_due_at <= $1
     ORDER BY (last_success_at IS NULL) DESC, next_due_at
     LIMIT $3
     FOR UPDATE SKIP LOCKED
), claimed AS (
    UPDATE site_scrapes SET next_due_at = $2, due_reason = 'claimed by a sweep in progress'
     WHERE site IN (SELECT site FROM due)
 RETURNING *
)
SELECT`+targetColumns+`
  FROM claimed s`+targetJoin, now, now.Add(leaseFor), limit)
	if err != nil {
		return nil, fmt.Errorf("claim due sites: %w", err)
	}
	defer rows.Close()

	return scanTargets(rows)
}

// TargetsNear returns the sites around a point, with the same state the sweep
// carries, regardless of whether they are due.
//
// The on-demand path asks for an area because someone is looking at it, not
// because the schedule says so, and it must still read and write the same
// state — otherwise its work teaches the scheduler nothing and the sweep
// re-reads everything it just did. Parked sites stay out: someone panning a
// map is not a reason to retry a host that has refused six times.
func (s *Store) TargetsNear(ctx context.Context, lat, lon, radiusKm float64, limit int) ([]sweep.Target, error) {
	point := fmt.Sprintf("SRID=4326;POINT(%v %v)", lon, lat)

	rows, err := s.pool.Query(ctx, `
SELECT`+targetColumns+`
  FROM site_scrapes s`+targetJoin+`
 WHERE s.parked_reason IS NULL
   AND ST_DWithin(m.location, $1::geography, $2)
 ORDER BY never_read DESC, m.sitelinks DESC
 LIMIT $3`, point, radiusKm*1000, limit)
	if err != nil {
		return nil, fmt.Errorf("targets near: %w", err)
	}
	defer rows.Close()

	return scanTargets(rows)
}

// scanTargets reads the rows every selection returns. Shared so the column
// lists cannot drift apart from what is read out of them.
func scanTargets(rows pgx.Rows) ([]sweep.Target, error) {
	var targets []sweep.Target
	for rows.Next() {
		var (
			site          sweep.Target
			lat, lon      *float64
			intervalHours float64
			failures      int
		)
		if err := rows.Scan(&site.Site, &site.Museum.Name, &site.Museum.Country,
			&site.Museum.Locality, &site.Museum.Website, &site.Museum.WikidataID,
			&lat, &lon,
			&site.ListingURL, &site.ETag, &site.LastModified, &site.Fingerprint,
			&intervalHours, &failures, &site.NeverRead,
		); err != nil {
			return nil, fmt.Errorf("scan site: %w", err)
		}
		if lat != nil && lon != nil {
			site.Museum.Latitude, site.Museum.Longitude = *lat, *lon
		}
		site.State = sweep.State{
			Interval:            time.Duration(intervalHours * float64(time.Hour)),
			ConsecutiveFailures: failures,
		}
		targets = append(targets, site)
	}
	return targets, rows.Err()
}

// SoonestClose returns the earliest closing date among a site's live
// exhibitions, which is when its listings are certain to be out of date.
// Permanent displays have none and are ignored.
func (s *Store) SoonestClose(ctx context.Context, site string) (*time.Time, error) {
	const stmt = `
SELECT min(ends_on) FROM exhibitions
 WHERE site = $1 AND retired_at IS NULL AND NOT permanent AND ends_on IS NOT NULL`

	var soonest *time.Time
	if err := s.pool.QueryRow(ctx, stmt, site).Scan(&soonest); err != nil {
		return nil, fmt.Errorf("soonest close for %s: %w", site, err)
	}
	return soonest, nil
}

// RecordScrape writes what an attempt cost and when to come back.
//
// Every attempt is recorded, including the ones that found nothing and the
// ones that failed. That is the point of the table: an attempt that yields no
// exhibition leaves no other trace, and without one the sweep cannot tell a
// site it has never tried from a site it reads fruitlessly every week.
func (s *Store) RecordScrape(ctx context.Context, record sweep.Record, now time.Time) error {
	const stmt = `
INSERT INTO site_scrapes (site, last_attempt_at, last_success_at, last_change_at,
                          listing_url, etag, last_modified, fingerprint,
                          found_count, consecutive_failures,
                          interval_hours, next_due_at, due_reason, parked_reason)
VALUES ($1, $2::timestamptz,
        CASE WHEN $3::boolean THEN $2::timestamptz ELSE NULL END,
        CASE WHEN $4::boolean THEN $2::timestamptz ELSE NULL END,
        nullif($5,''), nullif($6,''), nullif($7,''), nullif($8,''),
        $9, $10, $11, $12, $13, nullif($14,''))
ON CONFLICT (site) DO UPDATE SET
    last_attempt_at = EXCLUDED.last_attempt_at,
    last_success_at = coalesce(EXCLUDED.last_success_at, site_scrapes.last_success_at),
    last_change_at  = coalesce(EXCLUDED.last_change_at,  site_scrapes.last_change_at),
    -- A failed or unchanged read has nothing new to say about where the
    -- listings live, so the last good answer is kept rather than blanked.
    listing_url   = coalesce(EXCLUDED.listing_url,   site_scrapes.listing_url),
    etag          = coalesce(EXCLUDED.etag,          site_scrapes.etag),
    last_modified = coalesce(EXCLUDED.last_modified, site_scrapes.last_modified),
    fingerprint   = coalesce(EXCLUDED.fingerprint,   site_scrapes.fingerprint),
    found_count   = CASE WHEN $3::boolean THEN EXCLUDED.found_count ELSE site_scrapes.found_count END,
    consecutive_failures = EXCLUDED.consecutive_failures,
    interval_hours = EXCLUDED.interval_hours,
    next_due_at    = EXCLUDED.next_due_at,
    due_reason     = EXCLUDED.due_reason,
    parked_reason  = EXCLUDED.parked_reason`

	succeeded := record.Outcome != sweep.Failed
	changed := record.Outcome == sweep.Changed

	var parked string
	if record.Plan.Park {
		parked = record.Plan.Reason
	}

	_, err := s.pool.Exec(ctx, stmt,
		record.Site, now, succeeded, changed,
		record.ListingURL, record.ETag, record.LastModified, record.Fingerprint,
		record.FoundCount, record.Plan.ConsecutiveFailures,
		record.Plan.Interval.Hours(), record.Plan.DueAt, record.Plan.Reason, parked)
	if err != nil {
		return fmt.Errorf("record scrape for %s: %w", record.Site, err)
	}
	return nil
}

// RetireUnseen hides a site's listings that were not found in the read that
// finished at seenFrom, and reports how many it hid.
//
// Only called after a read that found something. A site that answers but lists
// nothing is far more often broken, blocked or JavaScript-rendered than
// genuinely empty, and acting on that would erase a museum's whole programme
// on the strength of one bad afternoon.
//
// Submissions are never retired: nothing will see them on a museum's site,
// because they did not come from one.
func (s *Store) RetireUnseen(ctx context.Context, site string, seenFrom time.Time) (int64, error) {
	const stmt = `
UPDATE exhibitions
   SET retired_at = $2
 WHERE site = $1
   AND source = 'scraped'
   AND retired_at IS NULL
   AND last_seen_at < $2`

	tag, err := s.pool.Exec(ctx, stmt, site, seenFrom)
	if err != nil {
		return 0, fmt.Errorf("retire unseen for %s: %w", site, err)
	}
	return tag.RowsAffected(), nil
}

// TouchSite marks a site's listings as still current without rewriting them,
// for the case where the site answered that its listing page has not moved.
//
// Without this a 304 would look exactly like a read that found nothing, and
// the next successful read would retire everything the site still publishes.
func (s *Store) TouchSite(ctx context.Context, site string, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE exhibitions SET last_seen_at = $2 WHERE site = $1 AND retired_at IS NULL`,
		site, now)
	if err != nil {
		return 0, fmt.Errorf("touch site %s: %w", site, err)
	}
	return tag.RowsAffected(), nil
}

// SweepSummary is what a sweep run and the audit report on.
type SweepSummary struct {
	SitesKnown          int64
	SitesRead           int64
	SitesParked         int64
	SitesDue            int64
	SitesNever          int64
	OldestRead          *time.Time
	MedianIntervalHours float64
}

// SweepStatus reports how well the catalogue is being kept fresh.
func (s *Store) SweepStatus(ctx context.Context, now time.Time) (SweepSummary, error) {
	const stmt = `
SELECT count(*),
       count(*) FILTER (WHERE last_success_at IS NOT NULL),
       count(*) FILTER (WHERE parked_reason IS NOT NULL),
       count(*) FILTER (WHERE parked_reason IS NULL AND next_due_at <= $1),
       count(*) FILTER (WHERE last_success_at IS NULL),
       min(last_success_at),
       coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY interval_hours)
                FILTER (WHERE parked_reason IS NULL), 0)
  FROM site_scrapes`

	var summary SweepSummary
	if err := s.pool.QueryRow(ctx, stmt, now).Scan(
		&summary.SitesKnown, &summary.SitesRead, &summary.SitesParked,
		&summary.SitesDue, &summary.SitesNever, &summary.OldestRead,
		&summary.MedianIntervalHours,
	); err != nil {
		return SweepSummary{}, fmt.Errorf("sweep status: %w", err)
	}
	return summary, nil
}

// MarkAreaScraped records that an area has just been read.
func (s *Store) MarkAreaScraped(ctx context.Context, cell string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO area_scrapes (cell, scraped_at) VALUES ($1, $2)
ON CONFLICT (cell) DO UPDATE SET scraped_at = excluded.scraped_at`, cell, at)
	if err != nil {
		return fmt.Errorf("mark area scraped: %w", err)
	}
	return nil
}

// AreasScrapedSince returns the areas read since a moment, and forgets the rest.
//
// The pruning is here rather than on a timer because this is the only caller:
// it runs once at startup to give the queue back the cooldowns it would
// otherwise have lost, and a row older than the cooldown can never make that
// answer differ. Doing both in one pass keeps the table the size of a day's
// browsing rather than a growing record of every area ever looked at.
func (s *Store) AreasScrapedSince(ctx context.Context, since time.Time) (map[string]time.Time, error) {
	if _, err := s.pool.Exec(ctx, `DELETE FROM area_scrapes WHERE scraped_at < $1`, since); err != nil {
		return nil, fmt.Errorf("prune area scrapes: %w", err)
	}

	rows, err := s.pool.Query(ctx, `SELECT cell, scraped_at FROM area_scrapes`)
	if err != nil {
		return nil, fmt.Errorf("areas scraped since: %w", err)
	}
	defer rows.Close()

	areas := make(map[string]time.Time)
	for rows.Next() {
		var (
			cell string
			at   time.Time
		)
		if err := rows.Scan(&cell, &at); err != nil {
			return nil, fmt.Errorf("scan area scrape: %w", err)
		}
		areas[cell] = at
	}
	return areas, rows.Err()
}
