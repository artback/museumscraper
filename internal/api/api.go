// Package api serves the catalogue over HTTP.
//
// Every query is answered by the database. The handlers hold no index of their
// own and no cache: Postgres maintains its indexes transactionally and has its
// own buffer pool, which removes both the in-process cache and the class of bug
// where a derived index silently disagreed with the records it came from.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"museum/internal/postgres"
)

const (
	// maxRadiusKm bounds a query, so one request cannot ask for the world.
	maxRadiusKm = 50

	// defaultRadiusKm is used when a request omits one.
	defaultRadiusKm = 3

	// maxLimit caps how many results a single response carries.
	maxLimit = 500

	// defaultLimit is used when a request omits one.
	defaultLimit = 50

	// maxQueryRunes bounds a search string. Anything longer is not a search.
	maxQueryRunes = 200

	// maxPlaceNameChars bounds a place name before it reaches the geocoder.
	maxPlaceNameChars = 200

	// maxOffset bounds how deep a caller may page. Offset paging makes the
	// database skip every preceding row, so an unbounded offset is a cheap way
	// to buy an expensive scan. Well past any real result set: nothing in this
	// catalogue returns more than a few thousand rows for one area.
	maxOffset = 10_000
)

// Catalogue is what the API needs from the database.
//
// It is an interface so the handlers can be tested without a database, and so
// the storage layer can be replaced without touching them.
type Catalogue interface {
	NearbyVerified(ctx context.Context, lat, lon, radiusKm float64, limit, offset int, verifiedOnly bool) (postgres.Page, error)
	Search(ctx context.Context, query string, limit, offset int) (postgres.Page, error)
	MuseumByID(ctx context.Context, id string) (postgres.Hit, error)
	Points(ctx context.Context, west, south, east, north float64, hasBox bool, limit int) ([]postgres.Point, error)
	ExhibitionsNearby(ctx context.Context, lat, lon, radiusKm float64, includeUpcoming bool, limit int) ([]postgres.ExhibitionHit, error)
	SearchExhibitions(ctx context.Context, query string, lat, lon, radiusKm float64, near, includeUpcoming bool, limit, offset int) ([]postgres.ExhibitionHit, int64, error)
	ExhibitionCoverage(ctx context.Context, lat, lon, radiusKm float64) (postgres.Coverage, error)
	Counts(ctx context.Context) (postgres.Counts, error)
	Ping(ctx context.Context) error
}

// placeLookup resolves a place name to coordinates.
type placeLookup interface {
	Resolve(ctx context.Context, name string) (postgres.Place, error)
}

// Server answers catalogue queries over HTTP.
type Server struct {
	catalogue Catalogue
	places    placeLookup
	scrapes   *scrapeQueue
}

// NewServer returns a Server backed by the catalogue. Without a resolver the
// API still works; it just cannot answer "what is on in Paris" without being
// told where Paris is.
func NewServer(catalogue Catalogue) *Server {
	return &Server{catalogue: catalogue}
}

// WithScraping returns a Server that can read museum websites on demand, so a
// visitor arriving somewhere nobody has looked yet does not simply find it
// empty.
func (s *Server) WithScraping(store Harvester) *Server {
	s.scrapes = newScrapeQueue(store)
	return s
}

// Close releases what the server started. Safe on a server that started
// nothing.
func (s *Server) Close() {
	if s.scrapes != nil {
		s.scrapes.close()
	}
}

// WithPlaces returns a Server that can resolve place names.
func (s *Server) WithPlaces(places placeLookup) *Server {
	s.places = places
	return s
}

