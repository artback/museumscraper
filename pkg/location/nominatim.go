package location

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	nominatimSearchURL  = "https://nominatim.openstreetmap.org/search"
	nominatimDetailsURL = "https://nominatim.openstreetmap.org/details"
	nominatimUserAgent  = "museumscraper/1.0"
	nominatimMaxRetries = 3
)

// NominatimGeocoder implements Geocoder and PlaceDetailer using the
// Nominatim (OpenStreetMap) API. It is free, requires no API key, but
// enforces a strict 1 request/second usage policy.
//
// This implementation:
//   - Enforces the rate limit with a time.Ticker
//   - Retries transient failures (5xx, 429) with exponential backoff
//   - Uses a custom HTTP client with timeouts and connection pooling
//   - Propagates context for cancellation
type NominatimGeocoder struct {
	client  *http.Client
	limiter *time.Ticker
}

// NewNominatimGeocoder creates a Nominatim geocoder that respects the
// 1 request/second rate limit.
func NewNominatimGeocoder() *NominatimGeocoder {
	return &NominatimGeocoder{
		client:  newHTTPClient(),
		limiter: time.NewTicker(1 * time.Second),
	}
}

// Close stops the internal rate limiter. Call when the geocoder is no longer needed.
func (n *NominatimGeocoder) Close() {
	n.limiter.Stop()
}

func (n *NominatimGeocoder) Geocode(ctx context.Context, query string) (*GeoResult, error) {
	// Wait for rate limiter
	select {
	case <-n.limiter.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("addressdetails", "1")
	params.Set("limit", "1")
	params.Set("accept-language", "en")

	reqURL := fmt.Sprintf("%s?%s", nominatimSearchURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("nominatim: create request: %w", err)
	}
	req.Header.Set("User-Agent", nominatimUserAgent)

	resp, err := doWithRetry(ctx, n.client, req, nominatimMaxRetries)
	if err != nil {
		return nil, fmt.Errorf("nominatim: search %q: %w", query, err)
	}
	defer resp.Body.Close()

	var results NominatimResponse
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, fmt.Errorf("nominatim: decode response: %w", err)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("nominatim: no results for %q", query)
	}

	return nominatimToGeoResult(&results[0]), nil
}

func (n *NominatimGeocoder) PlaceDetails(ctx context.Context, osmType string, osmID int64) (*NominatimDetailsResponse, error) {
	select {
	case <-n.limiter.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	params := url.Values{}
	params.Set("osmtype", osmType)
	params.Set("osmid", fmt.Sprintf("%d", osmID))
	params.Set("addressdetails", "1")
	params.Set("hierarchy", "0")
	params.Set("group_hierarchy", "1")
	params.Set("format", "json")

	reqURL := fmt.Sprintf("%s?%s", nominatimDetailsURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("nominatim: create request: %w", err)
	}
	req.Header.Set("User-Agent", nominatimUserAgent)

	resp, err := doWithRetry(ctx, n.client, req, nominatimMaxRetries)
	if err != nil {
		return nil, fmt.Errorf("nominatim: details osmtype=%s osmid=%d: %w", osmType, osmID, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("nominatim: read body: %w", err)
	}

	var details NominatimDetailsResponse
	if err := json.Unmarshal(body, &details); err != nil {
		return nil, fmt.Errorf("nominatim: decode details: %w", err)
	}

	return &details, nil
}

func nominatimToGeoResult(loc *NominatimLocation) *GeoResult {
	lat, _ := strconv.ParseFloat(loc.Lat, 64)
	lon, _ := strconv.ParseFloat(loc.Lon, 64)

	city := loc.Address.City
	if city == "" {
		city = loc.Address.Town
	}
	if city == "" {
		city = loc.Address.Village
	}

	return &GeoResult{
		PlaceID:     loc.PlaceID,
		OsmType:     loc.OsmType,
		OsmID:       loc.OsmID,
		Lat:         lat,
		Lon:         lon,
		Name:        loc.Name,
		DisplayName: loc.DisplayName,
		Class:       loc.Class,
		Type:        loc.Type,
		Importance:  loc.Importance,
		Country:     loc.Address.Country,
		CountryCode: loc.Address.CountryCode,
		City:        city,
		Postcode:    loc.Address.Postcode,
	}
}
