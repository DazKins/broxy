package search

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/personal/broxy/internal/config"
)

const (
	ProviderBrave = "brave"

	defaultBraveEndpoint = "https://api.search.brave.com/res/v1/web/search"
)

type Provider interface {
	Search(ctx context.Context, req Request) (*Response, error)
}

type Request struct {
	Query          string
	MaxResults     int
	AllowedDomains []string
	BlockedDomains []string
}

type Response struct {
	Query   string   `json:"query"`
	Results []Result `json:"results"`
}

type Result struct {
	Title         string   `json:"title"`
	URL           string   `json:"url"`
	Description   string   `json:"description,omitempty"`
	ExtraSnippets []string `json:"extra_snippets,omitempty"`
}

func NewProvider(cfg config.SearchConfig) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "":
		return nil, nil
	case ProviderBrave:
		if strings.TrimSpace(cfg.BraveAPIKey) == "" {
			return nil, fmt.Errorf("search.brave_api_key is required when search.provider is %q", ProviderBrave)
		}
		return NewBraveProvider(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported search provider %q", cfg.Provider)
	}
}

func FormatResponse(resp *Response) string {
	if resp == nil || len(resp.Results) == 0 {
		return "No web search results found."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Web search results for %q:\n", resp.Query)
	for i, result := range resp.Results {
		fmt.Fprintf(&b, "\n%d. %s\nURL: %s", i+1, strings.TrimSpace(result.Title), strings.TrimSpace(result.URL))
		if strings.TrimSpace(result.Description) != "" {
			fmt.Fprintf(&b, "\nSnippet: %s", strings.TrimSpace(result.Description))
		}
		for _, snippet := range result.ExtraSnippets {
			if strings.TrimSpace(snippet) != "" {
				fmt.Fprintf(&b, "\nAdditional snippet: %s", strings.TrimSpace(snippet))
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func FilterResults(results []Result, allowedDomains []string, blockedDomains []string, limit int) []Result {
	filtered := make([]Result, 0, len(results))
	for _, result := range results {
		if !domainAllowed(result.URL, allowedDomains, blockedDomains) {
			continue
		}
		filtered = append(filtered, result)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	return filtered
}

func domainAllowed(rawURL string, allowedDomains []string, blockedDomains []string) bool {
	if len(allowedDomains) > 0 && !matchesAnyDomain(rawURL, allowedDomains) {
		return false
	}
	if len(blockedDomains) > 0 && matchesAnyDomain(rawURL, blockedDomains) {
		return false
	}
	return true
}

func matchesAnyDomain(rawURL string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchesDomain(rawURL, pattern) {
			return true
		}
	}
	return false
}

func matchesDomain(rawURL string, pattern string) bool {
	target, err := url.Parse(rawURL)
	if err != nil || target.Host == "" {
		return false
	}
	pattern = strings.TrimSpace(strings.ToLower(pattern))
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "://") {
		pattern = "https://" + pattern
	}
	parsedPattern, err := url.Parse(pattern)
	if err != nil || parsedPattern.Host == "" {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(target.Hostname()), "www.")
	patternHost := strings.TrimPrefix(strings.ToLower(parsedPattern.Hostname()), "www.")
	if host != patternHost && !strings.HasSuffix(host, "."+patternHost) {
		return false
	}
	if parsedPattern.Path != "" && parsedPattern.Path != "/" {
		return strings.HasPrefix(target.EscapedPath(), parsedPattern.EscapedPath())
	}
	return true
}