// Routes returns the HTTP handler for the API.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)

	// Liveness and readiness answer different questions, and conflating them
	// makes a database outage look like a broken process: an orchestrator
	// restarts pods that were working fine, turning an incident into a restart
	// storm. /livez says the process is running; /readyz says it can serve.
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
	})
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /v1/museums", s.handleMuseums)
	mux.HandleFunc("GET /v1/museums/{id}", s.handleMuseum)
	mux.HandleFunc("GET /v1/points", s.handlePoints)
	mux.HandleFunc("GET /v1/places", s.handlePlaces)
	mux.HandleFunc("GET /v1/scrape", s.handleScrape)
	mux.HandleFunc("POST /v1/scrape", s.handleScrape)
	mux.HandleFunc("GET /map", s.handleMap)
	mux.HandleFunc("GET /map/vendor/{file}", s.handleVendor)
	mux.HandleFunc("GET /map/assets/{file}", s.handleAsset)
	mux.HandleFunc("GET /{$}", s.handleMap)
	mux.HandleFunc("GET /v1/exhibitions", s.handleExhibitions)
	mux.HandleFunc("GET /v1/search", s.handleSearch)

	// The mux answers an unknown path with plain text and a wrong method with
	// an empty body, so a client that decodes JSON on every non-2xx response
	// fails on exactly the two statuses it is most likely to meet. Routing
	// through one handler keeps the error shape uniform.
	mux.HandleFunc("/", s.handleNotFound)

	return withMiddleware(mux)
}

// handleNotFound answers anything the routes did not claim.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed,
			fmt.Errorf("%s is not supported; this API is read-only", r.Method))
		return
	}
	writeError(w, http.StatusNotFound, fmt.Errorf("no such endpoint: %s", r.URL.Path))
}

// query is the location and paging a request asked for.
type query struct {
	lat, lon float64
	radiusKm float64
	limit    int
	offset   int
	// verifiedOnly drops the unverified tail: names read off list pages that
	// no source confirmed are museums.
	verifiedOnly bool
	// place is the name that produced the coordinates, echoed back so a caller
	// can see which "Springfield" it was given.
	place string
}

// parseQuery reads and validates the shared query parameters.
//
// A request may give coordinates directly or name a place. Coordinates win
// when both are present: they are unambiguous, and a caller that sends both
// probably means the pair it computed itself.
func (s *Server) parseQuery(r *http.Request) (query, error) {
	values := r.URL.Query()

	if name := strings.TrimSpace(values.Get("place")); name != "" &&
		values.Get("lat") == "" && values.Get("lon") == "" {
		return s.parsePlaceQuery(r, name, values)
	}

	lat, err := parseFloat(values.Get("lat"), "lat")
	if err != nil {
		return query{}, err
	}
	lon, err := parseFloat(values.Get("lon"), "lon")
	if err != nil {
		return query{}, err
	}
	if lat < -90 || lat > 90 {
		return query{}, errors.New("lat must be between -90 and 90")
	}
	if lon < -180 || lon > 180 {
		return query{}, errors.New("lon must be between -180 and 180")
	}

	radius := float64(defaultRadiusKm)
	if raw := values.Get("radius_km"); raw != "" {
		if radius, err = parseFloat(raw, "radius_km"); err != nil {
			return query{}, err
		}
	}
	if radius <= 0 {
		return query{}, errors.New("radius_km must be greater than zero")
	}
	if radius > maxRadiusKm {
		return query{}, fmt.Errorf("radius_km must be %d or less", maxRadiusKm)
	}

	limit, err := parseLimit(values.Get("limit"))
	if err != nil {
		return query{}, err
	}
	offset, err := parseOffset(values.Get("offset"))
	if err != nil {
		return query{}, err
	}

	return query{lat: lat, lon: lon, radiusKm: radius, limit: limit, offset: offset,
		verifiedOnly: values.Get("verified") == "true"}, nil
}

func parseOffset(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		return 0, errors.New("offset must be a whole number of zero or more")
	}
	if parsed > maxOffset {
		return 0, fmt.Errorf("offset must be %d or less", maxOffset)
	}
	return parsed, nil
}

