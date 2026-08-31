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
	"github.com/rxbynerd/billet/internal/rpcserver"
	"github.com/rxbynerd/billet/internal/secret"
	"github.com/rxbynerd/billet/internal/service"
)

// shutdownGrace bounds how long `billet serve` waits for in-flight
// requests to finish after a shutdown signal.
const shutdownGrace = 10 * time.Second

// HTTP server timeouts, applied to both transports' listeners. The
// endpoints are unauthenticated by default (docs/security.md), so
// bounding how long a connection may sit idle or trickle in headers/a
// body matters: without these, a slow client (or a deliberate
// slowloris) can hold a connection open indefinitely.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
)

func newServeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Billet server",
		Long: `Start the server exposing save_memory and search_memory, backed by
the configured storage backend, on the enabled transports: MCP
Streamable HTTP (--mcp, default on) for direct agent-environment
access, and/or billet.v1.MemoryService Connect RPC (--rpc, default off)
for control-plane-proxied access.

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
	svc := service.New(b, guard)

	// One http.Server per enabled transport: separate listeners let the
	// two deployment models expose different network surfaces
	// (docs/DECISIONS.md, "two transports"). The service (and so the
	// cost guard) is shared, so the budget gates total calls across both.
	type transport struct {
		name   string
		server *http.Server
	}
	var transports []transport
	if cfg.MCP.Enabled {
		transports = append(transports, transport{name: "mcp", server: &http.Server{
			Addr:              cfg.MCP.Listen,
			Handler:           mcpserver.New(svc).Handler(),
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
		}})
	}
	if cfg.RPC.Enabled {
		transports = append(transports, transport{name: "rpc", server: &http.Server{
			Addr:              cfg.RPC.Listen,
			Handler:           rpcserver.New(svc).Handler(),
			// Cleartext gRPC needs unencrypted HTTP/2.
			Protocols:         rpcserver.Protocols(),
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
		}})
	}

	logger.Info("billet serving",
		"backend", cfg.Backend.Type, "namespace", cfg.Namespace,
		"mcp", listenOrDisabled(cfg.MCP), "rpc", listenOrDisabled(cfg.RPC))

	errCh := make(chan error, len(transports))
	for _, tr := range transports {
		go func() {
			if err := tr.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s listener: %w", tr.name, err)
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	var serveErr error
	select {
	case serveErr = <-errCh:
		// Fail fast, deliberately: one dead transport takes the process
		// down rather than leaving a half-serving Billet up — the same
		// no-silent-degradation posture as fail-closed backend
		// construction.
		logger.Error("listener failed; shutting down", "error", serveErr)
	case sig := <-sigCh:
		logger.Info("shutting down", "signal", sig.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	for _, tr := range transports {
		if err := tr.server.Shutdown(shutdownCtx); err != nil {
			logger.Warn("shutdown grace expired with requests still in flight",
				"transport", tr.name, "error", err)
		}
	}

	// A second listener may have failed while the first failure was being
	// handled; surface it in the logs rather than dropping it.
	for {
		select {
		case err := <-errCh:
			logger.Error("additional listener failure during shutdown", "error", err)
		default:
			return serveErr
		}
	}
}

func listenOrDisabled(t config.TransportConfig) string {
	if !t.Enabled {
		return "disabled"
	}
	return t.Listen
}
