package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/personal/broxy/internal/awsbedrock"
	cfgpkg "github.com/personal/broxy/internal/config"
	"github.com/personal/broxy/internal/db"
	"github.com/personal/broxy/internal/domain"
	"github.com/personal/broxy/internal/httpapi"
	"github.com/personal/broxy/internal/logging"
	"github.com/personal/broxy/internal/pricing"
	"github.com/personal/broxy/internal/security"
	"github.com/personal/broxy/internal/service"
)

var Version = "dev"

type initResult struct {
	ConfigPath    string `json:"config_path"`
	ConfigDir     string `json:"config_dir"`
	StateDir      string `json:"state_dir"`
	DBPath        string `json:"db_path"`
	PricingPath   string `json:"pricing_path"`
	LogDir        string `json:"log_dir"`
	AdminUsername string `json:"admin_username"`
	AdminPassword string `json:"admin_password"`
}

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
		newServiceCommand(&configPath),
		newConfigCommand(&configPath),
		newVersionCommand(),
		newAdminCommand(&configPath),
		newAPIKeyCommand(&configPath),
		newModelsCommand(&configPath),
		newUsageCommand(&configPath),
		newLogsCommand(&configPath),
	)
	return cmd
}

func newInitCommand(configPath *string) *cobra.Command {
	var nonInteractive bool
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize config, database, pricing catalog, and admin credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = nonInteractive

			path, err := cfgpkg.ConfigPath(*configPath)
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("config already exists at %s", path)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("stat config %s: %w", path, err)
			}

			cfg, err := cfgpkg.DefaultForPath(path)
			if err != nil {
				return err
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

			result := initResult{
				ConfigPath:    path,
				ConfigDir:     cfg.ConfigDir,
				StateDir:      cfg.StateDir,
				DBPath:        cfg.DBPath,
				PricingPath:   cfg.PricingPath,
				LogDir:        cfg.LogDir(),
				AdminUsername: "admin",
				AdminPassword: password,
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), result)
			}
			fmt.Fprintf(
				cmd.OutOrStdout(),
				"Config: %s\nState: %s\nDB: %s\nPricing: %s\nLogs: %s\nAdmin username: %s\nAdmin password: %s\n",
				result.ConfigPath,
				result.StateDir,
				result.DBPath,
				result.PricingPath,
				result.LogDir,
				result.AdminUsername,
				result.AdminPassword,
			)
			return nil
		},
	}
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "disable interactive prompts")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the result as JSON")
	return cmd
}

func newServeCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the proxy server and embedded admin UI",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, store, provider, logger, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			provider.LogAuth(cmd.Context())
			server := &http.Server{
				Addr:    cfg.ListenAddr,
				Handler: httpapi.NewWithLogger(cfg, store, provider, Version, logger).Router(),
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

func newServiceCommand(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage the background broxy service",
	}

	var dryRun bool
	installCmd := &cobra.Command{
		Use:   "install",
		Short: "Install the native user service definition",
		RunE: func(cmd *cobra.Command, args []string) error {
			def, cfg, err := serviceDefinition(*configPath, !dryRun)
			if err != nil {
				return err
			}
			rendered, err := service.Render(def)
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "Service file: %s\n\n%s", def.ServiceFile, rendered)
				return nil
			}
			if err := cfgpkg.EnsureLayout(cfg); err != nil {
				return err
			}
			if err := service.Install(def); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Installed service definition at %s\n", def.ServiceFile)
			return nil
		},
	}
	installCmd.Flags().BoolVar(&dryRun, "dry-run", false, "render the service file without writing it")

	cmd.AddCommand(installCmd)
	cmd.AddCommand(&cobra.Command{
		Use:   "uninstall",
		Short: "Remove the native user service definition",
		RunE: func(cmd *cobra.Command, args []string) error {
			def, _, err := serviceDefinition(*configPath, false)
			if err != nil {
				return err
			}
			if err := service.Uninstall(def); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed service definition %s\n", def.ServiceFile)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "Start the background service",
		RunE: func(cmd *cobra.Command, args []string) error {
			def, _, err := serviceDefinition(*configPath, true)
			if err != nil {
				return err
			}
			if err := service.Start(def); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Started broxy service")
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the background service",
		RunE: func(cmd *cobra.Command, args []string) error {
			def, _, err := serviceDefinition(*configPath, false)
			if err != nil {
				return err
			}
			if err := service.Stop(def); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Stopped broxy service")
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "restart",
		Short: "Restart the background service",
		RunE: func(cmd *cobra.Command, args []string) error {
			def, _, err := serviceDefinition(*configPath, true)
			if err != nil {
				return err
			}
			if err := service.Restart(def); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Restarted broxy service")
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show background service status",
		RunE: func(cmd *cobra.Command, args []string) error {
			def, cfg, err := serviceDefinition(*configPath, false)
			if err != nil {
				return err
			}
			if _, err := os.Stat(def.ServiceFile); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					fmt.Fprintf(
						cmd.OutOrStdout(),
						"manager=%s\nservice=%s\nstate=not-installed\nconfig=%s\nlisten_addr=%s\nversion=%s\n",
						def.Target,
						def.Label,
						def.ConfigPath,
						cfg.ListenAddr,
						Version,
					)
					return nil
				}
				return err
			}
			status, err := service.GetStatus(def)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			state := "stopped"
			subState := ""
			enabled := "installed"
			pid := ""
			manager := string(def.Target)
			if status != nil {
				manager = status.Manager
				if status.State != "" {
					state = status.State
				}
				subState = status.SubState
				if status.Enabled != "" {
					enabled = status.Enabled
				}
				pid = status.PID
			}
			fmt.Fprintf(
				cmd.OutOrStdout(),
				"manager=%s\nservice=%s\nstate=%s\nsubstate=%s\nenabled=%s\npid=%s\nconfig=%s\nlisten_addr=%s\nversion=%s\n",
				manager,
				def.Label,
				state,
				subState,
				enabled,
				pid,
				def.ConfigPath,
				cfg.ListenAddr,
				Version,
			)
			return nil
		},
	})

	var logLines int
	var logStream string
	logsCmd := &cobra.Command{
		Use:   "logs",
		Short: "Tail service log files",
		RunE: func(cmd *cobra.Command, args []string) error {
			def, _, err := serviceDefinition(*configPath, false)
			if err != nil {
				return err
			}
			body, err := service.TailLogs(def, logStream, logLines)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), body)
			return nil
		},
	}
	logsCmd.Flags().IntVar(&logLines, "lines", service.DefaultLogTail, "number of lines to show")
	logsCmd.Flags().StringVar(&logStream, "stream", "both", "stdout, stderr, or both")
	cmd.AddCommand(logsCmd)

	return cmd
}

