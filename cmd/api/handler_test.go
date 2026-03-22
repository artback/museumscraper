package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---------------------------------------------------------------------------
// Tests for query-parsing helpers
// ---------------------------------------------------------------------------

func TestQueryInt(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		key        string
		defaultVal int
		wantVal    int
		wantErr    bool
	}{
		{name: "missing key returns default", query: "", key: "limit", defaultVal: 50, wantVal: 50},
		{name: "valid integer", query: "limit=10", key: "limit", defaultVal: 50, wantVal: 10},
		{name: "zero value", query: "offset=0", key: "offset", defaultVal: 5, wantVal: 0},
		{name: "negative integer", query: "offset=-1", key: "offset", defaultVal: 0, wantVal: -1},
		{name: "non-numeric string", query: "limit=abc", key: "limit", defaultVal: 50, wantErr: true},
		{name: "float string", query: "limit=3.14", key: "limit", defaultVal: 50, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/?"+tt.query, nil)
			got, err := queryInt(r, tt.key, tt.defaultVal)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantVal {
				t.Errorf("got %d, want %d", got, tt.wantVal)
			}
		})
	}
}

func TestQueryFloat(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		key       string
		wantVal   float64
		wantFound bool
		wantErr   bool
	}{
		{name: "missing key", query: "", key: "lat", wantVal: 0, wantFound: false},
		{name: "valid float", query: "lat=48.8566", key: "lat", wantVal: 48.8566, wantFound: true},
		{name: "negative float", query: "lon=-2.3522", key: "lon", wantVal: -2.3522, wantFound: true},
		{name: "integer as float", query: "lat=10", key: "lat", wantVal: 10.0, wantFound: true},
		{name: "non-numeric", query: "lat=abc", key: "lat", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/?"+tt.query, nil)
			got, found, err := queryFloat(r, tt.key)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if found != tt.wantFound {
				t.Errorf("found = %v, want %v", found, tt.wantFound)
			}
			if got != tt.wantVal {
				t.Errorf("got %f, want %f", got, tt.wantVal)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests for JSON response helpers
// ---------------------------------------------------------------------------

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusCreated, map[string]string{"hello": "world"})

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if body["hello"] != "world" {
		t.Errorf("body = %v, want {hello:world}", body)
	}
}

func TestWriteList(t *testing.T) {
	w := httptest.NewRecorder()
	writeList(w, []string{"a", "b"}, 2)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body listResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if body.Count != 2 {
		t.Errorf("count = %d, want 2", body.Count)
	}
}

func TestWriteItem(t *testing.T) {
	w := httptest.NewRecorder()
	writeItem(w, map[string]int{"id": 42})

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body itemResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if body.Data == nil {
		t.Error("data should not be nil")
	}
}

func TestWriteError(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, http.StatusBadRequest, "bad input")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if body.Error != "bad input" {
		t.Errorf("error = %q, want %q", body.Error, "bad input")
	}
}

// ---------------------------------------------------------------------------
// Handler tests – parameter validation (returns 400 before hitting DB)
// ---------------------------------------------------------------------------

// newTestMux builds a mux wired to a Handler with nil repos. Requests that
// pass validation will panic (nil pointer) but validation-failure paths
// return 400 without touching the repos.
func newTestMux() *http.ServeMux {
	h := &Handler{} // nil repos
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/museums", h.ListMuseums)
	mux.HandleFunc("GET /api/v1/museums/{id}", h.GetMuseum)
	mux.HandleFunc("GET /api/v1/museums/search", h.SearchMuseums)
	mux.HandleFunc("GET /api/v1/museums/nearby", h.NearbyMuseums)
	mux.HandleFunc("GET /api/v1/museums/city/{city}", h.MuseumsByCity)
	mux.HandleFunc("GET /api/v1/museums/country/{country}", h.MuseumsByCountry)
	mux.HandleFunc("GET /api/v1/exhibitions/city/{city}", h.ExhibitionsByCity)
	mux.HandleFunc("GET /api/v1/exhibitions/nearby", h.ExhibitionsNearby)
	mux.HandleFunc("GET /health", h.HealthCheck)
	return mux
}

func TestHealthCheck(t *testing.T) {
	mux := newTestMux()
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want %q", body["status"], "ok")
	}
}

func TestListMuseums_InvalidLimit(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "limit zero", query: "limit=0"},
		{name: "limit negative", query: "limit=-5"},
		{name: "limit too large", query: "limit=501"},
		{name: "limit non-numeric", query: "limit=abc"},
	}
	mux := newTestMux()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/museums?"+tt.query, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestListMuseums_InvalidOffset(t *testing.T) {
	mux := newTestMux()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/museums?offset=-1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestGetMuseum_InvalidID(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{name: "non-numeric", id: "abc"},
		{name: "float", id: "3.14"},
	}
	mux := newTestMux()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/museums/"+tt.id, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestSearchMuseums_MissingQuery(t *testing.T) {
	mux := newTestMux()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/museums/search", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	var body errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if body.Error == "" {
		t.Error("expected non-empty error message")
	}
}

func TestSearchMuseums_InvalidLimit(t *testing.T) {
	mux := newTestMux()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/museums/search?q=test&limit=0", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestNearbyMuseums_MissingLatLon(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "missing both", query: ""},
		{name: "missing lon", query: "lat=48.8"},
		{name: "missing lat", query: "lon=2.3"},
	}
	mux := newTestMux()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/museums/nearby?"+tt.query, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestNearbyMuseums_InvalidCoords(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "lat too high", query: "lat=91&lon=0"},
		{name: "lat too low", query: "lat=-91&lon=0"},
		{name: "lon too high", query: "lat=0&lon=181"},
		{name: "lon too low", query: "lat=0&lon=-181"},
	}
	mux := newTestMux()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/museums/nearby?"+tt.query, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestNearbyMuseums_RadiusTooLarge(t *testing.T) {
	mux := newTestMux()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/museums/nearby?lat=48&lon=2&radius=100001", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestNearbyMuseums_InvalidRadius(t *testing.T) {
	mux := newTestMux()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/museums/nearby?lat=48&lon=2&radius=abc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestNearbyMuseums_InvalidLimitParam(t *testing.T) {
	mux := newTestMux()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/museums/nearby?lat=48&lon=2&limit=0", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestExhibitionsNearby_MissingLatLon(t *testing.T) {
	mux := newTestMux()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/exhibitions/nearby", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestExhibitionsNearby_InvalidCoords(t *testing.T) {
	mux := newTestMux()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/exhibitions/nearby?lat=91&lon=0", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestExhibitionsNearby_RadiusTooLarge(t *testing.T) {
	mux := newTestMux()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/exhibitions/nearby?lat=48&lon=2&radius=100001", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}
