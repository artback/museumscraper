package wikipedia

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type Client struct {
	httpClient *http.Client
	userAgent  string
}

func NewClient() *Client {
	return &Client{
		httpClient: http.DefaultClient,
		userAgent:  "Golang_Spider_Bot/3.0",
	}
}

func (c *Client) FetchCategoryMembers(categoryTitle string, cmContinue string) (*APIResponse, error) {
	categoryTitle = strings.ReplaceAll(categoryTitle, " ", "_")
	apiURL := fmt.Sprintf(
		"https://en.wikipedia.org/w/api.php?action=query&list=categorymembers&cmtitle=%s&format=json&cmlimit=500",
		categoryTitle,
	)
	if cmContinue != "" {
		apiURL += "&cmcontinue=" + cmContinue
	}

	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}
	return &apiResp, nil
}

func (c *Client) FetchPageContent(pageTitle string) (*PageAPIResponse, error) {
	pageTitle = strings.ReplaceAll(pageTitle, " ", "_")
	apiURL := fmt.Sprintf("https://en.wikipedia.org/w/api.php?action=query&prop=revisions&rvprop=content&titles=%s&format=json", pageTitle)

	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var apiResp PageAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}
	return &apiResp, nil
}
