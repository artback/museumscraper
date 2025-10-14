package wikipedia

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type rewriteRoundTripper struct{ base *url.URL }

func (r rewriteRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// clone the request to avoid mutating the original
	c := new(http.Request)
	*c = *req
	// rewrite scheme and host to point to the test server, keep path and query
	u := *req.URL
	c.URL = &u
	c.URL.Scheme = r.base.Scheme
	c.URL.Host = r.base.Host
	// Ensure Host header matches server for Go <1.20 behavior
	c.Host = r.base.Host
	return http.DefaultTransport.RoundTrip(c)
}

func newTestClient(serverURL string) *Client {
	u, _ := url.Parse(serverURL)
	httpClient := &http.Client{Transport: rewriteRoundTripper{base: u}}
	return &Client{httpClient: httpClient, userAgent: "test-agent"}
}

func TestCategoryService_GetAllCategoryMembers_TableDriven(t *testing.T) {
	// Set up fake Wikipedia API behavior
	mux := http.NewServeMux()
	mux.HandleFunc("/w/api.php", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		list := q.Get("list")
		cmtitle := q.Get("cmtitle")
		cmcontinue := q.Get("cmcontinue")
		if list != "categorymembers" {
			t.Fatalf("unexpected list param: %s", list)
		}
		// pagination behavior based on cmcontinue
		var resp APIResponse
		switch cmcontinue {
		case "":
			resp = APIResponse{
				Query:    Query{CategoryMembers: []CategoryMember{{PageID: 1, NS: 0, Title: cmtitle + " Page A"}}},
				Continue: Continue{CMContinue: "next"},
			}
		case "next":
			resp = APIResponse{
				Query:    Query{CategoryMembers: []CategoryMember{{PageID: 2, NS: 14, Title: cmtitle + " Subcat"}}},
				Continue: Continue{CMContinue: ""},
			}
		default:
			t.Fatalf("unexpected cmcontinue: %s", cmcontinue)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(server.URL)
	svc := NewCategoryService(client)

	tests := []struct {
		name  string
		input string
		want  []CategoryMember
	}{
		{name: "aggregates pagination", input: "Category:Museums in Testland", want: []CategoryMember{{PageID: 1, NS: 0, Title: "Category:Museums_in_Testland Page A"}, {PageID: 2, NS: 14, Title: "Category:Museums_in_Testland Subcat"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := svc.GetAllCategoryMembers(tt.input)
			if err != nil {
				t.Fatalf("GetAllCategoryMembers error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len mismatch: got %d want %d (%v)", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("idx %d: got %+v want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCategoryService_GetPageContent_TableDriven(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/w/api.php", func(w http.ResponseWriter, r *http.Request) {
		// Different responses by page title
		title := r.URL.Query().Get("titles")
		var resp PageAPIResponse
		switch {
		case strings.Contains(title, "WithContent"):
			resp = PageAPIResponse{Query: PageQuery{Pages: map[string]Page{
				"1": {PageID: 1, Title: title, Revisions: []Revision{{Content: "wiki-text-content"}}},
			}}}
		case strings.Contains(title, "NoRevisions"):
			resp = PageAPIResponse{Query: PageQuery{Pages: map[string]Page{
				"2": {PageID: 2, Title: title, Revisions: []Revision{}},
			}}}
		default:
			// unexpected
			t.Fatalf("unexpected title: %s", title)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(server.URL)
	svc := NewCategoryService(client)

	tests := []struct {
		name    string
		title   string
		want    string
		wantErr bool
	}{
		{name: "returns first revision content", title: "Some Page WithContent", want: "wiki-text-content", wantErr: false},
		{name: "errors when no revisions", title: "Another Page NoRevisions", want: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := svc.GetPageContent(tt.title)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error presence mismatch: err=%v wantErr=%v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
			if tt.wantErr && err == nil {
				t.Errorf("expected error but got nil")
			}
		})
	}
}

func TestClient_URLBuilding_SpacesReplaced(t *testing.T) {
	// Ensure spaces are replaced by underscores in API requests
	var gotPath string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RawQuery
		// Send minimal valid response for categorymembers
		if strings.Contains(r.URL.RawQuery, "list=categorymembers") {
			_ = json.NewEncoder(w).Encode(APIResponse{})
			return
		}
		_ = json.NewEncoder(w).Encode(PageAPIResponse{Query: PageQuery{Pages: map[string]Page{"1": {Revisions: []Revision{{Content: "c"}}}}}})
	}
	server := httptest.NewServer(http.HandlerFunc(handler))
	defer server.Close()

	client := newTestClient(server.URL)

	// Category members: should include cmtitle with underscore
	_, _ = client.FetchCategoryMembers("My Category Title", "")
	if !strings.Contains(gotPath, fmt.Sprintf("%s", url.QueryEscape("My_Category_Title"))) {
		t.Errorf("expected underscores in cmtitle, got query: %s", gotPath)
	}
}
