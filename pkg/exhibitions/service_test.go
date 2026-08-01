package exhibitions

import (
	"context"
	"fmt"
	"testing"

	"museum/internal/models"
)

// Stream reports every site it reads, including the ones that had nothing.
//
// Most sites have nothing, so a callback that fired only when something turned
// up could not be counted: the map's progress reported four sites read out of
// forty and then jumped straight to finished.
func TestStreamReportsEverySiteIncludingEmptyOnes(t *testing.T) {
	const sites = 5

	museums := make([]models.Museum, sites)
	for i := range museums {
		// Unresolvable on purpose: no site has anything to find, which is the
		// case that used to go unreported.
		museums[i] = models.Museum{
			Name:    fmt.Sprintf("Museum %d", i),
			Website: fmt.Sprintf("https://museum-%d.invalid/", i),
		}
	}

	var calls int
	NewScraper().Stream(context.Background(), museums, 4, func([]Exhibition) { calls++ })

	if calls != sites {
		t.Errorf("fn called %d times for %d sites; counting the calls has to count the sites read",
			calls, sites)
	}
}

// One page can name the same entry twice — once as a heading and once as a
// photograph with no text, whose title is read off the slug. They are one
// exhibition, and the museum's own spelling is the one to keep.
func TestDedupeKeepsTheBetterTitleForOneURL(t *testing.T) {
	const url = "https://www.gnm.se/utstallningar/permanenta-utstallningar/daggdjurssalen/"

	got := dedupe([]Exhibition{
		{Title: "Daggdjurssalen", URL: url},
		{Title: "Däggdjurssalen", URL: url},
	})

	if len(got) != 1 {
		t.Fatalf("got %d entries for one URL, want 1: %+v", len(got), got)
	}
	if got[0].Title != "Däggdjurssalen" {
		t.Errorf("Title = %q, want the museum's own spelling rather than the slug", got[0].Title)
	}
}
