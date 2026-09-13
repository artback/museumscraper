package registers

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"museum/internal/models"
	"museum/pkg/licence"
)

func TestCleanURL(t *testing.T) {
	cases := map[string]string{
		// IMLS stores every URL upper-cased. Scheme and host are
		// case-insensitive; the path is not.
		"HTTP://WWW.MOBILEMUSEUMOFART.COM/":    "http://www.mobilemuseumofart.com/",
		"HTTPS://MMFA.ORG/Exhibitions/Current": "https://mmfa.org/Exhibitions/Current",
		"https://museepompiers.com/":           "https://museepompiers.com/",
		"www.example-museum.org":               "https://www.example-museum.org",
		"  ":                                   "",
		"ftp://files.example.org":              "",
		"not a url":                            "",
		// A host with no dot is not one.
		"http://localhost/admin": "",
	}
	for raw, want := range cases {
		if got := cleanURL(raw); got != want {
			t.Errorf("cleanURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestCleanURLKeepsPathCase is the point of doing this by hand: a lower-cased
// path 404s on plenty of sites, and the sweep would read that as a museum whose
// site is unreachable and back it off.
func TestCleanURLKeepsPathCase(t *testing.T) {
	if got := cleanURL("HTTP://EXAMPLE.ORG/WhatsOn"); got != "http://example.org/WhatsOn" {
		t.Errorf("cleanURL lower-cased the path: %q", got)
	}
}

func TestTitleCase(t *testing.T) {
	cases := map[string]string{
		"MOBILE MUSEUM OF ART":     "Mobile Museum of Art",
		"HISTORY MUSEUM OF MOBILE": "History Museum of Mobile",
		"USS CONSTITUTION MUSEUM":  "USS Constitution Museum",
		"O'BRIEN HOUSE MUSEUM":     "O'Brien House Museum",
		"THE MUSEUM OF FLIGHT":     "The Museum of Flight",
		"YMCA HERITAGE CENTER":     "YMCA Heritage Center",
		"WELLS-FARGO MUSEUM":       "Wells-Fargo Museum",
		// Already mixed case: left alone entirely, since the register knows
		// better than the heuristic does.
		"musée des sapeurs-pompiers": "musée des sapeurs-pompiers",
		"Museum of London":           "Museum of London",
	}
	for raw, want := range cases {
		if got := titleCase(raw); got != want {
			t.Errorf("titleCase(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestServiceStreamsAndLabels checks the two things read() adds to every
// record: the register's country, which the merger cannot match a record
// without, and the source name, which is what its licence is looked up by.
func TestServiceStreamsAndLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Nom_officiel|Ville|URL|Coordonnees|Identifiant\n" +
			"musée de test|Lyon|https://example.org|45.5, 4.5|M1\n"))
	}))
	defer srv.Close()

	s := &Service{
		client: srv.Client(),
		agent:  "test",
		datasets: []Dataset{{
			Source: "test-register", Country: "France", URL: srv.URL, Parse: parseMuseofile,
		}},
	}

	var got []models.Museum
	for museum := range s.Museums(context.Background()) {
		got = append(got, museum)
	}

	if len(got) != 1 {
		t.Fatalf("got %d museums, want 1", len(got))
	}
	if got[0].Country != "France" {
		t.Errorf("Country = %q, want France", got[0].Country)
	}
	if len(got[0].Sources) != 1 || got[0].Sources[0] != "test-register" {
		t.Errorf("Sources = %v, want [test-register]", got[0].Sources)
	}
}

// TestServiceSkipsARegisterItCannotRead: one unreachable government host must
// not cost a crawl that is also collecting from three working APIs.
func TestServiceSkipsARegisterItCannotRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken" {
			http.Error(w, "gone", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("Nom_officiel|Ville\nmusée de test|Lyon\n"))
	}))
	defer srv.Close()

	s := &Service{
		client: srv.Client(),
		agent:  "test",
		datasets: []Dataset{
			{Source: "broken", Country: "France", URL: srv.URL + "/broken", Parse: parseMuseofile},
			{Source: "working", Country: "France", URL: srv.URL + "/ok", Parse: parseMuseofile},
		},
	}

	var count int
	for range s.Museums(context.Background()) {
		count++
	}
	if count != 1 {
		t.Errorf("got %d museums, want the 1 from the register that answered", count)
	}
}

// zipOf builds a zip the way the IMLS file is shaped, for the parser tests.
func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestEveryDatasetHasALicence: a register whose source name has no licence
// entry is redistributed with no notice attached, which is the one thing the
// attribution work exists to prevent. This is the invariant that keeps adding
// a register from quietly skipping that step.
func TestEveryDatasetHasALicence(t *testing.T) {
	for _, dataset := range Datasets {
		l, ok := licence.For(dataset.Source)
		if !ok {
			t.Errorf("%s has no licence entry", dataset.Source)
			continue
		}
		if l.Attribution == "" {
			t.Errorf("%s (%s) carries no credit line", dataset.Source, l.Name)
		}
		if dataset.Country == "" || dataset.URL == "" || dataset.Parse == nil {
			t.Errorf("%s is incompletely described: %+v", dataset.Source, dataset)
		}
	}
}
