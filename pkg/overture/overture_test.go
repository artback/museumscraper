package overture

import (
	"math"
	"slices"
	"testing"

	"museum/internal/models"
)

func TestToMuseum(t *testing.T) {
	p := newPlace("Musée de Tahiti et des Îles", "history_museum", 0.87, -17.71, -149.58,
		[]string{"http://www.museetahiti.pf/"}, "PF", "Punaauia")

	museum, ok := toMuseum(p, map[string]int{})
	if !ok {
		t.Fatal("a history museum was rejected")
	}
	if museum.Name != "Musée de Tahiti et des Îles" {
		t.Errorf("Name = %q", museum.Name)
	}
	// The ISO code has to become the spelling the rest of the catalogue uses,
	// or the merger cannot match the record against anything.
	if museum.Country != "French Polynesia" {
		t.Errorf("Country = %q, want the code resolved to a name", museum.Country)
	}
	if museum.Locality != "Punaauia" || museum.Website != "http://www.museetahiti.pf/" {
		t.Errorf("unexpected record: %+v", museum)
	}
	// bbox is float32 in the file, so the position carries about seven digits —
	// a tenth of a metre, which is finer than the museum is.
	if math.Abs(museum.Latitude-(-17.71)) > 1e-4 || math.Abs(museum.Longitude-(-149.58)) > 1e-4 {
		t.Errorf("coordinates = %f,%f", museum.Latitude, museum.Longitude)
	}
	if !slices.Equal(museum.Classes, []string{"history museum"}) {
		t.Errorf("Classes = %v", museum.Classes)
	}
	if !slices.Equal(museum.Sources, []string{SourceName}) {
		t.Errorf("Sources = %v", museum.Sources)
	}
}

// TestToMuseumRejectsNonMuseums: Overture's places theme is every kind of point
// of interest there is, so the category filter is the whole of what makes this
// a museum source.
func TestToMuseumRejectsNonMuseums(t *testing.T) {
	for _, category := range []string{"restaurant", "hotel", "shopping", "bar", ""} {
		if _, ok := toMuseum(newPlace("Somewhere", category, 0.9, 1, 1, nil, "FR", "Paris"), map[string]int{}); ok {
			t.Errorf("category %q was admitted", category)
		}
	}
}

// TestUnknownMuseumCategoriesAreCounted: a category added upstream must become
// visible rather than silently dropping a kind of museum from the catalogue.
func TestUnknownMuseumCategoriesAreCounted(t *testing.T) {
	unknown := map[string]int{}

	if _, ok := toMuseum(newPlace("Future Museum", "hologram_museum", 0.9, 1, 1, nil, "FR", "Paris"), unknown); ok {
		t.Error("an unrecognised category was admitted")
	}
	if unknown["hologram_museum"] != 1 {
		t.Errorf("unknown = %v, want the new category counted", unknown)
	}

	// Something with no "museum" in the name is an ordinary place, not a
	// category worth reporting.
	toMuseum(newPlace("Corner Shop", "convenience_store", 0.9, 1, 1, nil, "FR", "Paris"), unknown)
	if len(unknown) != 1 {
		t.Errorf("unknown = %v, want only the museum-like category", unknown)
	}
}

// TestToMuseumDropsLowConfidence: Overture scores places by how much its
// sources agree, and the bottom of that range is mostly single unconfirmed
// contributions — as often a closed museum or a duplicate as a real one.
func TestToMuseumDropsLowConfidence(t *testing.T) {
	if _, ok := toMuseum(newPlace("Doubtful Museum", "museum", 0.1, 1, 1, nil, "FR", "Paris"), map[string]int{}); ok {
		t.Error("a place below the confidence floor was admitted")
	}
	if _, ok := toMuseum(newPlace("Likely Museum", "museum", minConfidence, 1, 1, nil, "FR", "Paris"), map[string]int{}); !ok {
		t.Error("a place exactly at the floor was rejected")
	}
}

func TestToMuseumRequiresAName(t *testing.T) {
	if _, ok := toMuseum(newPlace("   ", "museum", 0.9, 1, 1, nil, "FR", "Paris"), map[string]int{}); ok {
		t.Error("an unnamed place was admitted")
	}
}

