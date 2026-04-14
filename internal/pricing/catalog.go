package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/personal/broxy/internal/domain"
)

//go:embed default_pricing.json
var defaultPricing []byte

const RouteDefaultVersion = "route-default"

func DefaultCatalog() ([]domain.PricingEntry, error) {
	return parse(defaultPricing)
}

func LoadFromFile(path string) ([]domain.PricingEntry, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pricing file: %w", err)
	}
	return parse(content)
}

func EnsureFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir pricing dir: %w", err)
	}
	if err := os.WriteFile(path, defaultPricing, 0o600); err != nil {
		return fmt.Errorf("write pricing file: %w", err)
	}
	return nil
}

func SaveToFile(path string, rows []domain.PricingEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir pricing dir: %w", err)
	}
	content, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pricing catalog: %w", err)
	}
	content = append(content, '\n')
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("write pricing file: %w", err)
	}
	return nil
}

func EnsureEntry(path, modelID, region string) (*domain.PricingEntry, error) {
	if err := EnsureFile(path); err != nil {
		return nil, err
	}
	rows, err := LoadFromFile(path)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ModelID == modelID && rows[i].Region == region {
			return &rows[i], nil
		}
	}
	entry := domain.PricingEntry{
		ModelID:          modelID,
		Region:           region,
		InputPerMTokens:  0,
		OutputPerMTokens: 0,
		Version:          RouteDefaultVersion,
		UpdatedAt:        time.Now().UTC(),
	}
	rows = append(rows, entry)
	if err := SaveToFile(path, rows); err != nil {
		return nil, err
	}
	return &entry, nil
}

func RemoveEntry(path, modelID, region string) (bool, error) {
	if err := EnsureFile(path); err != nil {
		return false, err
	}
	rows, err := LoadFromFile(path)
	if err != nil {
		return false, err
	}
	filtered := rows[:0]
	removed := false
	for _, row := range rows {
		if row.ModelID == modelID && row.Region == region {
			removed = true
			continue
		}
		filtered = append(filtered, row)
	}
	if !removed {
		return false, nil
	}
	if err := SaveToFile(path, filtered); err != nil {
		return false, err
	}
	return true, nil
}

func parse(content []byte) ([]domain.PricingEntry, error) {
	var rows []domain.PricingEntry
	if err := json.Unmarshal(content, &rows); err != nil {
		return nil, fmt.Errorf("parse pricing catalog: %w", err)
	}
	for i := range rows {
		if rows[i].UpdatedAt.IsZero() {
			rows[i].UpdatedAt = time.Now().UTC()
		}
	}
	return rows, nil
}

func EstimateCost(entry *domain.PricingEntry, usage domain.TokenUsage) float64 {
	if entry == nil {
		return 0
	}
	input := (float64(usage.Input) / 1_000_000) * entry.InputPerMTokens
	output := (float64(usage.Output) / 1_000_000) * entry.OutputPerMTokens
	return input + output
}
