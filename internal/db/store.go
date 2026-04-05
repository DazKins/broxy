package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/personal/broxy/internal/domain"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	stmts := []string{
		`create table if not exists admin_users (
			username text primary key,
			password_hash text not null,
			created_at text not null
		);`,
		`create table if not exists api_keys (
			id text primary key,
			name text not null,
			key_prefix text not null,
			key_hash text not null unique,
			content_logging integer not null default 0,
			enabled integer not null default 1,
			created_at text not null,
			last_used_at text
		);`,
		`create table if not exists model_routes (
			id text primary key,
			alias text not null unique,
			bedrock_model_id text not null,
			region text not null,
			enabled integer not null default 1,
			default_temperature real,
			default_max_tokens integer,
			created_at text not null,
			updated_at text not null
		);`,
		`create table if not exists pricing_entries (
			model_id text not null,
			region text not null,
			input_per_m_tokens real not null,
			output_per_m_tokens real not null,
			version text not null,
			updated_at text not null,
			primary key(model_id, region)
		);`,
		`create table if not exists request_logs (
			id text primary key,
			started_at text not null,
			finished_at text not null,
			api_key_id text not null,
			method text not null,
			path text not null,
			model_name text not null,
			bedrock_model_id text not null,
			region text not null,
			status_code integer not null,
			latency_ms integer not null,
			input_tokens integer not null default 0,
			output_tokens integer not null default 0,
			total_tokens integer not null default 0,
			estimated_cost_usd real not null default 0,
			content_logged integer not null default 0,
			request_json text,
			response_text text,
			error_text text,
			upstream_request_id text,
			stream integer not null default 0
		);`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("run migration: %w", err)
		}
	}
	return nil
}

func (s *Store) UpsertAdminUser(ctx context.Context, user domain.AdminUser) error {
	_, err := s.db.ExecContext(ctx, `
		insert into admin_users(username, password_hash, created_at)
		values (?, ?, ?)
		on conflict(username) do update set password_hash = excluded.password_hash
	`, user.Username, user.PasswordHash, user.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert admin user: %w", err)
	}
	return nil
}

func (s *Store) GetAdminUser(ctx context.Context, username string) (*domain.AdminUser, error) {
	row := s.db.QueryRowContext(ctx, `select username, password_hash, created_at from admin_users where username = ?`, username)
	var user domain.AdminUser
	var createdAt string
	if err := row.Scan(&user.Username, &user.PasswordHash, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan admin user: %w", err)
	}
	user.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	return &user, nil
}

func (s *Store) CreateAPIKey(ctx context.Context, name, keyPrefix, keyHash string, contentLogging bool) (*domain.APIKey, error) {
	item := &domain.APIKey{
		ID:             uuid.NewString(),
		Name:           name,
		KeyPrefix:      keyPrefix,
		ContentLogging: contentLogging,
		Enabled:        true,
		CreatedAt:      time.Now().UTC(),
	}
	_, err := s.db.ExecContext(ctx, `
		insert into api_keys(id, name, key_prefix, key_hash, content_logging, enabled, created_at)
		values (?, ?, ?, ?, ?, 1, ?)
	`, item.ID, item.Name, item.KeyPrefix, keyHash, boolToInt(contentLogging), item.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("create api key: %w", err)
	}
	return item, nil
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]domain.APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		select id, name, key_prefix, content_logging, enabled, created_at, coalesce(last_used_at, '')
		from api_keys
		order by created_at desc
	`)
	if err != nil {
		return nil, fmt.Errorf("query api keys: %w", err)
	}
	defer rows.Close()
	var items []domain.APIKey
	for rows.Next() {
		var item domain.APIKey
		var createdAt, lastUsedAt string
		var contentLogging, enabled int
		if err := rows.Scan(&item.ID, &item.Name, &item.KeyPrefix, &contentLogging, &enabled, &createdAt, &lastUsedAt); err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}
		item.ContentLogging = contentLogging == 1
		item.Enabled = enabled == 1
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		if lastUsedAt != "" {
			item.LastUsedAt, _ = time.Parse(time.RFC3339Nano, lastUsedAt)
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Store) AuthenticateAPIKey(ctx context.Context, keyHash string) (*domain.APIKey, error) {
	row := s.db.QueryRowContext(ctx, `
		select id, name, key_prefix, content_logging, enabled, created_at, coalesce(last_used_at, '')
		from api_keys
		where key_hash = ?
	`, keyHash)
	var item domain.APIKey
	var createdAt, lastUsedAt string
	var contentLogging, enabled int
	if err := row.Scan(&item.ID, &item.Name, &item.KeyPrefix, &contentLogging, &enabled, &createdAt, &lastUsedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan auth api key: %w", err)
	}
	item.ContentLogging = contentLogging == 1
	item.Enabled = enabled == 1
	item.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	if lastUsedAt != "" {
		item.LastUsedAt, _ = time.Parse(time.RFC3339Nano, lastUsedAt)
	}
	if _, err := s.db.ExecContext(ctx, `update api_keys set last_used_at = ? where id = ?`, time.Now().UTC().Format(time.RFC3339Nano), item.ID); err != nil {
		return nil, fmt.Errorf("update api key last_used_at: %w", err)
	}
	return &item, nil
}

func (s *Store) DisableAPIKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `update api_keys set enabled = 0 where id = ?`, id)
	if err != nil {
		return fmt.Errorf("disable api key: %w", err)
	}
	return nil
}

func (s *Store) UpsertModelRoute(ctx context.Context, route domain.ModelRoute) (*domain.ModelRoute, error) {
	now := time.Now().UTC()
	if route.ID == "" {
		route.ID = uuid.NewString()
	}
	if route.CreatedAt.IsZero() {
		route.CreatedAt = now
	}
	route.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
		insert into model_routes(id, alias, bedrock_model_id, region, enabled, default_temperature, default_max_tokens, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(alias) do update set
			bedrock_model_id = excluded.bedrock_model_id,
			region = excluded.region,
			enabled = excluded.enabled,
			default_temperature = excluded.default_temperature,
			default_max_tokens = excluded.default_max_tokens,
			updated_at = excluded.updated_at
	`, route.ID, route.Alias, route.BedrockModelID, route.Region, boolToInt(route.Enabled), nullableFloat(route.DefaultTemperature), nullableInt(route.DefaultMaxTokens), route.CreatedAt.Format(time.RFC3339Nano), route.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("upsert model route: %w", err)
	}
	return s.GetModelRoute(ctx, route.Alias)
}

