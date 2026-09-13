package registers

import (
	"slices"
	"testing"
)

const imlsHeader = "MID,DISCIPL,COMMONNAME,LEGALNAME,ALTNAME,AKADBA,ADCITY,WEBURL,LATITUDE,LONGITUDE\n"

func parseIMLSRows(t *testing.T, rows string) []museumRow {
	t.Helper()

	museums, err := parseIMLS(zipOf(t, map[string]string{"file1.csv": imlsHeader + rows}))
	if err != nil {
		t.Fatal(err)
	}

	out := make([]museumRow, 0, len(museums))
	for _, m := range museums {
		out = append(out, museumRow{m.Name, m.Locality, m.Website, m.Classes, m.AlsoKnownAs, m.Latitude, m.Longitude})
	}
	return out
}

type museumRow struct {
	name, locality, website string
	classes, aka            []string
	lat, lon                float64
}

// TestParseIMLSAdmitsOnlyMuseums: the file covers more than museums, and the
// three disciplines left out are the ones that sit next to a museum without
// being one — the same line the OSM query draws at arts centres.
func TestParseIMLSAdmitsOnlyMuseums(t *testing.T) {
	got := parseIMLSRows(t, ""+
		"1,ART,ART MUSEUM,,,,MOBILE,,30.7,-88.1\n"+
		"2,GMU,GENERAL MUSEUM,,,,MOBILE,,30.7,-88.2\n"+
		"3,HST,HISTORY MUSEUM,,,,MOBILE,,30.7,-88.3\n"+
		"4,NAT,NATURE MUSEUM,,,,MOBILE,,30.7,-88.4\n"+
		"5,SCI,SCIENCE MUSEUM,,,,MOBILE,,30.7,-88.5\n"+
		"6,CMU,CHILDRENS MUSEUM,,,,MOBILE,,30.7,-88.6\n"+
		"7,HSC,COUNTY HISTORICAL SOCIETY,,,,MOBILE,,30.7,-88.7\n"+
		"8,BOT,BOTANICAL GARDEN,,,,MOBILE,,30.7,-88.8\n"+
		"9,ZAW,CITY ZOO,,,,MOBILE,,30.7,-88.9\n")

	if len(got) != 6 {
		t.Fatalf("got %d museums, want 6: %+v", len(got), got)
	}
	for _, m := range got {
		for _, excluded := range []string{"Historical Society", "Botanical Garden", "City Zoo"} {
			if m.name == excluded {
				t.Errorf("%q was admitted", m.name)
			}
		}
		if len(m.classes) != 1 {
			t.Errorf("%q carries classes %v, want one", m.name, m.classes)
		}
	}
}

// TestParseIMLSReadsBothDisciplineColumns: the column is DISCIPL in one of the
// three files and DISCIPLINE in the other two. Reading only one name drops two
// thirds of the file and nothing says so.
func TestParseIMLSReadsBothDisciplineColumns(t *testing.T) {
	museums, err := parseIMLS(zipOf(t, map[string]string{
		"file1.csv": imlsHeader + "1,ART,FIRST MUSEUM,,,,MOBILE,,30.7,-88.1\n",
		"file2.csv": "MID,DISCIPLINE,COMMONNAME,ADCITY,LATITUDE,LONGITUDE\n" +
			"2,HST,SECOND MUSEUM,MOBILE,30.8,-88.2\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(museums) != 2 {
		t.Fatalf("got %d museums, want both files read: %+v", len(museums), museums)
	}
}

// TestParseIMLSDropsAnUnrelatedLegalName is the one that matters most. The
// legal name is matched from IRS filings and is sometimes a different museum —
// the real row for the History Museum of Mobile carries "Mobile Medical Museum
// Inc" — and the merger matches on every name a source supplies, so a wrong
// alias silently folds two museums into one record.
func TestParseIMLSDropsAnUnrelatedLegalName(t *testing.T) {
	got := parseIMLSRows(t, ""+
		"1,HST,HISTORY MUSEUM OF MOBILE,MOBILE MEDICAL MUSEUM INC,,,MOBILE,,30.7,-88.1\n"+
		"2,ART,MOBILE MUSEUM OF ART,THE MOBILE MUSEUM OF ART INC,,,MOBILE,,30.8,-88.2\n")

	if len(got) != 2 {
		t.Fatalf("got %d museums, want 2", len(got))
	}
	if len(got[0].aka) != 0 {
		t.Errorf("kept an unrelated legal name as an alias: %v", got[0].aka)
	}
	if !slices.Contains(got[1].aka, "The Mobile Museum of Art Inc") {
		t.Errorf("dropped a legal name that is the same museum: %v", got[1].aka)
	}
}

func TestParseIMLSNormalisesNamesAndURLs(t *testing.T) {
	got := parseIMLSRows(t,
		"1,ART,MOBILE MUSEUM OF ART,,,,MOBILE,HTTP://WWW.MOBILEMUSEUMOFART.COM/,30.7,-88.1\n")

	if len(got) != 1 {
		t.Fatalf("got %d museums, want 1", len(got))
	}
	if got[0].name != "Mobile Museum of Art" {
		t.Errorf("name = %q", got[0].name)
	}
	if got[0].locality != "Mobile" {
		t.Errorf("locality = %q", got[0].locality)
	}
	if got[0].website != "http://www.mobilemuseumofart.com/" {
		t.Errorf("website = %q", got[0].website)
	}
}

// TestParseIMLSRejectsNullIsland: a row with no usable coordinates must not be
// stored at 0,0, which is in the Atlantic.
func TestParseIMLSRejectsNullIsland(t *testing.T) {
	got := parseIMLSRows(t, ""+
		"1,ART,NO COORDINATES MUSEUM,,,,MOBILE,,,\n"+
		"2,ART,ZERO COORDINATES MUSEUM,,,,MOBILE,,0,0\n"+
		"3,ART,OUT OF RANGE MUSEUM,,,,MOBILE,,300,-88.1\n")

	if len(got) != 3 {
		t.Fatalf("got %d museums, want all 3 kept: %+v", len(got), got)
	}
	for _, m := range got {
		if m.lat != 0 || m.lon != 0 {
			t.Errorf("%q was placed at %f,%f", m.name, m.lat, m.lon)
		}
	}
}

// TestParseIMLSDecodesLatin1: the file is Latin-1, and reading its bytes as
// UTF-8 produces text Postgres will not store at all.
func TestParseIMLSDecodesLatin1(t *testing.T) {
	// "MUSÉE" with É as the single Latin-1 byte 0xC9.
	latin1 := "1,ART,MUS\xc9E DU TEST,,,,MOBILE,,30.7,-88.1\n"

	got := parseIMLSRows(t, latin1)
	if len(got) != 1 {
		t.Fatalf("got %d museums, want 1", len(got))
	}
	if got[0].name != "Musée du Test" {
		t.Errorf("name = %q, want the accent decoded", got[0].name)
	}
}

// TestParseIMLSCollapsesDuplicates: an institution occasionally appears in two
// of the three discipline files.
func TestParseIMLSCollapsesDuplicates(t *testing.T) {
	museums, err := parseIMLS(zipOf(t, map[string]string{
		"file1.csv": imlsHeader + "1,ART,MOBILE MUSEUM OF ART,,,,MOBILE,,30.7,-88.1\n",
		"file2.csv": imlsHeader + "2,GMU,MOBILE MUSEUM OF ART,,,,MOBILE,,30.7,-88.1\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(museums) != 1 {
		t.Errorf("got %d museums, want the duplicate collapsed: %+v", len(museums), museums)
	}
}
