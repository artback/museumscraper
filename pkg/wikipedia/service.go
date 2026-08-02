package wikipedia

import (
	"context"
	"fmt"
)

// CategoryService adds pagination and content extraction on top of the raw
// Client.
type CategoryService struct {
	client *Client
}

// NewCategoryService returns a CategoryService backed by client.
func NewCategoryService(client *Client) *CategoryService {
	return &CategoryService{client: client}
}

// GetAllCategoryMembers walks every continuation page of a category and returns
// all of its members.
func (s *CategoryService) GetAllCategoryMembers(ctx context.Context, title string) ([]CategoryMember, error) {
	var all []CategoryMember
	cmContinue := ""
	for {
		resp, err := s.client.FetchCategoryMembers(ctx, title, cmContinue)
		if err != nil {
			return nil, fmt.Errorf("fetch members of %q: %w", title, err)
		}
		all = append(all, resp.Query.CategoryMembers...)
		cmContinue = resp.Continue.CMContinue
		if cmContinue == "" {
			return all, nil
		}
		if err := ctx.Err(); err != nil {
			return all, err
		}
	}
}

// GetPageContent returns the wikitext of a page.
func (s *CategoryService) GetPageContent(ctx context.Context, title string) (string, error) {
	resp, err := s.client.FetchPageContent(ctx, title)
	if err != nil {
		return "", fmt.Errorf("fetch content of %q: %w", title, err)
	}
	for _, page := range resp.Query.Pages {
		if len(page.Revisions) > 0 {
			return page.Revisions[0].Text(), nil
		}
	}
	return "", fmt.Errorf("no content for %s", title)
}

// ResolveTitles looks up article metadata for any number of titles, batching
// requests to stay within the API's per-query limit. A failing batch does not
// discard the batches that succeeded; the first error is returned alongside
// whatever was resolved.
func (s *CategoryService) ResolveTitles(ctx context.Context, titles []string) (map[string]PageMetadata, error) {
	out := make(map[string]PageMetadata, len(titles))
	var firstErr error

	for start := 0; start < len(titles); start += maxTitlesPerQuery {
		end := min(start+maxTitlesPerQuery, len(titles))

		meta, err := s.client.FetchPageMetadata(ctx, titles[start:end])
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for k, v := range meta {
			out[k] = v
		}
	}
	return out, firstErr
}

// Language returns the Wikipedia edition this service reads.
func (s *CategoryService) Language() string {
	if s.client == nil {
		return DefaultLanguage
	}
	return s.client.Language()
}
