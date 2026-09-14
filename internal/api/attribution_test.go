package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"museum/internal/models"
	"museum/internal/postgres"
	"museum/pkg/licence"
)

// TestMuseumResponseCarriesAttribution: ODbL and CC BY-SA require the credit to
// travel with the data. A client holding a page of these records carries those
// obligations whether or not anyone read the documentation, so the response has
// to say which ones.
func TestMuseumResponseCarriesAttribution(t *testing.T) {
	c := &fakeCatalogue{nearby: []postgres.Hit{
		{Museum: models.Museum{
			Name: "Rijksmuseum", Country: "Netherlands", Latitude: 52.36, Longitude: 4.88,
			Description: "Dutch national museum", Sources: []string{"wikipedia-list", "openstreetmap"},
		}},
		{Museum: models.Museum{
			Name: "Mauritshuis", Country: "Netherlands", Latitude: 52.08, Longitude: 4.31,
			Sources: []string{"wikidata"},
		}},
	}}
	srv := httptest.NewServer(NewServer(c).Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/museums?lat=52.36&lon=4.88&radius_km=5")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body struct {
		Attribution []licence.Licence `json:"attribution"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{"ODbL 1.0": false, "CC BY-SA 4.0": false, "CC0 1.0": false}
	for _, l := range body.Attribution {
		if _, expected := want[l.Name]; !expected {
			t.Errorf("unexpected licence %q", l.Name)
			continue
		}
		want[l.Name] = true
		if l.ShareAlike && l.Attribution == "" {
			t.Errorf("%s is share-alike but carries no credit line", l.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("%s missing from the response attribution", name)
		}
	}
}

// TestAttributionCoversGeocodedPositions: a museum Wikidata named but Nominatim
// placed carries ODbL for its position, even though Wikidata itself is CC0.
func TestAttributionCoversGeocodedPositions(t *testing.T) {
	c := &fakeCatalogue{nearby: []postgres.Hit{
		{Museum: models.Museum{
			Name: "Village Museum", Country: "Sweden", Latitude: 57.7, Longitude: 11.9,
			Sources: []string{"wikidata"},
		}, ApproximateLocation: true},
	}}
	srv := httptest.NewServer(NewServer(c).Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/museums?lat=57.7&lon=11.9")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body struct {
		Attribution []licence.Licence `json:"attribution"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	var hasODbL bool
	for _, l := range body.Attribution {
		hasODbL = hasODbL || l.Name == "ODbL 1.0"
	}
	if !hasODbL {
		t.Errorf("a geocoded position was served without OpenStreetMap's licence: %+v", body.Attribution)
	}
}

func TestAttributionEndpoint(t *testing.T) {
	srv := httptest.NewServer(NewServer(&fakeCatalogue{}).Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/attribution")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %s", resp.Status)
	}

	var body struct {
		Licences []licence.Licence `json:"licences"`
		Note     string            `json:"note"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Licences) < 4 || body.Note == "" {
		t.Errorf("thin attribution document: %+v", body)
	}
	for _, l := range body.Licences {
		if l.Source == "" || l.Name == "" {
			t.Errorf("incomplete entry: %+v", l)
		}
	}
}