// parsePlaceQuery resolves a named place into the same query the coordinate
// form produces.
//
// The radius defaults to the extent the geocoder reported for the place rather
// than the fixed 3 km, so "Paris" searches Paris and "Rue de Rivoli" searches a
// street. An explicit radius_km still overrides it.
func (s *Server) parsePlaceQuery(r *http.Request, name string, values url.Values) (query, error) {
	if s.places == nil {
		return query{}, errors.New("place lookup is not configured on this server; pass lat and lon")
	}
	if len(name) > maxPlaceNameChars {
		return query{}, fmt.Errorf("place must be %d characters or fewer", maxPlaceNameChars)
	}

	found, err := s.places.Resolve(r.Context(), name)
	if err != nil {
		return query{}, err
	}

	radius := found.RadiusKm
	if raw := values.Get("radius_km"); raw != "" {
		if radius, err = parseFloat(raw, "radius_km"); err != nil {
			return query{}, err
		}
		if radius <= 0 {
			return query{}, errors.New("radius_km must be greater than zero")
		}
		if radius > maxRadiusKm {
			return query{}, fmt.Errorf("radius_km must be %d or less", maxRadiusKm)
		}
	}

	limit, err := parseLimit(values.Get("limit"))
	if err != nil {
		return query{}, err
	}
	offset, err := parseOffset(values.Get("offset"))
	if err != nil {
		return query{}, err
	}

	return query{lat: found.Latitude, lon: found.Longitude, radiusKm: radius,
		limit: limit, offset: offset, place: found.DisplayName,
		verifiedOnly: values.Get("verified") == "true"}, nil
}

func parseLimit(raw string) (int, error) {
	if raw == "" {
		return defaultLimit, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 1 {
		return 0, errors.New("limit must be a positive whole number")
	}
	return min(parsed, maxLimit), nil
}

func parseFloat(raw, name string) (float64, error) {
	if raw == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("%s must be a number", name)
	}
	return value, nil
}

// responseQuery echoes what was asked, so a client can tell whether its radius
// or limit was clamped.
type responseQuery struct {
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lon"`
	RadiusKm  float64 `json:"radius_km"`
	Limit     int     `json:"limit"`
	Offset    int     `json:"offset,omitempty"`
	// Place is the geocoder's full name for what a place= query resolved to,
	// so a caller can see whether "Springfield" meant the one it had in mind.
	Place string `json:"place,omitempty"`
	// Text is the search term, echoed so a caller can tell a search apart from
	// a plain radius query in the reply alone.
	Text string `json:"q,omitempty"`
}

func echo(q query) responseQuery {
	return responseQuery{Latitude: q.lat, Longitude: q.lon,
		RadiusKm: q.radiusKm, Limit: q.limit, Offset: q.offset, Place: q.place}
}

type museumResponse struct {
	Count int `json:"count"`
	// Total is how many museums matched, not how many were returned. Without
	// it a full page is indistinguishable from a complete result set.
	Total int64 `json:"total"`
	// HasMore says whether another page exists, so a client does not have to
	// do the arithmetic itself.
	HasMore bool          `json:"has_more"`
	Museums []museumHit   `json:"museums"`
	Query   responseQuery `json:"query"`
}

