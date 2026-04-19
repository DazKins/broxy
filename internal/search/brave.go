package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/personal/broxy/internal/config"
)

type BraveProvider struct {
	apiKey     string
	endpoint   string
	maxResults int
	timeout    time.Duration
	country    string
	searchLang string
	client     *http.Client
}

func NewBraveProvider(cfg config.SearchConfig) *BraveProvider {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = defaultBraveEndpoint
	}
	maxResults := cfg.MaxResults
	if maxResults <= 0 {
		maxResults = 5
	}
	timeoutSeconds := cfg.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 10
	}
	return &BraveProvider{
		apiKey:     cfg.BraveAPIKey,
		endpoint:   endpoint,
		maxResults: maxResults,
		timeout:    time.Duration(timeoutSeconds) * time.Second,
		country:    cfg.Country,
		searchLang: cfg.SearchLang,
		client: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		},
	}
}

func (p *BraveProvider) Search(ctx context.Context, req Request) (*Response, error) {
	if strings.TrimSpace(p.apiKey) == "" {
		return nil, fmt.Errorf("brave search is configured but search.brave_api_key is empty")
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, fmt.Errorf("web search query is empty")
	}
	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = p.maxResults
	}
	requestCount := braveRequestCount(maxResults)
	u, err := url.Parse(p.endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse Brave endpoint: %w", err)
	}
	params := u.Query()
	params.Set("q", query)
	params.Set("count", strconv.Itoa(requestCount))
	if strings.TrimSpace(p.country) != "" {
		params.Set("country", strings.TrimSpace(p.country))
	}
	if strings.TrimSpace(p.searchLang) != "" {
		params.Set("search_lang", strings.TrimSpace(p.searchLang))
	}
	u.RawQuery = params.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create Brave request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Subscription-Token", p.apiKey)

	client := p.client
	if client == nil {
		client = &http.Client{Timeout: p.timeout}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call Brave search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("brave search returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var body braveResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode Brave response: %w", err)
	}
	results := make([]Result, 0, len(body.Web.Results))
	for _, item := range body.Web.Results {
		if strings.TrimSpace(item.URL) == "" {
			continue
		}
		results = append(results, Result{
			Title:         item.Title,
			URL:           item.URL,
			Description:   item.Description,
			ExtraSnippets: item.ExtraSnippets,
		})
	}
	results = FilterResults(results, req.AllowedDomains, req.BlockedDomains, maxResults)
	return &Response{Query: query, Results: results}, nil
}

func braveRequestCount(maxResults int) int {
	if maxResults <= 0 {
		maxResults = 5
	}
	if maxResults > 20 {
		maxResults = 20
	}
	return maxResults
}

type braveResponse struct {
	Web struct {
		Results []struct {
			Title         string   `json:"title"`
			URL           string   `json:"url"`
			Description   string   `json:"description"`
			ExtraSnippets []string `json:"extra_snippets"`
		} `json:"results"`
	} `json:"web"`
}
