package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/gateway"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/spf13/cobra"
)

var version = "dev"

func SetVersion(value string) {
	if value != "" {
		version = value
	}
}

type options struct {
	addr            string
	providers       string
	cache           string
	credentials     string
	refreshInterval time.Duration
	maxAttempts     int
}

func Execute() error {
	opts := defaultOptions()
	root := &cobra.Command{
		Use:           "free-router",
		Short:         "An auto-updating router for free AI models",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServer(cmd.Context(), opts)
		},
	}
	bindFlags(root, &opts)
	addAuthCommands(root, &opts)

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
		Short: "Fetch and print models from every configured free provider",
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
		Use:   "providers",
		Short: "Show built-in free providers and configuration status",
		RunE: func(_ *cobra.Command, _ []string) error {
			vault := credentials.New(opts.credentials)
			return json.NewEncoder(os.Stdout).Encode(provider.BuiltinStatus(vault.Get))
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

func defaultOptions() options {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = os.TempDir()
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir = cacheDir
	}
	return options{
		addr:            envOr("FREE_ROUTER_ADDR", ":1314"),
		providers:       os.Getenv("FREE_ROUTER_PROVIDERS"),
		cache:           filepath.Join(cacheDir, "free-router", "models.json"),
		credentials:     envOr("FREE_ROUTER_CREDENTIALS", filepath.Join(configDir, "free-router", "credentials.json")),
		refreshInterval: time.Hour,
		maxAttempts:     6,
	}
}

func bindFlags(command *cobra.Command, opts *options) {
	command.PersistentFlags().StringVar(&opts.addr, "addr", opts.addr, "listen address")
	command.PersistentFlags().StringVar(&opts.providers, "providers-json", opts.providers, "custom OpenAI-compatible free providers as JSON")
	command.PersistentFlags().StringVar(&opts.cache, "cache", opts.cache, "model catalog cache file")
	command.PersistentFlags().StringVar(&opts.credentials, "credentials", opts.credentials, "saved provider credentials file")
	command.PersistentFlags().DurationVar(&opts.refreshInterval, "refresh", opts.refreshInterval, "model catalog refresh interval")
	command.PersistentFlags().IntVar(&opts.maxAttempts, "max-attempts", opts.maxAttempts, "maximum upstream attempts for model=auto")
}

func runServer(ctx context.Context, opts options) error {
	if opts.maxAttempts < 1 {
		return errors.New("max-attempts must be at least 1")
	}
	registry, err := newRegistry(opts)
	if err != nil {
		return err
	}
	store := catalog.New(registry, opts.cache, catalogHTTPClient())
	if err := store.Bootstrap(ctx); err != nil {
		return fmt.Errorf("load free model catalog: %w", err)
	}
	store.Start(ctx, opts.refreshInterval)

	handler := gateway.New(store, registry, gateway.Config{MaxAttempts: opts.maxAttempts}, http.DefaultClient)
	server := &http.Server{
		Addr:              opts.addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("free-router started", "addr", opts.addr, "providers", len(registry.All()), "models", len(store.Models()))
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
	return provider.NewRegistry(opts.providers, vault.Get)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func catalogHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}