type museumHit struct {
	// ID is stable across requests and re-crawls: the thing to deep link to,
	// dedupe by, and fetch again from /v1/museums/{id}.
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	DistanceKm  float64 `json:"distance_km"`
	Country     string  `json:"country,omitempty"`
	Locality    string  `json:"locality,omitempty"`
	Description string  `json:"description,omitempty"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	// ApproximateLocation marks a position taken from the museum's town rather
	// than the museum itself, because no geocoder could find it by name. The
	// museum is really in that town; it is not really at that point.
	ApproximateLocation bool `json:"approximate_location,omitempty"`
	// Verified means the museum is backed by a Wikipedia article — every source
	// sets it on that basis and no other. It is a proxy for confidence, not a
	// judgement that the record is a museum: plenty of real small museums have
	// no article, and Gothenburg has 75 museums of which 20 are verified.
	//
	// It is still the best filter available against the catalogue's noisy tail.
	// Names read off list pages are emitted unverified, which is where
	// "Williamsburg, Virginia" and "Silverton (hotel and casino)" come from —
	// real entries on a list of museums, but not museums. A caller that cannot
	// tolerate those should pass verified=true and accept losing some real ones.
	Verified     bool     `json:"verified"`
	Website      string   `json:"website,omitempty"`
	WikipediaURL string   `json:"wikipedia_url,omitempty"`
	WikidataID   string   `json:"wikidata_id,omitempty"`
	Sources      []string `json:"sources,omitempty"`
	// Classes are what kind of thing the museum is, in the source's own words:
	// "steamboat", "passenger ship", "working life museum". They are what tells
	// a caller that "Bohuslän" is a preserved steamship rather than the Swedish
	// province of the same name — a distinction neither the name nor the
	// description carries.
	Classes []string `json:"classes,omitempty"`
}

type searchResponse struct {
	Count   int         `json:"count"`
	Total   int64       `json:"total"`
	HasMore bool        `json:"has_more"`
	Limit   int         `json:"limit"`
	Offset  int         `json:"offset,omitempty"`
	Query   string      `json:"query"`
	Museums []searchHit `json:"museums"`
}

type searchHit struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Country    string   `json:"country,omitempty"`
	Locality   string   `json:"locality,omitempty"`
	Aliases    []string `json:"also_known_as,omitempty"`
	Latitude   float64  `json:"latitude,omitempty"`
	Longitude  float64  `json:"longitude,omitempty"`
	Website    string   `json:"website,omitempty"`
	Wikipedia  string   `json:"wikipedia_url,omitempty"`
	WikidataID string   `json:"wikidata_id,omitempty"`
	// Locatable says whether the museum can also be found by coordinates.
	// Around a quarter of the catalogue cannot, and a caller plotting results
	// on a map needs to know which.
	Locatable bool    `json:"locatable"`
	Score     float64 `json:"score"`
}

type exhibitionResponse struct {
	Count int `json:"count"`
	// Total is how many matched a text search, as against how many were
	// returned. Absent for a plain radius query, which does not count beyond
	// its limit.
	Total       int64           `json:"total,omitempty"`
	Exhibitions []exhibitionHit `json:"exhibitions"`
	Query       responseQuery   `json:"query"`
	// Coverage says what is known about the area, so an empty result can be
	// read correctly. "Nothing is on" and "nobody has looked here yet" are very
	// different answers and looked identical without it.
	Coverage *coverageReport `json:"coverage,omitempty"`
}

// coverageReport explains an exhibition result.
type coverageReport struct {
	MuseumsInArea   int64      `json:"museums_in_area"`
	MuseumsWithSite int64      `json:"museums_with_website"`
	LastScraped     *time.Time `json:"last_scraped,omitempty"`
	// Note is present only when the result needs explaining, and says what to
	// do about it.
	Note string `json:"note,omitempty"`
}

type exhibitionHit struct {
	Title  string `json:"title"`
	URL    string `json:"url"`
	Museum string `json:"museum"`
	// MuseumWikidataID is which museum, as opposed to what it was called when
	// the listing was read. A caller matching exhibitions to museums by name
	// gets it wrong in three ways that all happen in practice: a re-scrape
	// rewrites the name, merging name variants renames the museum afterwards,
	// and a show listed by two venues carries only one of them. The same
	// identifier is on museumHit, so the two can be joined exactly.
	MuseumWikidataID string     `json:"museum_wikidata_id,omitempty"`
	DistanceKm       float64    `json:"distance_km"`
	Start            *time.Time `json:"start,omitempty"`
	End              *time.Time `json:"end,omitempty"`
	Running          bool       `json:"running"`
	Upcoming         bool       `json:"upcoming"`
	// Permanent marks a display that is always on, which is why it carries no
	// dates. Without it a caller reads the empty start and end as a listing the
	// scraper failed on, and a caller asking what closes soonest would put an
	// exhibition that has been up for thirty years at the top.
	Permanent bool      `json:"permanent"`
	Latitude  float64   `json:"latitude"`
	Longitude float64   `json:"longitude"`
	ScrapedAt time.Time `json:"scraped_at"`
}

// handleHealth reports both liveness and what the catalogue holds.
//
// An empty catalogue answers every query with nothing and no error, which looks
// identical to "there are no museums here". Reporting the counts makes the two
// distinguishable without a database client.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.catalogue.Ping(r.Context()); err != nil {
		log.Printf("api: health ping failed: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}

	counts, err := s.catalogue.Counts(r.Context())
	if err != nil {
		log.Printf("api: health counts failed: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"museums":          counts.Museums,
		"with_coordinates": counts.WithCoordinates,
		"countries":        counts.Countries,
		"exhibitions":      counts.Exhibitions,
		"last_updated":     counts.LastUpdated,
	})
}

// handleReady reports whether the catalogue can be queried.
//
// It gets its own short deadline. A partition leaves established connections
// black-holed rather than refused, so an unbounded ping does not fail — it
// hangs, and a probe reading a timeout instead of a 503 cannot tell a wedged
// process from an unreachable database.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	if err := s.catalogue.Ping(ctx); err != nil {
		log.Printf("api: not ready: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleMuseums(w http.ResponseWriter, r *http.Request) {
	q, err := s.parseQuery(r)
	if err != nil {
		writeQueryError(w, r, err)
		return
	}

	page, err := s.catalogue.NearbyVerified(r.Context(), q.lat, q.lon, q.radiusKm, q.limit, q.offset, q.verifiedOnly)
	if err != nil {
		writeServerError(w, r, err)
		return
	}

	museums := make([]museumHit, 0, len(page.Hits))
	for _, hit := range page.Hits {
		museums = append(museums, museumHitFrom(hit, round2(hit.DistanceKm)))
	}

	writeJSON(w, http.StatusOK, museumResponse{
		Count:   len(museums),
		Total:   page.Total,
		HasMore: int64(q.offset+len(museums)) < page.Total,
		Museums: museums,
		Query:   echo(q),
	})
}

// maxPoints bounds a map request. Enough to draw the world — the museums are
// dense enough that at this count they trace the coastlines themselves — while
// keeping the response to a few megabytes on a local connection.
const maxPoints = 40_000

// handlePlaces resolves a place name to somewhere on the map.
//
// The catalogue answers "which museum is this?"; this answers "where is this?".
// Without it a search box can only find things the catalogue already holds, so
// typing a city name that has no museum of its own finds nothing at all.
func (s *Server) handlePlaces(w http.ResponseWriter, r *http.Request) {
	if s.places == nil {
		writeError(w, http.StatusNotImplemented, errors.New("place lookup is not enabled"))
		return
	}

	name := strings.TrimSpace(r.URL.Query().Get("q"))
	if name == "" {
		writeError(w, http.StatusBadRequest, errors.New("q is required"))
		return
	}
	if utf8.RuneCountInString(name) > maxPlaceNameChars {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("q must be %d characters or fewer", maxPlaceNameChars))
		return
	}

	place, err := s.places.Resolve(r.Context(), name)
	if errors.Is(err, postgres.ErrPlaceUnknown) {
		writeJSON(w, http.StatusOK, map[string]any{"count": 0, "places": []any{}})
		return
	}
	if err != nil {
		writeServerError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"count": 1,
		"places": []map[string]any{{
			"name":      place.DisplayName,
			"latitude":  place.Latitude,
			"longitude": place.Longitude,
			"radius_km": place.RadiusKm,
		}},
	})
}

// handlePoints returns museum positions for drawing.
func (s *Server) handlePoints(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()

	limit := maxPoints
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, errors.New("limit must be a positive whole number"))
			return
		}
		limit = min(parsed, maxPoints)
	}

	west, south, east, north, hasBox, err := parseBBox(values.Get("bbox"))
	if err != nil {
		writeQueryError(w, r, err)
		return
	}

	points, err := s.catalogue.Points(r.Context(), west, south, east, north, hasBox, limit)
	if err != nil {
		writeServerError(w, r, err)
		return
	}

	// Flat triples rather than objects: forty thousand museums as
	// {"id":…,"lat":…,"lon":…} is several times the size for the same numbers,
	// and every byte of it crosses the wire before the map can draw.
	flat := make([][3]float64, 0, len(points))
	for _, p := range points {
		flat = append(flat, [3]float64{float64(p.ID), p.Lat, p.Lon})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"count":     len(flat),
		"truncated": len(flat) == limit,
		"points":    flat,
	})
}

// parseBBox reads "west,south,east,north" in degrees.
func parseBBox(raw string) (west, south, east, north float64, ok bool, err error) {
	if raw == "" {
		return 0, 0, 0, 0, false, nil
	}

	parts := strings.Split(raw, ",")
	if len(parts) != 4 {
		return 0, 0, 0, 0, false, errors.New("bbox must be west,south,east,north")
	}

	values := make([]float64, 4)
	for i, part := range parts {
		parsed, parseErr := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if parseErr != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, 0, 0, 0, false, errors.New("bbox values must be numbers")
		}
		values[i] = parsed
	}

	west, south, east, north = values[0], values[1], values[2], values[3]
	if south > north {
		south, north = north, south
	}
	// Clamped rather than rejected: a map dragged past the pole produces
	// out-of-range latitudes constantly, and refusing them would make the view
	// stutter at the edges for no benefit.
	south, north = math.Max(south, -90), math.Min(north, 90)
	west, east = math.Max(west, -180), math.Min(east, 180)

	return west, south, east, north, true, nil
}

// handleMuseum returns one museum by id, so a result can be linked to.
func (s *Server) handleMuseum(w http.ResponseWriter, r *http.Request) {
	hit, err := s.catalogue.MuseumByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, postgres.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, museumHitFrom(hit, 0))
}

// handleSearch answers name queries.
//
// This is the only interface that reaches museums with no coordinates, and the
// only one tolerant of a misspelling.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, errors.New("q is required"))
		return
	}
	if len([]rune(query)) > maxQueryRunes {
		writeError(w, http.StatusBadRequest, fmt.Errorf("q must be %d characters or fewer", maxQueryRunes))
		return
	}

	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeQueryError(w, r, err)
		return
	}
	offset, err := parseOffset(r.URL.Query().Get("offset"))
	if err != nil {
		writeQueryError(w, r, err)
		return
	}

	page, err := s.catalogue.Search(r.Context(), query, limit, offset)
	if err != nil {
		writeServerError(w, r, err)
		return
	}

	museums := make([]searchHit, 0, len(page.Hits))
	for _, hit := range page.Hits {
		m := hit.Museum
		museums = append(museums, searchHit{
			ID:   hit.ID,
			Name: m.Name, Country: m.Country, Locality: m.Locality, Aliases: m.AlsoKnownAs,
			Latitude: m.Latitude, Longitude: m.Longitude,
			Website: m.Website, Wikipedia: m.WikipediaURL, WikidataID: m.WikidataID,
			Locatable: m.HasCoordinates(), Score: round2(hit.Score),
		})
	}

	writeJSON(w, http.StatusOK, searchResponse{
		Count:   len(museums),
		Total:   page.Total,
		HasMore: int64(offset+len(museums)) < page.Total,
		Limit:   limit,
		Offset:  offset,
		Query:   query,
		Museums: museums,
	})
}

func (s *Server) handleExhibitions(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()

	// Upcoming exhibitions are excluded by default: the common question is what
	// can be seen now.
	includeUpcoming := values.Get("upcoming") == "true"

	text := strings.TrimSpace(values.Get("q"))
	// Except when a name was given. Someone searching for a title wants that
	// title, open yet or not — asking for "hasselblad" and being told nothing
	// exists, because every one of its shows opens next month, is not an answer
	// to the question that was asked.
	if text != "" && values.Get("upcoming") != "false" {
		includeUpcoming = true
	}
	if len([]rune(text)) > maxQueryRunes {
		writeError(w, http.StatusBadRequest, fmt.Errorf("q must be %d characters or fewer", maxQueryRunes))
		return
	}

	// A search may name a place or not. Without one it searches everywhere,
	// which is the point: someone who knows a show's name rarely knows which
	// town it is in, and requiring a location made the name useless.
	located := values.Get("lat") != "" || values.Get("lon") != "" || values.Get("place") != ""
	if text != "" && !located {
		s.searchExhibitions(w, r, text, query{limit: defaultLimit}, false, includeUpcoming)
		return
	}

	q, err := s.parseQuery(r)
	if err != nil {
		writeQueryError(w, r, err)
		return
	}
	if text != "" {
		s.searchExhibitions(w, r, text, q, true, includeUpcoming)
		return
	}

	hits, err := s.catalogue.ExhibitionsNearby(r.Context(), q.lat, q.lon, q.radiusKm, includeUpcoming, q.limit)
	if err != nil {
		writeServerError(w, r, err)
		return
	}

	found := make([]exhibitionHit, 0, len(hits))
	for _, hit := range hits {
		found = append(found, exhibitionHit{
			Title: hit.Title, URL: hit.URL, Museum: hit.Museum,
			MuseumWikidataID: hit.MuseumWikidataID,
			DistanceKm:       round2(hit.DistanceKm),
			Start:            hit.Start, End: hit.End,
			Running: hit.Running, Upcoming: hit.Upcoming,
			Permanent: hit.Permanent,
			Latitude:  hit.Latitude, Longitude: hit.Longitude,
			ScrapedAt: hit.ScrapedAt,
		})
	}

	writeJSON(w, http.StatusOK, exhibitionResponse{
		Count: len(found), Exhibitions: found, Query: echo(q),
		Coverage: s.coverageFor(r, q, len(found)),
	})
}

// searchExhibitions answers a search by name, with or without a place.
//
// No coverage report: coverage answers "has anyone looked here yet", which is a
// question about an area. A search that found nothing means the catalogue holds
// no such title, and pointing at an area's scrape history would not explain it.
func (s *Server) searchExhibitions(w http.ResponseWriter, r *http.Request, text string, q query, near, includeUpcoming bool) {
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeQueryError(w, r, err)
		return
	}
	offset, err := parseOffset(r.URL.Query().Get("offset"))
	if err != nil {
		writeQueryError(w, r, err)
		return
	}

	hits, total, err := s.catalogue.SearchExhibitions(r.Context(), text,
		q.lat, q.lon, q.radiusKm, near, includeUpcoming, limit, offset)
	if err != nil {
		writeServerError(w, r, err)
		return
	}

	found := make([]exhibitionHit, 0, len(hits))
	for _, hit := range hits {
		found = append(found, exhibitionHit{
			Title: hit.Title, URL: hit.URL, Museum: hit.Museum,
			MuseumWikidataID: hit.MuseumWikidataID,
			DistanceKm:       round2(hit.DistanceKm),
			Start:            hit.Start, End: hit.End,
			Running: hit.Running, Upcoming: hit.Upcoming,
			Permanent: hit.Permanent,
			Latitude:  hit.Latitude, Longitude: hit.Longitude,
			ScrapedAt: hit.ScrapedAt,
		})
	}

	echoed := echo(q)
	echoed.Limit, echoed.Offset, echoed.Text = limit, offset, text

	writeJSON(w, http.StatusOK, exhibitionResponse{
		Count: len(found), Total: total, Exhibitions: found, Query: echoed,
	})
}

// coverageFor explains a result, and is most useful when there is none.
//
// A failure to read coverage is not worth failing the request over: the
// exhibitions themselves are already in hand, and the explanation is a
// courtesy.
func (s *Server) coverageFor(r *http.Request, q query, found int) *coverageReport {
	coverage, err := s.catalogue.ExhibitionCoverage(r.Context(), q.lat, q.lon, q.radiusKm)
	if err != nil {
		log.Printf("api: coverage unavailable: %v", err)
		return nil
	}

	report := &coverageReport{
		MuseumsInArea:   coverage.MuseumsInArea,
		MuseumsWithSite: coverage.MuseumsWithSite,
		LastScraped:     coverage.LastScraped,
	}

	switch {
	case found > 0:
	case coverage.MuseumsInArea == 0:
		report.Note = "no museums are known in this area; try a larger radius"
	case coverage.LastScraped == nil && coverage.MuseumsWithSite > 0:
		report.Note = fmt.Sprintf(
			"no exhibitions have been collected here yet: %d museums have a website but none has been scraped. Run \"museum refresh\" for this area.",
			coverage.MuseumsWithSite)
	case coverage.MuseumsWithSite == 0:
		report.Note = "no museum in this area has a website to read exhibitions from"
	default:
		report.Note = "this area has been scraped, but nothing is on show right now"
	}
	return report
}

// museumHitFrom converts a stored museum into the response shape.
func museumHitFrom(hit postgres.Hit, distanceKm float64) museumHit {
	m := hit.Museum
	return museumHit{
		ID: hit.ID, Name: m.Name, DistanceKm: distanceKm,
		ApproximateLocation: hit.ApproximateLocation,
		Verified:            m.Verified,
		Country:             m.Country, Locality: m.Locality, Description: m.Description,
		Latitude: m.Latitude, Longitude: m.Longitude,
		Website: m.Website, WikipediaURL: m.WikipediaURL,
		WikidataID: m.WikidataID, Sources: m.Sources, Classes: m.Classes,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("api: write response: %v", err)
	}
}

// writeQueryError reports a failure to understand or resolve a request.
//
// Most are the caller's fault and answer 400. An unresolvable place name is
// different: the request was well formed and the answer is simply that no such
// place is known, which is a 404. A geocoder that is unreachable is our
// problem, not theirs.
func writeQueryError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, postgres.ErrPlaceUnknown):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeServerError(w, r, err)
	case strings.HasPrefix(err.Error(), "geocode "):
		writeServerError(w, r, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

// writeError reports a fault in the request itself. The message describes what
// the caller got wrong, so it is safe — and useful — to send back.
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// writeServerError reports a fault on our side, and deliberately says almost
// nothing.
//
// The driver's text was being sent verbatim to unauthenticated callers, which
// disclosed the database user, the database name, the internal hostname and the
// resolver address. The password was redacted by pgx, but any future SQL error
// would have exposed schema detail by the same route. The detail belongs in the
// log, where an operator can reach it and a stranger cannot.
//
// A cancelled request is not a failure worth reporting: the client has already
// gone, and counting it as a gateway error makes an ordinary disconnect look
// like an outage. A deadline exceeded is a real failure, and reported as one.
func writeServerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		log.Printf("api: %s %s abandoned by the client", r.Method, r.URL.Path)
		return
	case errors.Is(err, context.DeadlineExceeded):
		log.Printf("api: %s %s exceeded the %s budget: %v", r.Method, r.URL.Path, requestTimeout, err)
		writeError(w, http.StatusGatewayTimeout, errors.New("the query took too long"))
	default:
		log.Printf("api: %s %s failed: %v", r.Method, r.URL.Path, err)
		writeError(w, http.StatusBadGateway, errors.New("the catalogue is unavailable"))
	}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// logRequests records each request with its status and duration.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		log.Printf("%s %s%s -> %d in %s",
			r.Method, r.URL.Path, querySuffix(r), recorder.status, time.Since(start).Round(time.Millisecond))
	})
}

func querySuffix(r *http.Request) string {
	raw := r.URL.RawQuery
	if raw == "" {
		return ""
	}
	if len(raw) > maxLoggedQueryChars {
		raw = raw[:maxLoggedQueryChars] + "...(truncated)"
	}
	return "?" + raw
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
