package cli

import (
	"context"
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
