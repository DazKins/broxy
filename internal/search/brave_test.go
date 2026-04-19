package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/personal/broxy/internal/config"
)

func TestBraveProviderSearch(t *testing.T) {
	var gotToken string
	var gotQuery string
	var gotCount string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Subscription-Token")
		gotQuery = r.URL.Query().Get("q")
		gotCount = r.URL.Query().Get("count")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"web": map[string]any{
				"results": []map[string]any{
					{
						"title":       "Kept",
						"url":         "https://docs.example.com/page",
						"description": "Main snippet",
					},
					{
						"title":       "Filtered",
						"url":         "https://blocked.example.com/page",
						"description": "Blocked snippet",
					},
				},
			},
		})
	}))
	defer ts.Close()

	provider := NewBraveProvider(config.SearchConfig{
		BraveAPIKey:    "brave-token",
		Endpoint:       ts.URL,
		MaxResults:     5,
		TimeoutSeconds: 1,
	})
	resp, err := provider.Search(context.Background(), Request{
		Query:          "broxy web search",
		AllowedDomains: []string{"example.com"},
		BlockedDomains: []string{"blocked.example.com"},
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if gotToken != "brave-token" {
		t.Fatalf("X-Subscription-Token = %q", gotToken)
	}
	if gotQuery != "broxy web search" {
		t.Fatalf("q = %q", gotQuery)
	}
	if gotCount != "5" {
		t.Fatalf("count = %q", gotCount)
	}
	if len(resp.Results) != 1 || resp.Results[0].Title != "Kept" {
		t.Fatalf("unexpected results = %#v", resp.Results)
	}
}

func TestBraveProviderRequiresAPIKey(t *testing.T) {
	provider := NewBraveProvider(config.SearchConfig{})
	if _, err := provider.Search(context.Background(), Request{Query: "test"}); err == nil {
		t.Fatalf("Search() error = nil, want missing API key error")
	}
}
