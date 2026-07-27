package wikipedia

// APIResponse is the top-level struct for the JSON response, including query results and pagination info.
type APIResponse struct {
	Query    Query    `json:"query"`
	Continue Continue `json:"continue"`
}

// Continue holds the continuation token for the next API request, essential for pagination.
type Continue struct {
	CMContinue string `json:"cmcontinue"`
	Continue   string `json:"continue"`
}

// Query contains the results of the API query, specifically the list of category members.
type Query struct {
	CategoryMembers []CategoryMember `json:"categorymembers"`
}

// CategoryMember represents a single page or subcategory. NS=14 for categories, NS=0 for articles.
type CategoryMember struct {
	PageID int    `json:"pageid"`
	NS     int    `json:"ns"`
	Title  string `json:"title"`
}

// PageAPIResponse is the top-level struct for a page content query.
type PageAPIResponse struct {
	Query PageQuery `json:"query"`
}

// PageQuery contains the pages map from a page content query.
type PageQuery struct {
	Pages map[string]Page `json:"pages"`
}

// Page represents a single page with its revisions.
type Page struct {
	PageID    int        `json:"pageid"`
	Title     string     `json:"title"`
	Revisions []Revision `json:"revisions"`
}

// Revision holds the wikitext of a page revision. The API returns the content
// either directly under "*" or, when slots are requested, nested under the
// main slot; Text handles both.
type Revision struct {
	Content string `json:"*"`
	Slots   struct {
		Main struct {
			Content string `json:"*"`
		} `json:"main"`
	} `json:"slots"`
}

// Text returns the revision's wikitext regardless of which shape the API used.
func (r Revision) Text() string {
	if r.Slots.Main.Content != "" {
		return r.Slots.Main.Content
	}
	return r.Content
}

// PageMetadata is the resolved Wikipedia data for a single article: where it
// lives, where it is on the map, and what it is.
type PageMetadata struct {
	PageID    int
	Title     string
	Namespace int

	// Missing is true when no article exists at the title (a red link).
	Missing bool
	// IsDisambiguation is true for "Mercury (disambiguation)"-style pages.
	IsDisambiguation bool

	// URL is the canonical article URL.
	URL string
	// Description is Wikidata's short description, e.g. "Art museum in Paris,
	// France". Empty for many articles.
	Description string
	// WikidataID is the "Q…" identifier of the linked Wikidata item.
	WikidataID string

	Latitude       float64
	Longitude      float64
	HasCoordinates bool
}

// metadataResponse mirrors a formatversion=2 query response for the
// coordinates/info/pageprops properties.
type metadataResponse struct {
	Query struct {
		Normalized []titleMapping `json:"normalized"`
		Redirects  []titleMapping `json:"redirects"`
		Pages      []pageResult   `json:"pages"`
	} `json:"query"`
}

// titleMapping records a title rewrite the API performed, either normalisation
// or a redirect.
type titleMapping struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// pageResult is one page entry in a metadataResponse.
type pageResult struct {
	PageID      int    `json:"pageid"`
	NS          int    `json:"ns"`
	Title       string `json:"title"`
	Missing     bool   `json:"missing"`
	FullURL     string `json:"fullurl"`
	Coordinates []struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	} `json:"coordinates"`
	PageProps map[string]string `json:"pageprops"`
}
