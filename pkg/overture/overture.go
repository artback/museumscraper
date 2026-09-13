package overture

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"

	"museum/internal/models"
	"museum/pkg/geo"
	"museum/pkg/useragent"
)

// SourceName identifies records that came from Overture.
const SourceName = "overture"

const (
	// bucketURL is the public mirror of the Overture release bucket. No
	// credentials, no client library: the listing is XML over HTTPS and the
	// data is served with range requests.
	bucketURL = "https://overturemaps-us-west-2.s3.amazonaws.com"

	// placesPrefix is where a release keeps the places theme.
	placesPrefix = "theme=places/type=place/"

	// minConfidence is the floor below which a place is not worth storing.
	//
	// Overture scores every place by how much its sources agree. Across a
	// sample of 363 museums the scores spread the whole range, with about a
	// quarter below 0.5 — those are typically a single unconfirmed
	// contribution, which is as often a closed museum or a duplicate as a real
	// one. The floor is low rather than strict because this source exists to
	// reach places the others cannot, and a thin record from Lagos is worth
	// more than a confident duplicate of the Louvre.
	minConfidence = 0.3

	requestTimeout = 2 * time.Minute
)

// museumCategories are the Overture categories this catalogue treats as
// museums.
//
// Enumerated rather than matched on the substring "museum": that would admit
// "museum_store" and miss "planetarium". Anything unrecognised whose name
// contains "museum" is counted and logged at the end of a pass, which is how
// this list was corrected — the first full pass reported nine categories it
// had never heard of, 2,922 museums in all, every one of them real.
//
// planetarium is here because the American register counts planetariums as
// science museums, and taking them from one source and not another would be an
// inconsistency nothing downstream could explain.
//
// art_gallery is deliberately *not* here, and it is the single biggest
// decision in this file. The first full pass found 140,650 of them against
// 134,627 of everything else: admitting them would have more than doubled the
// source with a population that is mostly commercial — "Galerie Au Chevalet",
// "Manua Exquisite Tahitian Art", a gallery whose website sells prints. That
// is the mistake this catalogue has already reasoned itself out of twice, at
// arts centres in the OSM query and at historical societies in the American
// register: a large class of nearly-right records that nothing downstream can
// tell apart from the real ones.
//
// OpenStreetMap's tourism=gallery stays included, and the two are not in
// tension. That tag is applied by mappers, is small, and is genuinely used for
// museums in countries where the museum tag never got added; this is a
// commercial directory's category for a shop that sells art. A museum
// mis-categorised here is still reachable through OSM.
var museumCategories = map[string]string{
	"museum":                  "museum",
	"history_museum":          "history museum",
	"art_museum":              "art museum",
	"modern_art_museum":       "modern art museum",
	"contemporary_art_museum": "contemporary art museum",
	"asian_art_museum":        "art museum",
	"decorative_arts_museum":  "decorative arts museum",
	"design_museum":           "design museum",
	"photography_museum":      "photography museum",
	"textile_museum":          "textile museum",
	"costume_museum":          "costume museum",
	"cartooning_museum":       "cartooning museum",
	"science_museum":          "science museum",
	"natural_history_museum":  "natural history museum",
	"childrens_museum":        "children's museum",
	"community_museum":        "community museum",
	"civilization_museum":     "civilization museum",
	"national_museum":         "national museum",
	"state_museum":            "state museum",
	"computer_museum":         "computer museum",
	"sports_museum":           "sports museum",
	"military_museum":         "military museum",
	"railroad_museum":         "railway museum",
	"maritime_museum":         "maritime museum",
	"aviation_museum":         "aviation museum",
	"wax_museum":              "wax museum",
	"planetarium":             "planetarium",
}

// place is the projection read out of each row. Its fields are the whole
// reason a pass over the planet costs two gigabytes rather than ten.
type place struct {
	Categories struct {
		Primary string `parquet:"primary"`
	} `parquet:"categories"`
	Names struct {
		Primary string `parquet:"primary"`
	} `parquet:"names"`
	Bbox struct {
		Xmin float32 `parquet:"xmin"`
		Ymin float32 `parquet:"ymin"`
	} `parquet:"bbox"`
	Confidence float64  `parquet:"confidence"`
	Websites   []string `parquet:"websites,list"`
	Addresses  []struct {
		Country  string `parquet:"country"`
		Locality string `parquet:"locality"`
	} `parquet:"addresses,list"`
}

// Client reads the Overture release bucket.
type Client struct {
	httpClient *http.Client
	agent      string
	bucket     string
}

// NewClient returns a Client for the public release bucket.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: requestTimeout},
		agent:      useragent.For("museum locations", "OVERTURE_USER_AGENT"),
		bucket:     bucketURL,
	}
}

