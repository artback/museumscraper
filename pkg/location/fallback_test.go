package location

import (
	"context"
	"errors"
	"testing"
)

// mockGeocoder is a test helper that returns canned results or errors.
type mockGeocoder struct {
	result *GeoResult
	err    error
	called bool
}

func (m *mockGeocoder) Geocode(_ context.Context, _ string) (*GeoResult, error) {
	m.called = true
	return m.result, m.err
}

func TestFallbackGeocoder_UsesFirstSuccess(t *testing.T) {
	expected := &GeoResult{Name: "Louvre", Country: "France"}
	first := &mockGeocoder{result: expected}
	second := &mockGeocoder{result: &GeoResult{Name: "Other"}}

	fb := NewFallbackGeocoder(first, second)
	got, err := fb.Geocode(context.Background(), "Louvre")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "Louvre" {
		t.Errorf("got Name=%s, want Louvre", got.Name)
	}
	if second.called {
		t.Error("second geocoder should not have been called")
	}
}

func TestFallbackGeocoder_FallsBackOnError(t *testing.T) {
	first := &mockGeocoder{err: errors.New("rate limited")}
	expected := &GeoResult{Name: "Louvre", Country: "France"}
	second := &mockGeocoder{result: expected}

	fb := NewFallbackGeocoder(first, second)
	got, err := fb.Geocode(context.Background(), "Louvre")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !first.called {
		t.Error("first geocoder should have been called")
	}
	if got.Name != "Louvre" {
		t.Errorf("got Name=%s, want Louvre", got.Name)
	}
}

func TestFallbackGeocoder_AllFail(t *testing.T) {
	first := &mockGeocoder{err: errors.New("error 1")}
	second := &mockGeocoder{err: errors.New("error 2")}

	fb := NewFallbackGeocoder(first, second)
	_, err := fb.Geocode(context.Background(), "Nowhere")
	if err == nil {
		t.Fatal("expected error when all geocoders fail")
	}
	if !first.called || !second.called {
		t.Error("both geocoders should have been called")
	}
}