// TestToMuseumRejectsNullIsland: a place with no usable position must not be
// placed at 0,0, which is in the Atlantic.
func TestToMuseumRejectsNullIsland(t *testing.T) {
	museum, ok := toMuseum(newPlace("Museum", "museum", 0.9, 0, 0, nil, "FR", "Paris"), map[string]int{})
	if !ok {
		t.Fatal("a place with no coordinates should still be a record")
	}
	if museum.HasCoordinates() {
		t.Errorf("placed at %f,%f", museum.Latitude, museum.Longitude)
	}
}

// TestUnknownCountryCodeIsNotGuessed: a country the catalogue cannot name is
// left unknown rather than being invented, since the merger keys on it.
func TestUnknownCountryCodeIsNotGuessed(t *testing.T) {
	museum, ok := toMuseum(newPlace("Museum", "museum", 0.9, 1, 1, nil, "ZZ", "Nowhere"), map[string]int{})
	if !ok {
		t.Fatal("rejected")
	}
	if museum.Country != "unknown" {
		t.Errorf("Country = %q, want unknown", museum.Country)
	}
}

// TestCountryCodesResolveIncludingTerritories: Overture stores ISO codes and
// the catalogue stores names, and territories are where that mapping is most
// likely to be missing.
func TestCountryCodesResolveIncludingTerritories(t *testing.T) {
	cases := map[string]string{
		"FR": "France", "US": "United States", "KE": "Kenya", "NG": "Nigeria",
		"JP": "Japan", "PF": "French Polynesia", "NU": "Niue", "GL": "Greenland",
	}
	for code, want := range cases {
		if got := countryName(code); got != want {
			t.Errorf("countryName(%q) = %q, want %q", code, got, want)
		}
	}
	if got := countryName(""); got != "" {
		t.Errorf("countryName(\"\") = %q", got)
	}
}

// TestCoalesceMergesNearbyRanges is what keeps a pass over the planet to one
// request per column chunk rather than one per page: a range request costs a
// round trip to us-west-2, so reading a little of what we do not need is
// cheaper than another trip.
func TestCoalesceMergesNearbyRanges(t *testing.T) {
	got := coalesce([]byteRange{
		{off: 5_000_000, length: 1000},
		{off: 100, length: 1000},
		{off: 1200, length: 500}, // adjacent to the one above
	})

	if len(got) != 2 {
		t.Fatalf("got %d ranges, want 2: %+v", len(got), got)
	}
	if got[0].off != 100 || got[0].length != 1600 {
		t.Errorf("first range = %+v, want the adjacent pair merged", got[0])
	}
	if got[1].off != 5_000_000 {
		t.Errorf("second range = %+v, want the distant one kept apart", got[1])
	}
	if coalesce(nil) != nil {
		t.Error("coalesce(nil) should be nil")
	}
}

// TestCoalesceKeepsOverlapExtent: a merged range must cover both, including
// when the second is wholly inside the first.
func TestCoalesceKeepsOverlapExtent(t *testing.T) {
	got := coalesce([]byteRange{{off: 0, length: 10_000}, {off: 100, length: 50}})

	if len(got) != 1 || got[0].off != 0 || got[0].length != 10_000 {
		t.Errorf("got %+v, want one range covering both", got)
	}
}

// newPlace builds a row as it comes out of the parquet projection.
func newPlace(name, category string, confidence float64, lat, lon float32,
	websites []string, country, locality string) place {

	var p place
	p.Names.Primary = name
	p.Categories.Primary = category
	p.Confidence = confidence
	p.Bbox.Ymin, p.Bbox.Xmin = lat, lon
	p.Websites = websites
	p.Addresses = append(p.Addresses, struct {
		Country  string `parquet:"country"`
		Locality string `parquet:"locality"`
	}{Country: country, Locality: locality})
	return p
}

var _ = models.Museum{}

// TestAGalleryWithNoSecondOpinionIsNotAMuseum: a gallery the source says
// nothing else about is the default case and the default is no. The first full
// pass found 140,650 art galleries, more than every other museum category put
// together, and most of them sell things.
func TestAGalleryWithNoSecondOpinionIsNotAMuseum(t *testing.T) {
	if _, ok := toMuseum(newPlace("Galerie Au Chevalet", "art_gallery", 0.9, 1, 1, nil, "PF", "Papeete"), map[string]int{}); ok {
		t.Error("an art gallery was admitted as a museum")
	}
	// An art *museum* is a different thing and stays.
	if _, ok := toMuseum(newPlace("Musée d'Orsay", "art_museum", 0.9, 1, 1, nil, "FR", "Paris"), map[string]int{}); !ok {
		t.Error("an art museum was rejected")
	}
}

