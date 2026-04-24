package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/personal/broxy/internal/domain"
)

func TestPricingEntryCacheRatesRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "broxy.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	entry := domain.PricingEntry{
		ModelID:              "model-a",
		Region:               "us-east-1",
		InputPerMTokens:      3,
		OutputPerMTokens:     15,
		CacheReadPerMTokens:  1,
		CacheWritePerMTokens: 6,
		Version:              "test",
	}
	if err := store.UpsertPricingEntries(context.Background(), []domain.PricingEntry{entry}); err != nil {
		t.Fatalf("UpsertPricingEntries() error = %v", err)
	}

	got, err := store.GetPricingEntry(context.Background(), "model-a", "us-east-1")
	if err != nil {
		t.Fatalf("GetPricingEntry() error = %v", err)
	}
	if got == nil {
		t.Fatalf("GetPricingEntry() = nil")
	}
	if got.CacheReadPerMTokens != 1 || got.CacheWritePerMTokens != 6 {
		t.Fatalf("cache rates = %f/%f, want 1/6", got.CacheReadPerMTokens, got.CacheWritePerMTokens)
	}
}

func TestPricingEntryMigrationAddsCacheRateColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broxy.db")
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := raw.Exec(`
		create table pricing_entries (
			model_id text not null,
			region text not null,
			input_per_m_tokens real not null,
			output_per_m_tokens real not null,
			version text not null,
			updated_at text not null,
			primary key(model_id, region)
		);
	`); err != nil {
		t.Fatalf("create old pricing_entries table: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw Close() error = %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	entry := domain.PricingEntry{
		ModelID:              "model-a",
		Region:               "us-east-1",
		InputPerMTokens:      3,
		OutputPerMTokens:     15,
		CacheReadPerMTokens:  1,
		CacheWritePerMTokens: 6,
		Version:              "test",
	}
	if err := store.UpsertPricingEntries(context.Background(), []domain.PricingEntry{entry}); err != nil {
		t.Fatalf("UpsertPricingEntries() after migration error = %v", err)
	}
}
