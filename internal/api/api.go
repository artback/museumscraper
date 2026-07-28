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
	"strconv"
	"strings"
	"time"

	"museum/internal/models"
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
)

// Catalogue is what the API needs from the database.
//
// It is an interface so the handlers can be tested without a database, and so
// the storage layer can be replaced without touching them.
type Catalogue interface {
	Nearby(ctx context.Context, lat, lon, radiusKm float64, limit int) ([]postgres.Hit, error)
	Search(ctx context.Context, query string, limit int) ([]postgres.Hit, error)
	ExhibitionsNearby(ctx context.Context, lat, lon, radiusKm float64, includeUpcoming bool, limit int) ([]postgres.ExhibitionHit, error)
	Counts(ctx context.Context) (postgres.Counts, error)
	Ping(ctx context.Context) error
}

// Server answers catalogue queries over HTTP.
type Server struct {
	catalogue Catalogue
}

// NewServer returns a Server backed by the catalogue.
func NewServer(catalogue Catalogue) *Server {
	return &Server{catalogue: catalogue}
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
}

// parseQuery reads and validates the shared query parameters.
func parseQuery(r *http.Request) (query, error) {
	values := r.URL.Query()

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

	return query{lat: lat, lon: lon, radiusKm: radius, limit: limit}, nil
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
}

func echo(q query) responseQuery {
	return responseQuery{Latitude: q.lat, Longitude: q.lon, RadiusKm: q.radiusKm, Limit: q.limit}
}

type museumResponse struct {
	Count   int           `json:"count"`
	Museums []museumHit   `json:"museums"`
	Query   responseQuery `json:"query"`
}

type museumHit struct {
	Name         string   `json:"name"`
	DistanceKm   float64  `json:"distance_km"`
	Country      string   `json:"country,omitempty"`
	Locality     string   `json:"locality,omitempty"`
	Description  string   `json:"description,omitempty"`
	Latitude     float64  `json:"latitude"`
	Longitude    float64  `json:"longitude"`
	Website      string   `json:"website,omitempty"`
	WikipediaURL string   `json:"wikipedia_url,omitempty"`
	WikidataID   string   `json:"wikidata_id,omitempty"`
	Sources      []string `json:"sources,omitempty"`
}

type searchResponse struct {
	Count   int         `json:"count"`
	Query   string      `json:"query"`
	Museums []searchHit `json:"museums"`
}

type searchHit struct {
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
	Count       int             `json:"count"`
	Exhibitions []exhibitionHit `json:"exhibitions"`
	Query       responseQuery   `json:"query"`
}

type exhibitionHit struct {
	Title      string     `json:"title"`
	URL        string     `json:"url"`
	Museum     string     `json:"museum"`
	DistanceKm float64    `json:"distance_km"`
	Start      *time.Time `json:"start,omitempty"`
	End        *time.Time `json:"end,omitempty"`
	Running    bool       `json:"running"`
	Upcoming   bool       `json:"upcoming"`
	Latitude   float64    `json:"latitude"`
	Longitude  float64    `json:"longitude"`
	ScrapedAt  time.Time  `json:"scraped_at"`
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
	q, err := parseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	hits, err := s.catalogue.Nearby(r.Context(), q.lat, q.lon, q.radiusKm, q.limit)
	if err != nil {
		writeServerError(w, r, err)
		return
	}

	museums := make([]museumHit, 0, len(hits))
	for _, hit := range hits {
		museums = append(museums, museumHitFrom(hit.Museum, round2(hit.DistanceKm)))
	}

	writeJSON(w, http.StatusOK, museumResponse{
		Count: len(museums), Museums: museums, Query: echo(q),
	})
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
		writeError(w, http.StatusBadRequest, err)
		return
	}

	hits, err := s.catalogue.Search(r.Context(), query, limit)
	if err != nil {
		writeServerError(w, r, err)
		return
	}

	museums := make([]searchHit, 0, len(hits))
	for _, hit := range hits {
		m := hit.Museum
		museums = append(museums, searchHit{
			Name: m.Name, Country: m.Country, Locality: m.Locality, Aliases: m.AlsoKnownAs,
			Latitude: m.Latitude, Longitude: m.Longitude,
			Website: m.Website, Wikipedia: m.WikipediaURL, WikidataID: m.WikidataID,
			Locatable: m.HasCoordinates(), Score: round2(hit.Score),
		})
	}

	writeJSON(w, http.StatusOK, searchResponse{Count: len(museums), Query: query, Museums: museums})
}

func (s *Server) handleExhibitions(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// Upcoming exhibitions are excluded by default: the common question is what
	// can be seen now.
	includeUpcoming := r.URL.Query().Get("upcoming") == "true"

	hits, err := s.catalogue.ExhibitionsNearby(r.Context(), q.lat, q.lon, q.radiusKm, includeUpcoming, q.limit)
	if err != nil {
		writeServerError(w, r, err)
		return
	}

	found := make([]exhibitionHit, 0, len(hits))
	for _, hit := range hits {
		found = append(found, exhibitionHit{
			Title: hit.Title, URL: hit.URL, Museum: hit.Museum,
			DistanceKm: round2(hit.DistanceKm),
			Start:      hit.Start, End: hit.End,
			Running: hit.Running, Upcoming: hit.Upcoming,
			Latitude: hit.Latitude, Longitude: hit.Longitude,
			ScrapedAt: hit.ScrapedAt,
		})
	}

	writeJSON(w, http.StatusOK, exhibitionResponse{
		Count: len(found), Exhibitions: found, Query: echo(q),
	})
}

// museumHitFrom converts a stored museum into the response shape.
func museumHitFrom(m models.Museum, distanceKm float64) museumHit {
	return museumHit{
		Name: m.Name, DistanceKm: distanceKm,
		Country: m.Country, Locality: m.Locality, Description: m.Description,
		Latitude: m.Latitude, Longitude: m.Longitude,
		Website: m.Website, WikipediaURL: m.WikipediaURL,
		WikidataID: m.WikidataID, Sources: m.Sources,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("api: write response: %v", err)
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
