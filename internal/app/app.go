package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/spf13/cobra"

	"github.com/personal/broxy/internal/awsbedrock"
	cfgpkg "github.com/personal/broxy/internal/config"
	"github.com/personal/broxy/internal/db"
	"github.com/personal/broxy/internal/domain"
	"github.com/personal/broxy/internal/httpapi"
	"github.com/personal/broxy/internal/pricing"
	"github.com/personal/broxy/internal/security"
)

const Version = "0.1.0"

func NewRootCommand() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "broxy",
		Short: "Standalone Bedrock proxy with OpenAI-compatible API, admin UI, and CLI",
	}
	cmd.PersistentFlags().StringVar(&configPath, "config", "", "config file path")
	cmd.AddCommand(
		newInitCommand(&configPath),
		newServeCommand(&configPath),
		newAdminCommand(&configPath),
		newAPIKeyCommand(&configPath),
		newModelsCommand(&configPath),
		newUsageCommand(&configPath),
		newLogsCommand(&configPath),
	)
	return cmd
}

func newInitCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize config, database, pricing catalog, and admin credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := cfgpkg.ConfigPath(*configPath)
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("config already exists at %s", path)
			}
			cfg, err := cfgpkg.Default()
			if err != nil {
				return err
			}
			if *configPath != "" {
				baseDir := filepath.Dir(path)
				cfg.ConfigDir = baseDir
				cfg.DataDir = filepath.Join(baseDir, "data")
				cfg.DBPath = filepath.Join(cfg.DataDir, "broxy.db")
				cfg.PricingPath = filepath.Join(baseDir, "pricing.json")
			}
			secret, err := security.RandomToken("sess_", 48)
			if err != nil {
				return err
			}
			cfg.SessionSecret = secret[:32]
			if err := pricing.EnsureFile(cfg.PricingPath); err != nil {
				return err
			}
			if err := cfgpkg.Save(path, cfg); err != nil {
				return err
			}
			store, err := db.Open(cfg.DBPath)
			if err != nil {
				return err
			}
			defer store.Close()
			password, err := resetAdminPassword(cmd.Context(), store)
			if err != nil {
				return err
			}
			if err := seedPricing(cmd.Context(), store, cfg.PricingPath); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Config: %s\nDB: %s\nPricing: %s\nAdmin username: admin\nAdmin password: %s\n", path, cfg.DBPath, cfg.PricingPath, password)
			return nil
		},
	}
}

func newServeCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the proxy server and embedded admin UI",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, store, provider, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			server := &http.Server{
				Addr:    cfg.ListenAddr,
				Handler: httpapi.New(cfg, store, provider, Version).Router(),
			}
			errCh := make(chan error, 1)
			go func() {
				errCh <- server.ListenAndServe()
			}()
			fmt.Fprintf(cmd.OutOrStdout(), "broxy listening on http://%s\n", cfg.ListenAddr)
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			select {
			case sig := <-sigCh:
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				fmt.Fprintf(cmd.OutOrStdout(), "shutting down on %s\n", sig)
				return server.Shutdown(ctx)
			case err := <-errCh:
				if err == nil || err == http.ErrServerClosed {
					return nil
				}
				return err
			}
		},
	}
}

func newAdminCommand(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Admin account utilities",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "reset-password",
		Short: "Reset the local admin password and print the new value",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			password, err := resetAdminPassword(cmd.Context(), store)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Admin username: admin\nAdmin password: %s\n", password)
			return nil
		},
	})
	return cmd
}

func newAPIKeyCommand(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apikey",
		Short: "Manage proxy API keys",
	}
	var contentLogging bool
	createCmd := &cobra.Command{
		Use:   "create --name <name>",
		Short: "Create a new client API key",
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("name")
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			_, store, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			token, err := security.RandomToken("bpx_", 24)
			if err != nil {
				return err
			}
			item, err := store.CreateAPIKey(cmd.Context(), name, security.KeyPrefix(token), security.HashAPIKey(token), contentLogging)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ID: %s\nName: %s\nKey: %s\n", item.ID, item.Name, token)
			return nil
		},
	}
	createCmd.Flags().String("name", "", "display name for the key")
	createCmd.Flags().BoolVar(&contentLogging, "log-content", false, "store prompts and outputs for this key")
	cmd.AddCommand(createCmd)
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List API keys",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			items, err := store.ListAPIKeys(cmd.Context())
			if err != nil {
				return err
			}
			for _, item := range items {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\tcontent_logging=%t\tenabled=%t\tlast_used=%s\n", item.ID, item.Name, item.ContentLogging, item.Enabled, item.LastUsedAt.Format(time.RFC3339))
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <id>",
		Short: "Disable an API key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			return store.DisableAPIKey(cmd.Context(), args[0])
		},
	})
	return cmd
}