func (s *Store) GetModelRoute(ctx context.Context, alias string) (*domain.ModelRoute, error) {
	row := s.db.QueryRowContext(ctx, `
		select id, alias, bedrock_model_id, region, enabled, default_temperature, default_max_tokens, created_at, updated_at
		from model_routes
		where alias = ?
	`, alias)
	return scanModelRoute(row)
}

func (s *Store) ListModelRoutes(ctx context.Context) ([]domain.ModelRoute, error) {
	rows, err := s.db.QueryContext(ctx, `
		select id, alias, bedrock_model_id, region, enabled, default_temperature, default_max_tokens, created_at, updated_at
		from model_routes
		order by alias asc
	`)
	if err != nil {
		return nil, fmt.Errorf("query model routes: %w", err)
	}
	defer rows.Close()
	var items []domain.ModelRoute
	for rows.Next() {
		item, err := scanModelRoute(rows)
		if err != nil {
			return nil, err
		}
		if item != nil {
			items = append(items, *item)
		}
	}
	return items, nil
}

func (s *Store) UpsertPricingEntries(ctx context.Context, items []domain.PricingEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pricing tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		insert into pricing_entries(model_id, region, input_per_m_tokens, output_per_m_tokens, version, updated_at)
		values (?, ?, ?, ?, ?, ?)
		on conflict(model_id, region) do update set
			input_per_m_tokens = excluded.input_per_m_tokens,
			output_per_m_tokens = excluded.output_per_m_tokens,
			version = excluded.version,
			updated_at = excluded.updated_at
	`)
	if err != nil {
		return fmt.Errorf("prepare pricing upsert: %w", err)
	}
	defer stmt.Close()
	for _, item := range items {
		if item.UpdatedAt.IsZero() {
			item.UpdatedAt = time.Now().UTC()
		}
		if _, err := stmt.ExecContext(ctx, item.ModelID, item.Region, item.InputPerMTokens, item.OutputPerMTokens, item.Version, item.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("exec pricing upsert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pricing tx: %w", err)
	}
	return nil
}

func (s *Store) GetPricingEntry(ctx context.Context, modelID, region string) (*domain.PricingEntry, error) {
	row := s.db.QueryRowContext(ctx, `
		select model_id, region, input_per_m_tokens, output_per_m_tokens, version, updated_at
		from pricing_entries
		where model_id = ? and region = ?
	`, modelID, region)
	var item domain.PricingEntry
	var updatedAt string
	if err := row.Scan(&item.ModelID, &item.Region, &item.InputPerMTokens, &item.OutputPerMTokens, &item.Version, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan pricing entry: %w", err)
	}
	item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return &item, nil
}

func (s *Store) CreateRequestLog(ctx context.Context, record domain.RequestRecord) error {
	if record.ID == "" {
		record.ID = uuid.NewString()
	}
	_, err := s.db.ExecContext(ctx, `
		insert into request_logs(
			id, started_at, finished_at, api_key_id, method, path, model_name, bedrock_model_id, region,
			status_code, latency_ms, input_tokens, output_tokens, total_tokens, estimated_cost_usd,
			content_logged, request_json, response_text, error_text, upstream_request_id, stream
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.ID, record.StartedAt.UTC().Format(time.RFC3339Nano), record.FinishedAt.UTC().Format(time.RFC3339Nano), record.APIKeyID, record.Method, record.Path, record.ModelName, record.BedrockModelID, record.Region, record.StatusCode, record.LatencyMS, record.InputTokens, record.OutputTokens, record.TotalTokens, record.EstimatedCostUSD, boolToInt(record.ContentLogged), emptyToNil(record.RequestJSON), emptyToNil(record.ResponseText), emptyToNil(record.ErrorText), emptyToNil(record.UpstreamRequestID), boolToInt(record.Stream))
	if err != nil {
		return fmt.Errorf("insert request log: %w", err)
	}
	return nil
}

