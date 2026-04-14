package pricing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/personal/broxy/internal/domain"
)

func TestEnsureFileCreatesEmptyCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.json")
	if err := EnsureFile(path); err != nil {
		t.Fatalf("EnsureFile() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "[]\n" {
		t.Fatalf("pricing file = %q, want empty catalog", content)
	}
}

func TestEnsureEntryAddsZeroValuedRouteEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.json")
	entry, err := EnsureEntry(path, "model-a", "us-east-1")
	if err != nil {
		t.Fatalf("EnsureEntry() error = %v", err)
	}
	if entry.ModelID != "model-a" || entry.Region != "us-east-1" {
		t.Fatalf("entry = %#v", entry)
	}
	if entry.InputPerMTokens != 0 || entry.OutputPerMTokens != 0 {
		t.Fatalf("entry should start with zero pricing: %#v", entry)
	}
	if entry.Version != RouteDefaultVersion {
		t.Fatalf("version = %q, want %q", entry.Version, RouteDefaultVersion)
	}

	rows, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
}

func TestEnsureEntryPreservesExistingPricing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.json")
	rows := []domain.PricingEntry{{
		ModelID:          "model-a",
		Region:           "us-east-1",
		InputPerMTokens:  1.25,
		OutputPerMTokens: 6.25,
		Version:          "manual",
	}}
	if err := SaveToFile(path, rows); err != nil {
		t.Fatalf("SaveToFile() error = %v", err)
	}
	entry, err := EnsureEntry(path, "model-a", "us-east-1")
	if err != nil {
		t.Fatalf("EnsureEntry() error = %v", err)
	}
	if entry.InputPerMTokens != 1.25 || entry.OutputPerMTokens != 6.25 || entry.Version != "manual" {
		t.Fatalf("existing entry was not preserved: %#v", entry)
	}
}

func TestRemoveEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.json")
	rows := []domain.PricingEntry{
		{ModelID: "model-a", Region: "us-east-1", Version: "manual"},
		{ModelID: "model-b", Region: "us-east-1", Version: "manual"},
	}
	if err := SaveToFile(path, rows); err != nil {
		t.Fatalf("SaveToFile() error = %v", err)
	}
	removed, err := RemoveEntry(path, "model-a", "us-east-1")
	if err != nil {
		t.Fatalf("RemoveEntry() error = %v", err)
	}
	if !removed {
		t.Fatalf("RemoveEntry() removed = false, want true")
	}
	got, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if len(got) != 1 || got[0].ModelID != "model-b" {
		t.Fatalf("remaining rows = %#v", got)
	}
}

func TestEstimateCost(t *testing.T) {
	entry := &domain.PricingEntry{
		InputPerMTokens:  3.0,
		OutputPerMTokens: 15.0,
	}
	usage := domain.TokenUsage{Input: 1000, Output: 2000}
	got := EstimateCost(entry, usage)
	want := 0.033
	if got != want {
		t.Fatalf("EstimateCost() = %f, want %f", got, want)
	}
}
