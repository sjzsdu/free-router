package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sjzsdu/free-router/internal/admin"
	"github.com/sjzsdu/free-router/internal/appdirs"
	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/gateway"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
	"github.com/sjzsdu/free-router/internal/transport"
	"github.com/spf13/cobra"
)

var version = "dev"

func SetVersion(value string) {
	if value != "" {
		version = value
	}
}

type options struct {
	addr             string
	providers        string
	cache            string
	config           string
	credentials      string
	freeModels       string
	adminAllowRemote bool
	adminToken       string
	apiToken         string
	maxAttempts      int
}

func Execute() error {
	if err := loadDaemonEnvironment(); err != nil {
		return err
	}
	opts := defaultOptions()
	root := &cobra.Command{
		Use:           "free-router",
		Short:         "An auto-updating router for free AI models",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	bindFlags(root, &opts)
	addAuthCommands(root, &opts)
	addDaemonCommands(root, &opts)
	addTrayCommand(root, &opts)
	addOnboardCommand(root, &opts)
	addDiscoveryCommand(root, &opts)

	serve := &cobra.Command{
		Use:   "serve",
		Short: "Start the OpenAI-compatible gateway",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServer(cmd.Context(), opts)
		},
	}
	root.AddCommand(serve)

	root.AddCommand(&cobra.Command{
		Use:   "models",
		Short: "Print models admitted by the Formula inventory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			registry, err := newRegistry(opts)
			if err != nil {
				return err
			}
			store := catalog.New(registry, opts.cache, catalogHTTPClient())
			if err := store.Refresh(cmd.Context()); err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(store.Models())
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "validate-model-data FILE",
		Short: "Validate a generated free model manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return validateModelData(args[0], cmd.OutOrStdout())
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "providers",
		Short: "Show built-in free providers and configuration status",
		RunE: func(_ *cobra.Command, _ []string) error {
			vault := credentials.New(opts.credentials)
			envMap, err := configuredEnvMap(opts)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(provider.BuiltinStatusWithManifest(envMap, opts.freeModels, vault.Get))
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println(version)
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return root.ExecuteContext(ctx)
}

func validateModelData(path string, output io.Writer) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("model data file path must not be empty")
	}
	manifest, err := provider.LoadFreeModelManifest(path)
	if err != nil {
		return err
	}
	models := 0
	for _, entry := range manifest.Providers {
		models += len(entry.Models)
	}
	_, err = fmt.Fprintf(output, "valid free model manifest: providers=%d models=%d\n", len(manifest.Providers), models)
	return err
}

func defaultOptions() options {
	dataDir := appdirs.Default()
	return options{
		addr:             envOr("FREE_ROUTER_ADDR", "127.0.0.1:1314"),
		providers:        os.Getenv("FREE_ROUTER_PROVIDERS"),
		cache:            envOr("FREE_ROUTER_CACHE", filepath.Join(dataDir, "models.json")),
		config:           envOr("FREE_ROUTER_CONFIG", filepath.Join(dataDir, "config.json")),
		credentials:      envOr("FREE_ROUTER_CREDENTIALS", filepath.Join(dataDir, "credentials.json")),
		freeModels:       os.Getenv("FREE_ROUTER_FREE_MODELS"),
		adminAllowRemote: envBool("FREE_ROUTER_ADMIN_ALLOW_REMOTE"),
		adminToken:       os.Getenv("FREE_ROUTER_ADMIN_TOKEN"),
		apiToken:         os.Getenv("FREE_ROUTER_API_TOKEN"),
		maxAttempts:      6,
	}
}

func bindFlags(command *cobra.Command, opts *options) {
	command.PersistentFlags().StringVar(&opts.addr, "addr", opts.addr, "listen address")
	command.PersistentFlags().StringVar(&opts.providers, "providers-json", opts.providers, "custom OpenAI-compatible free providers as JSON")
	command.PersistentFlags().StringVar(&opts.cache, "cache", opts.cache, "model catalog cache file")
	command.PersistentFlags().StringVar(&opts.config, "config", opts.config, "route configuration file")
	command.PersistentFlags().StringVar(&opts.credentials, "credentials", opts.credentials, "saved provider credentials file")
	command.PersistentFlags().StringVar(&opts.freeModels, "free-models", opts.freeModels, "external free model manifest (embedded data is used by default)")
	command.PersistentFlags().BoolVar(&opts.adminAllowRemote, "admin-allow-remote", opts.adminAllowRemote, "allow the admin UI outside localhost")
	command.PersistentFlags().StringVar(&opts.apiToken, "api-token", opts.apiToken, "API token for inference endpoints (required for remote access)")
	command.PersistentFlags().IntVar(&opts.maxAttempts, "max-attempts", opts.maxAttempts, "maximum upstream attempts for model=auto")
}

func runServer(ctx context.Context, opts options) error {
	if opts.maxAttempts < 1 {
		return errors.New("max-attempts must be at least 1")
	}
	if opts.adminAllowRemote && strings.TrimSpace(opts.adminToken) == "" {
		return errors.New("FREE_ROUTER_ADMIN_TOKEN is required when admin-allow-remote is enabled")
	}
	if isRemoteAddr(opts.addr) && strings.TrimSpace(opts.apiToken) == "" {
		return errors.New("FREE_ROUTER_API_TOKEN is required when listening on a remote address (not localhost)")
	}
	vault := credentials.New(opts.credentials)
	routes, err := routing.New(opts.config)
	if err != nil {
		return fmt.Errorf("load route configuration: %w", err)
	}
	registry, err := provider.NewRegistryWithManifest(opts.providers, provider.EnvMap(routes.Config().ProviderEnv), opts.freeModels, vault.Get)
	if err != nil {
		return err
	}
	store := catalog.New(registry, opts.cache, catalogHTTPClient())
	if err := store.Bootstrap(ctx); err != nil {
		return fmt.Errorf("load free model catalog: %w", err)
	}

	tracker := health.New()
	httpClient := transport.NewClient(transport.NewConfig())
	handler := gateway.New(store, registry, gateway.Config{MaxAttempts: opts.maxAttempts, Routes: routes, Health: tracker, APIToken: opts.apiToken}, httpClient)
	reloadProviders := func(providerEnv map[string][]string) (func(), error) {
		rollback := registry.Backup()
		if err := registry.ReloadWithManifest(opts.providers, provider.EnvMap(providerEnv), opts.freeModels, vault.Get); err != nil {
			rollback()
			return nil, err
		}
		return rollback, nil
	}
	handler.Handle("GET /admin", http.RedirectHandler("/admin/", http.StatusTemporaryRedirect))
	handler.Handle("/admin/", admin.New(routes, store, vault, tracker, admin.Config{AllowRemote: opts.adminAllowRemote, Token: opts.adminToken, Version: version, FreeModels: opts.freeModels}, reloadProviders))
	server := &http.Server{
		Addr:              opts.addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// WriteTimeout acts as an idle deadline for each write: a long
		// reasoning pause in a streaming SSE response must not hard-cut
		// the stream, so it is kept well above typical model latency.
		// Stalled upstream bodies are guarded separately in copyResponse.
		WriteTimeout:   15 * time.Minute,
		IdleTimeout:    90 * time.Second,
		MaxHeaderBytes: 10 * 1024 * 1024,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("free-router started", "addr", opts.addr, "providers", len(registry.All()), "catalog_providers", len(registry.CatalogAll()), "models", len(store.Models()))
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func newRegistry(opts options) (*provider.Registry, error) {
	vault := credentials.New(opts.credentials)
	envMap, err := configuredEnvMap(opts)
	if err != nil {
		return nil, err
	}
	return provider.NewRegistryWithManifest(opts.providers, envMap, opts.freeModels, vault.Get)
}

func configuredEnvMap(opts options) (provider.EnvMap, error) {
	routes, err := routing.New(opts.config)
	if err != nil {
		return nil, fmt.Errorf("load configuration: %w", err)
	}
	return provider.EnvMap(routes.Config().ProviderEnv), nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envBool(key string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func catalogHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

func isRemoteAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return false
	}
	return host != "localhost" && host != "127.0.0.1" && host != "::1"
}

func loadDaemonEnvironment() error {
	path := strings.TrimSpace(os.Getenv("FREE_ROUTER_DAEMON_ENV_FILE"))
	if path == "" {
		return nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read daemon environment: %w", err)
	}
	var environment map[string]string
	if err := json.Unmarshal(content, &environment); err != nil {
		return fmt.Errorf("decode daemon environment: %w", err)
	}
	for name, value := range environment {
		if os.Getenv(name) == "" && value != "" {
			if err := os.Setenv(name, value); err != nil {
				return fmt.Errorf("load daemon environment %s: %w", name, err)
			}
		}
	}
	return nil
}