func (s *Store) ListRequestLogs(ctx context.Context, limit int) ([]domain.RequestRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		select rl.id, rl.started_at, rl.finished_at, rl.api_key_id, ak.name, rl.method, rl.path, rl.model_name, rl.bedrock_model_id,
			rl.region, rl.status_code, rl.latency_ms, rl.input_tokens, rl.output_tokens, rl.total_tokens, rl.estimated_cost_usd,
			rl.content_logged, coalesce(rl.request_json, ''), coalesce(rl.response_text, ''), coalesce(rl.error_text, ''), coalesce(rl.upstream_request_id, ''), rl.stream
		from request_logs rl
		left join api_keys ak on ak.id = rl.api_key_id
		order by rl.started_at desc
		limit ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("query request logs: %w", err)
	}
	defer rows.Close()
	var items []domain.RequestRecord
	for rows.Next() {
		var item domain.RequestRecord
		var startedAt, finishedAt string
		var contentLogged, stream int
		if err := rows.Scan(&item.ID, &startedAt, &finishedAt, &item.APIKeyID, &item.APIKeyName, &item.Method, &item.Path, &item.ModelName, &item.BedrockModelID, &item.Region, &item.StatusCode, &item.LatencyMS, &item.InputTokens, &item.OutputTokens, &item.TotalTokens, &item.EstimatedCostUSD, &contentLogged, &item.RequestJSON, &item.ResponseText, &item.ErrorText, &item.UpstreamRequestID, &stream); err != nil {
			return nil, fmt.Errorf("scan request log: %w", err)
		}
		item.StartedAt, _ = time.Parse(time.RFC3339Nano, startedAt)
		item.FinishedAt, _ = time.Parse(time.RFC3339Nano, finishedAt)
		item.ContentLogged = contentLogged == 1
		item.Stream = stream == 1
		items = append(items, item)
	}
	return items, nil
}

func (s *Store) UsageBreakdown(ctx context.Context) ([]domain.UsageBreakdownRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		select substr(started_at, 1, 10) as bucket_date, model_name, coalesce(ak.name, ''), count(*),
			sum(input_tokens), sum(output_tokens), sum(total_tokens), sum(estimated_cost_usd)
		from request_logs rl
		left join api_keys ak on ak.id = rl.api_key_id
		group by bucket_date, model_name, ak.name
		order by bucket_date desc, model_name asc
	`)
	if err != nil {
		return nil, fmt.Errorf("query usage breakdown: %w", err)
	}
	defer rows.Close()
	var items []domain.UsageBreakdownRow
	for rows.Next() {
		var item domain.UsageBreakdownRow
		if err := rows.Scan(&item.BucketDate, &item.ModelName, &item.APIKeyName, &item.Requests, &item.InputTokens, &item.OutputTokens, &item.TotalTokens, &item.EstimatedCostUSD); err != nil {
			return nil, fmt.Errorf("scan usage breakdown: %w", err)
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Store) DashboardMetrics(ctx context.Context) (*domain.DashboardMetrics, error) {
	row := s.db.QueryRowContext(ctx, `
		select count(*),
			sum(case when status_code between 200 and 299 then 1 else 0 end),
			sum(case when status_code >= 400 then 1 else 0 end),
			coalesce(sum(input_tokens), 0),
			coalesce(sum(output_tokens), 0),
			coalesce(sum(estimated_cost_usd), 0),
			coalesce(avg(latency_ms), 0),
			coalesce(max(started_at), '')
		from request_logs
	`)
	var item domain.DashboardMetrics
	var lastRequestAt string
	if err := row.Scan(&item.TotalRequests, &item.SuccessRequests, &item.ErrorRequests, &item.TotalInputTokens, &item.TotalOutputTokens, &item.TotalCostUSD, &item.AverageLatencyMS, &lastRequestAt); err != nil {
		return nil, fmt.Errorf("scan dashboard metrics: %w", err)
	}
	if lastRequestAt != "" {
		item.LastRequestAt, _ = time.Parse(time.RFC3339Nano, lastRequestAt)
	}
	return &item, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanModelRoute(row scanner) (*domain.ModelRoute, error) {
	var item domain.ModelRoute
	var createdAt, updatedAt string
	var enabled int
	var temp sql.NullFloat64
	var maxTokens sql.NullInt64
	if err := row.Scan(&item.ID, &item.Alias, &item.BedrockModelID, &item.Region, &enabled, &temp, &maxTokens, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan model route: %w", err)
	}
	item.Enabled = enabled == 1
	if temp.Valid {
		v := temp.Float64
		item.DefaultTemperature = &v
	}
	if maxTokens.Valid {
		v := int(maxTokens.Int64)
		item.DefaultMaxTokens = &v
	}
	item.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return &item, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nullableFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func emptyToNil(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}