func newModelsCommand(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Manage model routing",
	}
	var temp float64
	var tempSet bool
	var maxTokens int
	var maxSet bool
	addCmd := &cobra.Command{
		Use:   "add --alias <alias> --model-id <id> --region <region>",
		Short: "Add or update a model alias",
		RunE: func(cmd *cobra.Command, args []string) error {
			alias, _ := cmd.Flags().GetString("alias")
			modelID, _ := cmd.Flags().GetString("model-id")
			region, _ := cmd.Flags().GetString("region")
			if alias == "" || modelID == "" || region == "" {
				return fmt.Errorf("--alias, --model-id, and --region are required")
			}
			_, store, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			var tempPtr *float64
			if tempSet {
				tempPtr = &temp
			}
			var maxPtr *int
			if maxSet {
				maxPtr = &maxTokens
			}
			item, err := store.UpsertModelRoute(cmd.Context(), domain.ModelRoute{
				Alias:              alias,
				BedrockModelID:     modelID,
				Region:             region,
				Enabled:            true,
				DefaultTemperature: tempPtr,
				DefaultMaxTokens:   maxPtr,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Upserted %s -> %s (%s)\n", item.Alias, item.BedrockModelID, item.Region)
			return nil
		},
	}
	addCmd.Flags().String("alias", "", "proxy alias")
	addCmd.Flags().String("model-id", "", "bedrock model ID")
	addCmd.Flags().String("region", "", "AWS region")
	addCmd.Flags().Float64Var(&temp, "temperature", 0, "default temperature")
	addCmd.Flags().IntVar(&maxTokens, "max-tokens", 0, "default max tokens")
	addCmd.Flags().BoolVar(&tempSet, "set-temperature", false, "apply --temperature")
	addCmd.Flags().BoolVar(&maxSet, "set-max-tokens", false, "apply --max-tokens")
	cmd.AddCommand(addCmd)
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List model routes",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			items, err := store.ListModelRoutes(cmd.Context())
			if err != nil {
				return err
			}
			for _, item := range items {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\tenabled=%t\n", item.Alias, item.BedrockModelID, item.Region, item.Enabled)
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "sync",
		Short: "Discover Bedrock foundation models and upsert them as aliases",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, store, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			if cfg.Upstream.Mode != cfgpkg.UpstreamAuthAWS {
				return fmt.Errorf("models sync requires AWS credential mode")
			}
			loadOptions := []func(*config.LoadOptions) error{
				config.WithRegion(cfg.Upstream.Region),
			}
			if cfg.Upstream.Profile != "" {
				loadOptions = append(loadOptions, config.WithSharedConfigProfile(cfg.Upstream.Profile))
			}
			awsCfg, err := config.LoadDefaultConfig(cmd.Context(), loadOptions...)
			if err != nil {
				return err
			}
			client := bedrock.NewFromConfig(awsCfg)
			out, err := client.ListFoundationModels(cmd.Context(), &bedrock.ListFoundationModelsInput{})
			if err != nil {
				return err
			}
			for _, summary := range out.ModelSummaries {
				modelID := awsString(summary.ModelId)
				if modelID == "" {
					continue
				}
				_, err := store.UpsertModelRoute(cmd.Context(), domain.ModelRoute{
					Alias:          modelID,
					BedrockModelID: modelID,
					Region:         cfg.Upstream.Region,
					Enabled:        true,
				})
				if err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Synced %d models into routes\n", len(out.ModelSummaries))
			return nil
		},
	})
	return cmd
}

func newUsageCommand(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Usage reporting",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "report",
		Short: "Print token and cost breakdown grouped by day/model/key",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			rows, err := store.UsageBreakdown(cmd.Context())
			if err != nil {
				return err
			}
			for _, row := range rows {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\trequests=%d\ttokens=%d\tcost=$%.6f\n", row.BucketDate, row.ModelName, row.APIKeyName, row.Requests, row.TotalTokens, row.EstimatedCostUSD)
			}
			return nil
		},
	})
	return cmd
}

func newLogsCommand(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Inspect request logs",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "tail",
		Short: "Print recent request logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			rows, err := store.ListRequestLogs(cmd.Context(), 20)
			if err != nil {
				return err
			}
			for _, row := range rows {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\tstatus=%d\tmodel=%s\tin=%d\tout=%d\tcost=$%.6f\tlatency=%dms\n", row.StartedAt.Format(time.RFC3339), row.StatusCode, row.ModelName, row.InputTokens, row.OutputTokens, row.EstimatedCostUSD, row.LatencyMS)
			}
			return nil
		},
	})
	return cmd
}

func bootstrap(ctx context.Context, configPath string) (*cfgpkg.Config, *db.Store, *awsbedrock.Client, func(), error) {
	path, err := cfgpkg.ConfigPath(configPath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	cfg, err := cfgpkg.Load(path)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("load config %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		return nil, nil, nil, nil, err
	}
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := seedPricing(ctx, store, cfg.PricingPath); err != nil {
		store.Close()
		return nil, nil, nil, nil, err
	}
	provider, err := awsbedrock.New(ctx, cfg.Upstream)
	if err != nil {
		store.Close()
		return nil, nil, nil, nil, err
	}
	return cfg, store, provider, func() {
		_ = store.Close()
	}, nil
}

func resetAdminPassword(ctx context.Context, store *db.Store) (string, error) {
	password, err := security.RandomToken("bpw_", 18)
	if err != nil {
		return "", err
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return "", err
	}
	if err := store.UpsertAdminUser(ctx, domain.AdminUser{
		Username:     "admin",
		PasswordHash: hash,
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		return "", err
	}
	return password, nil
}

func seedPricing(ctx context.Context, store *db.Store, path string) error {
	if err := pricing.EnsureFile(path); err != nil {
		return err
	}
	rows, err := pricing.LoadFromFile(path)
	if err != nil {
		return err
	}
	return store.UpsertPricingEntries(ctx, rows)
}

func awsString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
