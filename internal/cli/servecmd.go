package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/billet/internal/backend"
	"github.com/rxbynerd/billet/internal/config"
	"github.com/rxbynerd/billet/internal/cost"
	"github.com/rxbynerd/billet/internal/mcpserver"
	"github.com/rxbynerd/billet/internal/secret"
	"github.com/rxbynerd/billet/internal/service"
)

// shutdownGrace bounds how long `billet serve` waits for in-flight
// requests to finish after a shutdown signal.
const shutdownGrace = 10 * time.Second

// HTTP server timeouts. The MCP endpoint is unauthenticated by default
// (docs/security.md), so bounding how long a connection may sit idle or
// trickle in headers/a body matters: without these, a slow client (or a
// deliberate slowloris) can hold a connection open indefinitely.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
)

func newServeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Billet MCP server",
		Long: `Start the MCP Streamable HTTP server exposing save_memory and
search_memory, backed by the configured storage backend.

Fails closed: if backend.type names a real backend (agentcore-memory)
and it cannot be constructed (bad credentials, unreachable region, and
so on), billet exits non-zero rather than falling back to the in-process
memory backend.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("invalid billet config: %w", err)
			}
			return runServe(cmd, cfg)
		},
	}
	addConfigFlags(cmd)
	return cmd
}

// buildBackend constructs the Backend named by cfg.Backend.Type. It never
// falls back silently: an agentcore-memory config that fails to
// construct returns an error, not a working memory backend.
func buildBackend(ctx context.Context, cfg config.BilletConfig) (backend.Backend, error) {
	switch cfg.Backend.Type {
	case "", config.BackendMemory:
		return backend.NewMemoryBackend(), nil
	case config.BackendAgentCoreMemory:
		return backend.NewAgentCoreMemory(ctx, backend.AgentCoreMemoryConfig{
			Region:         cfg.Backend.Region,
			MemoryID:       cfg.Backend.MemoryID,
			Namespace:      cfg.Namespace,
			CredentialsRef: cfg.Backend.CredentialsRef,
		}, secret.Default())
	default:
		return nil, fmt.Errorf("unknown backend.type %q", cfg.Backend.Type)
	}
}

func runServe(cmd *cobra.Command, cfg config.BilletConfig) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	b, err := buildBackend(ctx, cfg)
	if err != nil {
		logger.Error("backend construction failed; refusing to start",
			"backend", cfg.Backend.Type, "error", err)
		return fmt.Errorf("backend construction failed: %w", err)
	}

	guard := cost.NewGuard(cfg.Budget.MonthlyGBP, os.Stderr)
	srv := mcpserver.New(service.New(b, guard))

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("billet serving",
			"listen", cfg.Listen, "backend", cfg.Backend.Type, "namespace", cfg.Namespace)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		logger.Info("shutting down", "signal", sig.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown grace expired with requests still in flight", "error", err)
	}
	return nil
}
