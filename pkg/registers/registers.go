// Package registers reads the museum registers that governments publish as open
// data.
//
// The three existing sources all describe museums that someone chose to write
// about: Wikidata and Wikipedia hold what an editor thought notable, and
// OpenStreetMap holds what a mapper stood in front of. A national register is a
// different kind of evidence — an administrative list of institutions,
// assembled by the body that funds or accredits them — and it is strongest
// exactly where the others are weakest: the county historical museum with no
// article, no mapper and a website that has not changed since 2009.
//
// It is also the only source that arrives as a file rather than an API. A
// register is published once and downloaded whole, which makes it the cheapest
// source by far — two requests for tens of thousands of museums, against one
// Overpass query per country — and the least current. Muséofile is maintained;
// the IMLS file is a 2018 snapshot its publisher has said will not be updated
// again, so it will slowly fill with museums that have since closed. That is a
// real cost and worth stating plainly, but a museum that closed in 2021 is a
// better catalogue entry than a museum that was never listed at all, and the
// enrichment and sweep stages are what find out which is which.
package registers

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"museum/internal/models"
	"museum/pkg/useragent"
)

const (
	// requestTimeout is generous: these are multi-megabyte files from
	// government hosts that are not always quick.
	requestTimeout = 5 * time.Minute

	// maxDownloadBytes caps a register. The largest in use is about 3 MB
	// compressed, so this is two orders of magnitude of headroom and still
	// bounded.
	maxDownloadBytes = 256 << 20
)

// Dataset is one published register.
type Dataset struct {
	// Source is what records from this register carry in their Sources field,
	// and the key its licence is registered under.
	Source string
	// Country is the country the register covers, in the spelling pkg/geo
	// canonicalises to. Every record gets it: a national register says which
	// country it is for by existing, and the merger cannot fold a record into
	// another without one.
	Country string
	// URL is where the file is published.
	URL string
	// Parse turns the downloaded bytes into museums.
	Parse func(data []byte) ([]models.Museum, error)
}

// Datasets are the registers this build knows how to read.
var Datasets = []Dataset{imls, museofile}

// Service streams museums out of the published registers.
type Service struct {
	client   *http.Client
	agent    string
	datasets []Dataset
}

// NewService returns a Service reading every known register.
func NewService() *Service {
	return &Service{
		client:   &http.Client{Timeout: requestTimeout},
		agent:    useragent.For("museum registers", ""),
		datasets: Datasets,
	}
}

// Museums streams every museum the registers hold.
//
// A register that cannot be read is logged and skipped rather than failing the
// crawl, which is how the other sources treat a country that fails: one
// unreachable government host should not cost a run that is also collecting
// from three working APIs.
func (s *Service) Museums(ctx context.Context) <-chan models.Museum {
	out := make(chan models.Museum)

	go func() {
		defer close(out)

		total := 0
		for _, dataset := range s.datasets {
			if ctx.Err() != nil {
				return
			}

			museums, err := s.read(ctx, dataset)
			if err != nil {
				log.Printf("registers: skipping %s: %v", dataset.Source, err)
				continue
			}

			for _, museum := range museums {
				select {
				case out <- museum:
					total++
				case <-ctx.Done():
					return
				}
			}
			log.Printf("registers: %-10s %5d museums (running total %d)", dataset.Source, len(museums), total)
		}

		log.Printf("registers: finished, %d museums", total)
	}()

	return out
}

// read downloads and parses one register.
func (s *Service) read(ctx context.Context, dataset Dataset) ([]models.Museum, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dataset.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", s.agent)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", dataset.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %s", dataset.URL, resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dataset.URL, err)
	}

	museums, err := dataset.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", dataset.Source, err)
	}

	for i := range museums {
		museums[i].Country = dataset.Country
		museums[i].Sources = []string{dataset.Source}
	}
	return museums, nil
}

