package cli

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/rxbynerd/billet/internal/backend"
	"github.com/rxbynerd/billet/internal/config"
)

func TestBuildBackendDefaultsToMemory(t *testing.T) {
	cfg := config.Default()
	b, err := buildBackend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildBackend: %v", err)
	}
	if _, ok := b.(*backend.Memory); !ok {
		t.Errorf("buildBackend returned %T, want *backend.Memory", b)
	}
}

func TestBuildBackendUnknownTypeErrors(t *testing.T) {
	cfg := config.Default()
	cfg.Backend.Type = "not-a-real-backend"
	if _, err := buildBackend(context.Background(), cfg); err == nil {
		t.Fatal("buildBackend accepted an unknown backend type")
	}
}

// TestBuildBackendAgentCoreMemoryFailsClosed pins the safety-posture
// requirement: a misconfigured (or credential-less, in this
// environment) agentcore-memory backend must return an error rather
// than silently falling back to the memory backend.
func TestBuildBackendAgentCoreMemoryFailsClosed(t *testing.T) {
	cfg := config.Default()
	cfg.Backend.Type = config.BackendAgentCoreMemory
	cfg.Backend.Region = "eu-west-2"
	cfg.Backend.MemoryID = "mem-1"
	cfg.Namespace = "prod"
	cfg.Backend.CredentialsRef = "secret://BILLET_TEST_UNSET_PROFILE_VAR"

	if _, err := buildBackend(context.Background(), cfg); err == nil {
		t.Fatal("buildBackend accepted an agentcore-memory config with an unresolvable credentialsRef")
	}
}

func TestBuildBackendBoltConstructs(t *testing.T) {
	cfg := config.Default()
	cfg.Backend.Type = config.BackendBolt
	cfg.Backend.Path = filepath.Join(t.TempDir(), "billet.db")

	b, err := buildBackend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildBackend: %v", err)
	}

	closer, ok := b.(io.Closer)
	if !ok {
		t.Fatalf("buildBackend returned %T, want it to implement io.Closer", b)
	}
	if err := closer.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestBuildBackendBoltFailsClosed pins the safety-posture requirement: a
// bolt backend whose database path names a nonexistent directory must
// return an error rather than silently falling back to the memory
// backend.
func TestBuildBackendBoltFailsClosed(t *testing.T) {
	cfg := config.Default()
	cfg.Backend.Type = config.BackendBolt
	cfg.Backend.Path = filepath.Join(t.TempDir(), "missing", "billet.db")

	if _, err := buildBackend(context.Background(), cfg); err == nil {
		t.Fatal("buildBackend accepted a bolt path with a nonexistent parent directory")
	}
}
