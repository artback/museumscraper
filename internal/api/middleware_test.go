package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getWith(t *testing.T, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for key, values := range header {
		req.Header[key] = values
	}
	rec := httptest.NewRecorder()
	NewServer(&fakeCatalogue{}).Routes().ServeHTTP(rec, req)
	return rec
}

func gzipHeader() http.Header {
	return http.Header{"Accept-Encoding": {"gzip"}}
}

func TestCompression_CompressesJSON(t *testing.T) {
	rec := getWith(t, "/v1/museums?lat=0&lon=0", gzipHeader())

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", got)
	}
	// A Content-Length left over from the uncompressed body makes a client wait
	// for bytes that are never sent.
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("content-length = %q, want it removed", got)
	}
	// Joined rather than read with Get: CORS already set Vary: Origin, so this
	// is the second value and Get would only ever return the first.
	if vary := rec.Header()["Vary"]; !strings.Contains(strings.Join(vary, ","), "Accept-Encoding") {
		t.Errorf("vary = %v, want it to include Accept-Encoding", vary)
	}

	reader, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "museums") {
		t.Errorf("decompressed body = %.120s", body)
	}
}

func TestCompression_LeavesAClientThatCannotTakeItAlone(t *testing.T) {
	rec := getWith(t, "/v1/museums?lat=0&lon=0", nil)

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("content-encoding = %q, want none", got)
	}
	if !strings.Contains(rec.Body.String(), "museums") {
		t.Errorf("body = %.120s", rec.Body)
	}
}

// The vendored library is gzipped at build time and served as-is. Compressing
// it again would spend CPU to make it fractionally larger, and would leave a
// body the client double-decodes.
func TestCompression_DoesNotRecompressTheVendoredLibrary(t *testing.T) {
	rec := getWith(t, "/map/vendor/maplibre-gl.js", gzipHeader())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header()["Content-Encoding"]; len(got) != 1 || got[0] != "gzip" {
		t.Fatalf("content-encoding = %v, want exactly one gzip", got)
	}
	// One layer, not two: this must be readable after a single decompression.
	reader, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := gzip.NewReader(strings.NewReader(string(body))); err == nil {
		t.Error("body was compressed twice")
	}
}

func TestMapPage_CarriesASecurityPolicy(t *testing.T) {
	rec := get(t, &fakeCatalogue{}, "/map")

	policy := rec.Header().Get("Content-Security-Policy")
	if policy == "" {
		t.Fatal("no Content-Security-Policy on the map page")
	}
	// The page's own code is served as modules from this origin, so nothing
	// inline needs to run. If that stops being true the policy must be the
	// thing that is changed deliberately, not quietly widened.
	if strings.Contains(policy, "script-src 'self' 'unsafe-inline'") {
		t.Error("script-src allows inline scripts; the page should not need it")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("x-content-type-options = %q, want nosniff", got)
	}
}