// listing is the subset of S3's XML listing this package reads.
type listing struct {
	Contents []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// LatestRelease returns the identifier of the newest Overture release, e.g.
// "2026-08-19.0".
//
// This is the whole update story for this source, and it costs one request.
// Overture publishes monthly and never rewrites a release, so the identifier
// is an exact answer to "is there anything new": if it has not moved, the two
// gigabytes behind it are the same two gigabytes as last month.
func (c *Client) LatestRelease(ctx context.Context) (string, error) {
	var releases []string

	err := c.eachPage(ctx, "release/", "/", func(page listing) {
		for _, prefix := range page.CommonPrefixes {
			name := strings.TrimSuffix(strings.TrimPrefix(prefix.Prefix, "release/"), "/")
			if name != "" {
				releases = append(releases, name)
			}
		}
	})
	if err != nil {
		return "", err
	}
	if len(releases) == 0 {
		return "", fmt.Errorf("no releases found in %s", c.bucket)
	}

	sort.Strings(releases)
	return releases[len(releases)-1], nil
}

// files lists the parquet files of a release's places theme.
func (c *Client) files(ctx context.Context, release string) ([]string, error) {
	var keys []string

	err := c.eachPage(ctx, "release/"+release+"/"+placesPrefix, "", func(page listing) {
		for _, object := range page.Contents {
			if strings.HasSuffix(object.Key, ".parquet") {
				keys = append(keys, object.Key)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("release %s has no places files", release)
	}

	sort.Strings(keys)
	return keys, nil
}

// eachPage walks a paginated bucket listing.
func (c *Client) eachPage(ctx context.Context, prefix, delimiter string, fn func(listing)) error {
	token := ""
	for {
		url := fmt.Sprintf("%s/?list-type=2&prefix=%s", c.bucket, prefix)
		if delimiter != "" {
			url += "&delimiter=" + delimiter
		}
		if token != "" {
			url += "&continuation-token=" + token
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", c.agent)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("list %s: %w", prefix, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("list %s: %w", prefix, err)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("list %s: status %s", prefix, resp.Status)
		}

		var page listing
		if err := xml.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("parse listing: %w", err)
		}
		fn(page)

		if !page.IsTruncated || page.NextContinuationToken == "" {
			return nil
		}
		token = page.NextContinuationToken
	}
}

// Service streams museums out of an Overture release.
type Service struct {
	client *Client
}

// NewService returns a Service backed by client.
func NewService(client *Client) *Service {
	if client == nil {
		client = NewClient()
	}
	return &Service{client: client}
}

// LatestRelease reports the newest release, so a caller can decide whether
// reading it is worth the traffic.
func (s *Service) LatestRelease(ctx context.Context) (string, error) {
	return s.client.LatestRelease(ctx)
}

// Museums streams every museum in a release, reading the files one at a time.
//
// Sequential rather than parallel across files: the crawl already runs four
// other sources at once, and this one is the only one whose cost is bandwidth
// rather than somebody else's rate limit. Saturating a home connection would
// slow the sources that are being politely throttled anyway.
func (s *Service) Museums(ctx context.Context, release string) <-chan models.Museum {
	out := make(chan models.Museum)

	go func() {
		defer close(out)

		keys, err := s.client.files(ctx, release)
		if err != nil {
			log.Printf("overture: %v", err)
			return
		}
		log.Printf("overture: reading release %s, %d files", release, len(keys))

		var (
			total     int
			read      int64
			requests  int64
			unknown   = map[string]int{}
			startedAt = time.Now()
		)

		for i, key := range keys {
			if ctx.Err() != nil {
				return
			}

			count, stats, err := s.readFile(ctx, key, unknown, func(m models.Museum) bool {
				select {
				case out <- m:
					return true
				case <-ctx.Done():
					return false
				}
			})
			read += stats.read
			requests += stats.requests
			total += count
			if err != nil {
				log.Printf("overture: %s: %v", key, err)
				continue
			}
			log.Printf("overture: file %d/%d — %d museums (running total %d, %.0f MB read)",
				i+1, len(keys), count, total, float64(read)/1e6)
		}

		for category, n := range unknown {
			log.Printf("overture: skipped %d places in unrecognised category %q", n, category)
		}
		log.Printf("overture: finished, %d museums in %s — %.1f GB over %d range requests",
			total, time.Since(startedAt).Round(time.Second), float64(read)/1e9, requests)
	}()

	return out
}

// stats is what one file cost.
type stats struct {
	read, requests int64
}

// readFile streams the museums out of one parquet file, calling emit for each
// and stopping when it returns false.
func (s *Service) readFile(ctx context.Context, key string, unknown map[string]int, emit func(models.Museum) bool) (int, stats, error) {
	url := s.client.bucket + "/" + key

	size, err := s.client.size(ctx, url)
	if err != nil {
		return 0, stats{}, err
	}

	reader := &chunkReaderAt{
		ctx:    ctx,
		client: s.client.httpClient,
		url:    url,
		agent:  s.client.agent,
		size:   size,
	}

	file, err := parquet.OpenFile(reader, size)
	if err != nil {
		return 0, stats{reader.read.Load(), reader.requests.Load()}, fmt.Errorf("open parquet: %w", err)
	}

	wanted := leafColumns(file.Schema())
	metadata := file.Metadata()
	found := 0

	for i, group := range file.RowGroups() {
		if ctx.Err() != nil {
			break
		}

		// Fetch exactly this row group's wanted columns before parquet asks for
		// them, so each column chunk costs one request rather than one per page.
		if err := reader.prefetch(wantedRanges(metadata.RowGroups[i], wanted)); err != nil {
			return found, stats{reader.read.Load(), reader.requests.Load()}, err
		}

		rows := parquet.NewGenericRowGroupReader[place](group)
		buffer := make([]place, 1024)
		for {
			n, err := rows.Read(buffer)
			for _, p := range buffer[:n] {
				museum, ok := toMuseum(p, unknown)
				if !ok {
					continue
				}
				found++
				if !emit(museum) {
					return found, stats{reader.read.Load(), reader.requests.Load()}, nil
				}
			}
			if err == io.EOF || n == 0 {
				break
			}
			if err != nil {
				return found, stats{reader.read.Load(), reader.requests.Load()}, fmt.Errorf("read rows: %w", err)
			}
		}
	}

	return found, stats{reader.read.Load(), reader.requests.Load()}, nil
}

// size asks how long a file is, which parquet needs before it can find the
// footer.
func (c *Client) size(ctx context.Context, url string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", c.agent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("head %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("head %s: status %s", url, resp.Status)
	}
	if resp.ContentLength <= 0 {
		return 0, fmt.Errorf("head %s: no content length", url)
	}
	return resp.ContentLength, nil
}

// toMuseum converts a place, reporting false for anything this catalogue does
// not take.
func toMuseum(p place, unknown map[string]int) (models.Museum, bool) {
	category := strings.TrimSpace(p.Categories.Primary)
	class, isMuseum := museumCategories[category]
	if !isMuseum {
		if strings.Contains(category, "museum") {
			// A category upstream added that this build does not know. Counted
			// rather than silently dropped: the alternative is a source that
			// quietly stops seeing a kind of museum.
			unknown[category]++
		}
		return models.Museum{}, false
	}

	name := strings.TrimSpace(p.Names.Primary)
	if name == "" || p.Confidence < minConfidence {
		return models.Museum{}, false
	}

	museum := models.Museum{
		Name:    name,
		Classes: []string{class},
		Sources: []string{SourceName},
	}

	// Places are points, so the bounding box is the point: there is no need to
	// transfer the geometry column, which is a third of what the projection
	// would otherwise cost.
	//
	// bbox is float32, so a position carries about seven significant digits —
	// a tenth of a metre. The geometry column would give full precision for
	// roughly another gigabyte per pass, which is a poor trade for a building
	// that is tens of metres across.
	if lat, lon := float64(p.Bbox.Ymin), float64(p.Bbox.Xmin); lat != 0 || lon != 0 {
		if lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 {
			museum.Latitude, museum.Longitude = lat, lon
		}
	}

	for _, site := range p.Websites {
		if site = strings.TrimSpace(site); site != "" {
			museum.Website = site
			break
		}
	}
	if len(p.Addresses) > 0 {
		museum.Locality = strings.TrimSpace(p.Addresses[0].Locality)
		museum.Country = countryName(p.Addresses[0].Country)
	}
	if museum.Country == "" {
		museum.Country = "unknown"
	}
	return museum, true
}

// countryName turns the ISO 3166-1 alpha-2 code Overture stores into the
// spelling the rest of the catalogue uses, so the merger can match on it. A
// code this build does not know leaves the country empty rather than guessing.
func countryName(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return ""
	}
	if name, ok := codeToCountry[code]; ok {
		return name
	}
	return ""
}

// codeToCountry inverts pkg/geo's country-to-code table once, at startup.
var codeToCountry = func() map[string]string {
	out := map[string]string{}
	for _, area := range geo.CrawlAreas() {
		if code, ok := geo.ISOCode(area); ok {
			if _, taken := out[code]; !taken {
				out[code] = area
			}
		}
	}
	return out
}()