func newConfigCommand(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect config paths and defaults",
	}
	var jsonOutput bool
	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Print effective config, state, and log paths",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := cfgpkg.ConfigPath(*configPath)
			if err != nil {
				return err
			}
			cfg, err := loadOrDefaultConfig(path)
			if err != nil {
				return err
			}
			payload := map[string]string{
				"config_path":  path,
				"config_dir":   cfg.ConfigDir,
				"state_dir":    cfg.StateDir,
				"db_path":      cfg.DBPath,
				"pricing_path": cfg.PricingPath,
				"log_dir":      cfg.LogDir(),
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), payload)
			}
			for _, key := range []string{"config_path", "config_dir", "state_dir", "db_path", "pricing_path", "log_dir"} {
				fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", key, payload[key])
			}
			return nil
		},
	}
	pathCmd.Flags().BoolVar(&jsonOutput, "json", false, "print the result as JSON")
	cmd.AddCommand(pathCmd)
	return cmd
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the current broxy version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), Version)
			return nil
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
			_, store, _, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
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
	var monthlyLimit float64
	createCmd := &cobra.Command{
		Use:   "create --name <name>",
		Short: "Create a new client API key",
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("name")
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			_, store, _, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			token, err := security.RandomToken("bpx_", 24)
			if err != nil {
				return err
			}
			var monthlyLimitPtr *float64
			if monthlyLimit > 0 {
				monthlyLimitPtr = &monthlyLimit
			}
			item, err := store.CreateAPIKey(cmd.Context(), name, security.KeyPrefix(token), security.HashAPIKey(token), contentLogging, monthlyLimitPtr)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ID: %s\nName: %s\nKey: %s\n", item.ID, item.Name, token)
			if item.MonthlyLimitUSD != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Monthly limit: $%.2f\n", *item.MonthlyLimitUSD)
			}
			return nil
		},
	}
	createCmd.Flags().String("name", "", "display name for the key")
	createCmd.Flags().BoolVar(&contentLogging, "log-content", false, "store prompts and outputs for this key")
	createCmd.Flags().Float64Var(&monthlyLimit, "monthly-limit", 0, "monthly USD spending limit (0 = unlimited)")
	cmd.AddCommand(createCmd)
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List API keys",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, _, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			items, err := store.ListAPIKeys(cmd.Context())
			if err != nil {
				return err
			}
			for _, item := range items {
				limitStr := "unlimited"
				if item.MonthlyLimitUSD != nil {
					limitStr = fmt.Sprintf("$%.2f", *item.MonthlyLimitUSD)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\tlimit=%s\tcontent_logging=%t\tenabled=%t\tlast_used=%s\n", item.ID, item.Name, limitStr, item.ContentLogging, item.Enabled, item.LastUsedAt.Format(time.RFC3339))
			}
			return nil
		},
	})
	setLimitCmd := &cobra.Command{
		Use:   "set-limit <id> --monthly-limit <amount>",
		Short: "Set or update the monthly USD spending limit for an API key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			limit, _ := cmd.Flags().GetFloat64("monthly-limit")
			_, store, _, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			var limitPtr *float64
			if limit > 0 {
				limitPtr = &limit
			}
			if err := store.UpdateAPIKeyLimit(cmd.Context(), args[0], limitPtr); err != nil {
				return err
			}
			if limitPtr == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Updated API key %s to unlimited\n", args[0])
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Updated API key %s with monthly limit: $%.2f\n", args[0], *limitPtr)
			}
			return nil
		},
	}
	setLimitCmd.Flags().Float64("monthly-limit", 0, "monthly USD spending limit (0 = unlimited)")
	cmd.AddCommand(setLimitCmd)
	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <id>",
		Short: "Disable an API key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, _, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			return store.DisableAPIKey(cmd.Context(), args[0])
		},
	})
	usageCmd := &cobra.Command{
		Use:   "usage [--month YYYY-MM]",
		Short: "Show per-key usage for a given month (defaults to current month)",
		RunE: func(cmd *cobra.Command, args []string) error {
			month, _ := cmd.Flags().GetString("month")
			if month == "" {
				month = time.Now().UTC().Format("2006-01")
			}
			_, store, _, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			usages, err := store.ListAPIKeyUsage(cmd.Context(), month)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Month %s usage:\n", month)
			for _, usage := range usages {
				limitStr := "unlimited"
				overStr := ""
				if usage.MonthlyLimitUSD != nil {
					limitStr = fmt.Sprintf("$%.2f", *usage.MonthlyLimitUSD)
					if usage.IsOverLimit {
						overStr = " [OVER LIMIT]"
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\trequests=%d\ttokens=%d\tcost=$%.6f\tlimit=%s%s\n", usage.APIKeyID, usage.APIKeyName, usage.Requests, usage.TotalTokens, usage.EstimatedCostUSD, limitStr, overStr)
			}
			return nil
		},
	}
	usageCmd.Flags().String("month", "", "month in YYYY-MM format (defaults to current month)")
	cmd.AddCommand(usageCmd)
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
			cfg, store, _, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			previous, err := store.GetModelRoute(cmd.Context(), alias)
			if err != nil {
				return err
			}
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
			if err := syncModelPricing(cmd.Context(), store, cfg.PricingPath, previous, item); err != nil {
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
	removeCmd := &cobra.Command{
		Use:     "remove --alias <alias>",
		Aliases: []string{"rm", "delete"},
		Short:   "Remove a model alias",
		RunE: func(cmd *cobra.Command, args []string) error {
			alias, _ := cmd.Flags().GetString("alias")
			if alias == "" {
				return fmt.Errorf("--alias is required")
			}
			cfg, store, _, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			previous, err := store.DeleteModelRoute(cmd.Context(), alias)
			if err != nil {
				return err
			}
			if previous == nil {
				return fmt.Errorf("model route %q not found", alias)
			}
			if err := syncModelPricing(cmd.Context(), store, cfg.PricingPath, previous, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s -> %s (%s)\n", previous.Alias, previous.BedrockModelID, previous.Region)
			return nil
		},
	}
	removeCmd.Flags().String("alias", "", "proxy alias")
	cmd.AddCommand(removeCmd)
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List model routes",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, _, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
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
			_, store, _, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
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
			_, store, _, _, cleanup, err := bootstrap(cmd.Context(), *configPath)
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

func bootstrap(ctx context.Context, configPath string) (*cfgpkg.Config, *db.Store, *awsbedrock.Client, *slog.Logger, func(), error) {
	path, err := cfgpkg.ConfigPath(configPath)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	cfg, err := cfgpkg.Load(path)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("load config %s: %w", path, err)
	}
	if err := cfgpkg.EnsureLayout(cfg); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if err := cfgpkg.MigrateLegacyState(path, cfg); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if err := cfgpkg.ApplyEnv(cfg.Env); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if err := seedPricing(ctx, store, cfg.PricingPath); err != nil {
		store.Close()
		return nil, nil, nil, nil, nil, err
	}
	logger := logging.FromEnv()
	provider, err := awsbedrock.NewWithLogger(ctx, cfg.Upstream, logger)
	if err != nil {
		store.Close()
		return nil, nil, nil, nil, nil, err
	}
	return cfg, store, provider, logger, func() {
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

func syncModelPricing(ctx context.Context, store *db.Store, path string, previous, current *domain.ModelRoute) error {
	if current != nil {
		entry, err := pricing.EnsureEntry(path, current.BedrockModelID, current.Region)
		if err != nil {
			return err
		}
		if err := store.UpsertPricingEntries(ctx, []domain.PricingEntry{*entry}); err != nil {
			return err
		}
	}
	if previous == nil {
		return nil
	}
	if current != nil && previous.BedrockModelID == current.BedrockModelID && previous.Region == current.Region {
		return nil
	}
	return removeUnusedPricingEntry(ctx, store, path, previous.BedrockModelID, previous.Region)
}

func removeUnusedPricingEntry(ctx context.Context, store *db.Store, path, modelID, region string) error {
	routes, err := store.ListModelRoutes(ctx)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.BedrockModelID == modelID && route.Region == region {
			return nil
		}
	}
	if _, err := pricing.RemoveEntry(path, modelID, region); err != nil {
		return err
	}
	return store.DeletePricingEntry(ctx, modelID, region)
}

func awsString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func loadOrDefaultConfig(path string) (*cfgpkg.Config, error) {
	if _, err := os.Stat(path); err == nil {
		return cfgpkg.Load(path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return cfgpkg.DefaultForPath(path)
}

func serviceDefinition(configOverride string, requireConfig bool) (*service.Definition, *cfgpkg.Config, error) {
	path, err := cfgpkg.ConfigPath(configOverride)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := loadOrDefaultConfig(path)
	if err != nil {
		return nil, nil, err
	}
	if requireConfig {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil, fmt.Errorf("config not found at %s; run `broxy init` first", path)
			}
			return nil, nil, err
		}
	}
	target, err := service.CurrentTarget()
	if err != nil {
		return nil, nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("locate current executable: %w", err)
	}
	env := service.CapturedEnvironment()
	for key, value := range cfg.Env {
		env[key] = value
	}
	def, err := service.NewDefinition(target, cfg, path, executable, env)
	if err != nil {
		return nil, nil, err
	}
	return def, cfg, nil
}

func writeJSON(w interface{ Write([]byte) (int, error) }, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = w.Write(encoded)
	return err
}