// TestTheCategoriesTheFirstFullPassFound: these nine were reported by the
// unrecognised-category counter on the first pass over the whole release —
// 2,922 museums that would otherwise have been dropped in silence. They are
// here so that a later edit cannot quietly lose them again.
func TestTheCategoriesTheFirstFullPassFound(t *testing.T) {
	for _, category := range []string{
		"state_museum", "national_museum", "contemporary_art_museum",
		"decorative_arts_museum", "cartooning_museum", "costume_museum",
		"civilization_museum", "textile_museum", "photography_museum",
	} {
		unknown := map[string]int{}
		if _, ok := toMuseum(newPlace("Some Museum", category, 0.9, 1, 1, nil, "FR", "Paris"), unknown); !ok {
			t.Errorf("%s was rejected", category)
		}
		if len(unknown) != 0 {
			t.Errorf("%s is still counted as unrecognised", category)
		}
	}
}

// TestEveryCategoryHasAClass: the class is what a record says it is, so a
// category mapped to an empty string would produce a museum that says nothing.
func TestEveryCategoryHasAClass(t *testing.T) {
	for category, class := range museumCategories {
		if class == "" {
			t.Errorf("%s maps to no class", category)
		}
	}
}

// TestGalleriesAreAdmittedOnTheSourcesOwnSecondOpinion: the art_gallery
// category holds a room that puts on exhibitions and a shop that sells
// paintings, and at 140,650 records neither taking all of it nor dropping all
// of it is right. Overture's own alternate categories separate them.
func TestGalleriesAreAdmittedOnTheSourcesOwnSecondOpinion(t *testing.T) {
	exhibiting := newPlace("Vidéochroniques", "art_gallery", 0.98, 43.3, 5.4, nil, "FR", "Marseille")
	exhibiting.Categories.Alternate = []string{"contemporary_art_museum", "art_museum"}

	museum, ok := toMuseum(exhibiting, map[string]int{})
	if !ok {
		t.Fatal("a gallery the source also calls a contemporary art museum was rejected")
	}
	if !slices.Equal(museum.Classes, []string{"art gallery"}) {
		t.Errorf("Classes = %v, want it recorded as a gallery", museum.Classes)
	}

	for _, alternates := range [][]string{
		{"home_goods_store", "arts_and_entertainment"}, // Wooden Gallery
		{"flowers_and_gifts_shop"},                     // UndARTground Concept Store
		{"bed_and_breakfast", "pop_up_shop"},           // whose website is an Airbnb listing
		{"tea_room", "cafe"},
		{"arts_and_entertainment"}, // the honest near miss: a real gallery, turned away
		{},
	} {
		shop := newPlace("Some Gallery", "art_gallery", 0.98, 43.3, 5.4, nil, "FR", "Marseille")
		shop.Categories.Alternate = alternates
		if _, ok := toMuseum(shop, map[string]int{}); ok {
			t.Errorf("a gallery with alternates %v was admitted", alternates)
		}
	}
}

// TestAdmittedGalleriesStillFaceEveryOtherCheck: the gallery path must not be
// a way around the name and confidence rules.
func TestAdmittedGalleriesStillFaceEveryOtherCheck(t *testing.T) {
	unnamed := newPlace("  ", "art_gallery", 0.98, 43.3, 5.4, nil, "FR", "Marseille")
	unnamed.Categories.Alternate = []string{"art_museum"}
	if _, ok := toMuseum(unnamed, map[string]int{}); ok {
		t.Error("an unnamed gallery was admitted")
	}

	doubtful := newPlace("Doubtful Gallery", "art_gallery", 0.05, 43.3, 5.4, nil, "FR", "Marseille")
	doubtful.Categories.Alternate = []string{"art_museum"}
	if _, ok := toMuseum(doubtful, map[string]int{}); ok {
		t.Error("a gallery below the confidence floor was admitted")
	}
}

// TestAlternatesDoNotRescueAnythingElse: the second-opinion rule is scoped to
// galleries, where the population is genuinely mixed. Letting any category in
// on an alternate would admit every restaurant that happens to carry one.
func TestAlternatesDoNotRescueAnythingElse(t *testing.T) {
	restaurant := newPlace("Museum Café", "restaurant", 0.98, 43.3, 5.4, nil, "FR", "Marseille")
	restaurant.Categories.Alternate = []string{"museum", "art_museum"}

	if _, ok := toMuseum(restaurant, map[string]int{}); ok {
		t.Error("a restaurant was admitted on an alternate category")
	}
}
