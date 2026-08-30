package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/billet/internal/config"
)

func TestConfigCommandDefaults(t *testing.T) {
	stdout, _, err := execute(t, "config")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	backend, _ := cfg["backend"].(map[string]any)
	if backend["type"] != config.BackendMemory {
		t.Errorf("backend.type = %v, want %q", backend["type"], config.BackendMemory)
	}
	if cfg["namespace"] != config.DefaultNamespace {
		t.Errorf("namespace = %v, want %q", cfg["namespace"], config.DefaultNamespace)
	}
}

func TestConfigCommandExplicitFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "base.json")
	if err := os.WriteFile(path, []byte(`{"namespace": "from-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := execute(t, "config", "--config", path)
	if err != nil {
		t.Fatalf("config --config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if cfg["namespace"] != "from-file" {
		t.Errorf("namespace = %v, want the base file's value", cfg["namespace"])
	}
}

func TestConfigCommandMissingFile(t *testing.T) {
	_, _, err := execute(t, "config", "--config", "/no/such/file.json")
	if err == nil || !strings.Contains(err.Error(), "open base config") {
		t.Errorf("err = %v, want the open-base-config failure", err)
	}
}

func TestConfigCommandMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"backend": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execute(t, "config", "--config", path); err == nil {
		t.Error("malformed base config was accepted silently")
	}
}

func TestConfigCommandFlagsOverrideBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "base.json")
	if err := os.WriteFile(path, []byte(`{"namespace": "from-file", "listen": ":1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := execute(t, "config", "--config", path, "--namespace", "from-flag")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if cfg["namespace"] != "from-flag" {
		t.Errorf("namespace = %v, want the explicit flag to win", cfg["namespace"])
	}
	if cfg["listen"] != ":1" {
		t.Errorf("listen = %v, want the base file's value to survive (flag not set)", cfg["listen"])
	}
}

// TestConfigCommandWithoutValidateAllowsIncomplete pins the pipeline-
// composition behaviour: an agentcore-memory config missing its
// namespace is still emitted (a later pipeline stage may complete it).
func TestConfigCommandWithoutValidateAllowsIncomplete(t *testing.T) {
	stdout, _, err := execute(t, "config", "--backend", "agentcore-memory", "--namespace", "")
	if err != nil {
		t.Fatalf("config without --validate rejected an incomplete config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
}

func TestConfigCommandValidateRejectsIncomplete(t *testing.T) {
	_, _, err := execute(t, "config", "--backend", "agentcore-memory", "--validate")
	if err == nil {
		t.Fatal("config --validate accepted an agentcore-memory config with no region/memoryId/namespace")
	}
}

func TestConfigCommandValidateAcceptsComplete(t *testing.T) {
	_, _, err := execute(t, "config", "--validate",
		"--backend", "agentcore-memory",
		"--namespace", "prod",
		"--region", "eu-west-2",
		"--memory-id", "mem-1",
	)
	if err != nil {
		t.Fatalf("config --validate rejected a complete config: %v", err)
	}
}

func TestConfigCommandRedactsCredentialsRef(t *testing.T) {
	stdout, _, err := execute(t, "config", "--credentials-ref", "secret://AWS_PROFILE", "--redact")
	if err != nil {
		t.Fatalf("config --redact: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	backend, _ := cfg["backend"].(map[string]any)
	if backend["credentialsRef"] != "secret://[REDACTED]" {
		t.Errorf("credentialsRef = %v, want redacted", backend["credentialsRef"])
	}
}

func TestConfigCommandWithoutRedactKeepsCredentialsRef(t *testing.T) {
	stdout, _, err := execute(t, "config", "--credentials-ref", "secret://AWS_PROFILE")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	backend, _ := cfg["backend"].(map[string]any)
	if backend["credentialsRef"] != "secret://AWS_PROFILE" {
		t.Errorf("credentialsRef = %v, want the unredacted reference by default", backend["credentialsRef"])
	}
}

func TestConfigCommandProducesNoSideEffects(t *testing.T) {
	// Two independent invocations must agree: `billet config` is
	// documented as side-effect-free (no server started, no backend
	// constructed against real credentials).
	first, _, err := execute(t, "config")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	second, _, err := execute(t, "config")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if first != second {
		t.Errorf("repeated `billet config` produced different output:\n%s\nvs\n%s", first, second)
	}
}
