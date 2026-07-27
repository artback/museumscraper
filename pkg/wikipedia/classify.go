package wikipedia

import (
	"regexp"
	"strings"
)

// museumKeywords are substrings that positively identify a museum, checked
// against both the article title and its Wikidata short description. The list
// covers the languages that show up in English Wikipedia's museum lists, since
// many articles keep their native name ("Musée du cheminot", "Museo Gurevich").
var museumKeywords = []string{
	"museum", "museums", "musée", "musee", "museo", "museu", "muzeum", "muzej",
	"museet", "museo", "musem", "múzeum", "muzeu",
	"gallery", "galleries", "galerie", "galleria", "galería", "galeria", "gallerie",
	"kunsthalle", "kunstmuseum", "pinacoteca", "pinakothek",
	"aquarium", "planetarium", "arboretum", "observatory",
	"collection", "heritage centre", "heritage center",
	"science centre", "science center", "visitor centre", "visitor center",
	"memorial", "historic house", "historic site",
	"treasury", "armoury", "armory", "menagerie", "zoo",
}

// Deliberately absent from museumKeywords: "exhibition" (an event, not an
// institution — it let "Africa Remix", a touring exhibition, through),
// "monument" (a structure) and "archive" (a records repository).

// settlementDescriptors mark an article as a place rather than a museum. They
// are matched against the Wikidata short description, which is remarkably
// consistent for settlements and administrative divisions: "Commune in
// Auvergne-Rhône-Alpes, France", "City in Ashanti Region, Ghana".
var settlementDescriptors = []string{
	"commune in", "commune of", "city in", "city of", "town in", "town of",
	"village in", "village of", "municipality in", "municipality of",
	"human settlement", "settlement in", "capital of", "capital city",
	"department in", "department of", "region of", "region in",
	"province in", "province of", "prefecture", "administrative",
	"county in", "county of", "district in", "district of",
	"state in", "state of", "canton", "arrondissement", "borough",
	"country in", "island country", "sovereign state",
	"topics referred to by the same term",
	"wikimedia", "wikipedia",
}

// otherNonMuseumDescriptors catch article kinds that regularly appear in list
// pages as context links rather than entries.
var otherNonMuseumDescriptors = []string{
	"river in", "mountain in", "lake in", "language spoken",
	"ethnic group", "political party", "football club", "railway station",
	"university in", "association of", "organization of", "organisation of",
	"government agency", "list of", "index of",
	// Categories and lists sit next to museums and describe things that happen
	// in them or are held by them, rather than the institutions themselves.
	"exhibition", "art installation", "database", "encyclopedia",
	"library in", "library of", "public library", "national library",
	"opera house", "theatre in", "theater in", "concert hall",
	"heritage designation", "award given", "trade union", "magazine",
}

// personDescriptors identify articles about people. House-museum lists link the
// person rather than the museum whenever the museum has no article of its own
// ("* [[Hovhannes Tumanyan]]" under a "House-museums" heading). Emitting those
// would record a poet's biography as a museum, complete with the wrong URL and
// no coordinates, so they are dropped.
var personDescriptors = []string{
	"poet", "writer", "novelist", "author", "playwright", "dramatist",
	"painter", "sculptor", "artist", "composer", "musician", "singer",
	"actor", "actress", "film director", "photographer",
	"politician", "revolutionary", "military leader", "military commander",
	"general", "activist", "philosopher", "polymath", "historian",
	"journalist", "physician", "scientist", "mathematician", "architect",
	"monarch", "emperor", "empress", "king of", "queen of", "saint",
	"businessman", "businesswoman", "explorer", "aviator", "athlete",
}

// lifespanRe matches the "(1869–1927)" suffix Wikidata puts on descriptions of
// people, a strong signal on its own.
var lifespanRe = regexp.MustCompile(`\(\s*\d{3,4}\s*[–—-]\s*\d{0,4}\s*\??\s*\)`)

// Rejection explains why a candidate was discarded, for logging.
type Rejection string

// Reasons a resolved candidate is not emitted as a museum.
const (
	RejectNamespace      Rejection = "not an article"
	RejectDisambiguation Rejection = "disambiguation page"
	RejectListPage       Rejection = "index/list page"
	RejectSettlement     Rejection = "describes a place, not a museum"
	RejectPerson         Rejection = "describes a person, not a museum"
	RejectOtherTopic     Rejection = "describes an unrelated topic"
	RejectUnresolvable   Rejection = "no article and no usable name"
)

// Decision is the outcome of classifying a candidate.
type Decision int

const (
	// Accept means the candidate has an English Wikipedia article describing a
	// museum, with a URL and usually coordinates.
	Accept Decision = iota
	// AcceptUnverified means the list page names the museum but no English
	// article exists for it — a red link. These are real museums (three
	// quarters of the French list is in this state, because the articles only
	// exist on the French Wikipedia), so they are kept with whatever the list
	// page supplied and left for the enrichment stage to locate. They carry no
	// Wikipedia URL or coordinates.
	AcceptUnverified
	// Reject means the candidate is not a museum.
	Reject
)

// Classify decides whether a resolved candidate should be emitted as a museum.
//
// The rules are ordered so that recall stays high: a positive museum keyword in
// the title or description always wins, because many genuine museums have a
// description that would otherwise look like a place ("Museum in Grenoble,
// France"). Only when there is no positive signal do the negative descriptors
// apply. Candidates with no signal either way are accepted, since plenty of
// real museums carry no short description at all ("Palacio Taranco",
// "Musée du cheminot") and dropping them would lose museums.
func Classify(meta PageMetadata) (Decision, Rejection) {
	switch {
	case meta.Namespace != 0:
		return Reject, RejectNamespace
	case meta.IsDisambiguation:
		return Reject, RejectDisambiguation
	case isListTitle(meta.Title):
		return Reject, RejectListPage
	}

	if meta.Missing {
		if strings.TrimSpace(meta.Title) == "" {
			return Reject, RejectUnresolvable
		}
		// No article means no description to judge by. The structural
		// extraction already placed this in a museum list's entry position, so
		// keep it rather than discard a real museum.
		return AcceptUnverified, ""
	}

	haystack := strings.ToLower(meta.Title + " \x00 " + meta.Description)
	for _, kw := range museumKeywords {
		if strings.Contains(haystack, kw) {
			return Accept, ""
		}
	}

	description := strings.ToLower(meta.Description)
	if description == "" {
		// No description to judge by; the structural extraction already placed
		// this in a museum list's name column, so keep it.
		return Accept, ""
	}
	for _, d := range settlementDescriptors {
		if strings.Contains(description, d) {
			return Reject, RejectSettlement
		}
	}
	if lifespanRe.MatchString(description) {
		return Reject, RejectPerson
	}
	for _, d := range personDescriptors {
		if strings.Contains(description, d) {
			return Reject, RejectPerson
		}
	}
	for _, d := range otherNonMuseumDescriptors {
		if strings.Contains(description, d) {
			return Reject, RejectOtherTopic
		}
	}
	return Accept, ""
}
