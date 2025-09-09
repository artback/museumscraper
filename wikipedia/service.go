package wikipedia

import "fmt"

type CategoryService struct {
	client *WikipediaClient
}

func NewCategoryService(client *WikipediaClient) *CategoryService {
	return &CategoryService{client: client}
}

func (s *CategoryService) GetAllCategoryMembers(title string) ([]CategoryMember, error) {
	var all []CategoryMember
	cmContinue := ""
	for {
		resp, err := s.client.FetchCategoryMembers(title, cmContinue)
		if err != nil {
			return nil, err
		}
		all = append(all, resp.Query.CategoryMembers...)
		cmContinue = resp.Continue.CMContinue
		if cmContinue == "" {
			break
		}
	}
	return all, nil
}

func (s *CategoryService) GetPageContent(title string) (string, error) {
	resp, err := s.client.FetchPageContent(title)
	if err != nil {
		return "", err
	}
	for _, page := range resp.Query.Pages {
		if len(page.Revisions) > 0 {
			return page.Revisions[0].Content, nil
		}
	}
	return "", fmt.Errorf("no content for %s", title)
}
