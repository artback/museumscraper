package location

import (
	"net"
	"net/http"
	"sync"
	"time"
)

var (
	sharedClient     *http.Client
	sharedClientOnce sync.Once
	rateLimiter      <-chan time.Time
	rateLimiterOnce  sync.Once
)

// Client returns a shared HTTP client configured with sensible timeouts
// and connection pooling for the Nominatim API.
func Client() *http.Client {
	sharedClientOnce.Do(func() {
		sharedClient = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				MaxIdleConnsPerHost: 10,
				MaxConnsPerHost:     10,
				IdleConnTimeout:     90 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		}
	})
	return sharedClient
}

// rateLimit returns a ticker channel that enforces Nominatim's 1 request/second
// policy. Callers should receive from this channel before making an API call.
func rateLimit() <-chan time.Time {
	rateLimiterOnce.Do(func() {
		rateLimiter = time.Tick(1 * time.Second)
	})
	return rateLimiter
}
