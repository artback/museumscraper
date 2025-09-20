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

// Revision holds the wikitext content of a page revision.
type Revision struct {
	Content string `json:"*"`
}