// cleanURL normalises a website as a register spells it.
//
// Registers store URLs in whatever case their data entry used, and IMLS stores
// them upper-cased throughout: "HTTP://WWW.MOBILEMUSEUMOFART.COM/". Scheme and
// host are case-insensitive and safe to lower; the path is not — plenty of
// sites serve /Exhibitions and 404 on /exhibitions — so it is left exactly as
// found. A record whose website is wrong in case is worse than one with none:
// the sweep would read it, fail, and back the site off as though the museum
// were unreachable.
func cleanURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	scheme, rest, found := strings.Cut(raw, "://")
	if !found {
		// A bare host, which several registers store. https, because a site
		// that does not serve it will redirect and one that does should not be
		// downgraded.
		scheme, rest = "https", raw
	}
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}

	host, path, hasPath := strings.Cut(rest, "/")
	host = strings.ToLower(host)
	if host == "" || !strings.Contains(host, ".") {
		return ""
	}
	if hasPath {
		return scheme + "://" + host + "/" + path
	}
	return scheme + "://" + host
}

// titleCase turns a register's upper-cased name into something readable.
//
// IMLS stores every name in capitals, and the catalogue shows names as it
// stored them: left alone, "MOBILE MUSEUM OF ART" is what a visitor sees, and
// it is also what the merger keeps as canonical when the register is the first
// source to report a museum. Lower-casing loses information the other way
// round, so short all-capital tokens that are not ordinary words are left as
// they are — "USS Constitution Museum", not "Uss Constitution Museum".
//
// Only applied to a name that is entirely upper case. A register that stores
// names properly is left alone.
func titleCase(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name != strings.ToUpper(name) {
		return name
	}

	words := strings.Fields(name)
	for i, word := range words {
		switch {
		case isLikelyAcronym(word):
			// Leave it.
		case i > 0 && lowercaseWords[strings.ToLower(word)]:
			words[i] = strings.ToLower(word)
		default:
			words[i] = capitalise(word)
		}
	}
	return strings.Join(words, " ")
}

// lowercaseWords stay lower in a title unless they lead it.
var lowercaseWords = map[string]bool{
	"a": true, "an": true, "and": true, "at": true, "by": true, "for": true,
	"in": true, "of": true, "on": true, "or": true, "the": true, "to": true,
	// The particles that appear in names the American file inherited from
	// somewhere else, and in the French register's own.
	"de": true, "del": true, "der": true, "des": true, "di": true, "du": true,
	"la": true, "le": true, "les": true, "van": true, "von": true, "y": true,
}

// isLikelyAcronym reports whether a token should keep its capitals.
//
// Short all-letter tokens that are not words: USS, NASA, YMCA, DAR. Length is
// the whole heuristic and it is not exact — "ART" as a standalone word would be
// kept — but the cost of being wrong either way is a name that reads slightly
// oddly, and the alternative is a dictionary this package has no business
// carrying.
func isLikelyAcronym(word string) bool {
	trimmed := strings.Trim(word, ".,()")
	if len(trimmed) < 2 || len(trimmed) > 5 {
		return false
	}
	if lowercaseWords[strings.ToLower(trimmed)] {
		return false
	}
	for _, r := range trimmed {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	// Anything with a vowel is more likely a short word than an acronym, with
	// the common museum-world initialisms excepted.
	if strings.ContainsAny(trimmed, "AEIOU") && !knownAcronyms[trimmed] {
		return false
	}
	return true
}

// knownAcronyms are initialisms with vowels that appear often enough in museum
// names to be worth naming.
var knownAcronyms = map[string]bool{
	"USS": true, "USA": true, "US": true, "UK": true, "YMCA": true, "YWCA": true,
	"DAR": true, "VFW": true, "NASA": true, "AMVE": true, "AME": true, "CCC": true,
}

// capitalise upper-cases the first letter and lowers the rest, keeping any
// letter that follows a punctuation mark capital: "O'BRIEN" becomes "O'Brien"
// rather than "O'brien".
func capitalise(word string) string {
	runes := []rune(strings.ToLower(word))
	upperNext := true
	for i, r := range runes {
		switch {
		case upperNext && r >= 'a' && r <= 'z':
			runes[i] = r - 32
			upperNext = false
		case r == '\'' || r == '-' || r == '.' || r == '(' || r == '/':
			upperNext = true
		default:
			upperNext = false
		}
	}
	return string(runes)
}
