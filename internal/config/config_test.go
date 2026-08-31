package config

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestDefault(t *testing.T) {
	d := Default()
	if d.Backend.Type != BackendMemory {
		t.Errorf("Backend.Type = %q, want %q", d.Backend.Type, BackendMemory)
	}
	if d.Namespace != DefaultNamespace {
		t.Errorf("Namespace = %q, want %q", d.Namespace, DefaultNamespace)
	}
	if !d.MCP.Enabled || d.MCP.Listen != DefaultMCPListen {
		t.Errorf("MCP = %+v, want enabled on %q", d.MCP, DefaultMCPListen)
	}
	if d.RPC.Enabled {
		t.Errorf("RPC.Enabled = true, want the RPC transport off by default")
	}
	if d.RPC.Listen != DefaultRPCListen {
		t.Errorf("RPC.Listen = %q, want %q pre-filled so enabling is a one-key change", d.RPC.Listen, DefaultRPCListen)
	}
	if d.Budget.MonthlyGBP != 0 {
		t.Errorf("Budget.MonthlyGBP = %v, want 0 (uncapped)", d.Budget.MonthlyGBP)
	}
	if err := d.Validate(); err != nil {
		t.Errorf("Default().Validate() = %v, want nil", err)
	}
}

func TestDecodeEmptyYieldsDefaults(t *testing.T) {
	cfg, err := Decode(strings.NewReader(""))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg != Default() {
		t.Errorf("Decode(empty) = %+v, want Default()", cfg)
	}
}

func TestDecodeJSONRoundtrip(t *testing.T) {
	input := `{
		"backend": {"type": "agentcore-memory", "region": "eu-west-2", "memoryId": "mem-123", "credentialsRef": "secret://AWS_PROFILE"},
		"namespace": "prod",
		"mcp": {"enabled": true, "listen": ":9000"},
		"rpc": {"enabled": true, "listen": ":9001"},
		"budget": {"monthlyGbp": 50}
	}`
	cfg, err := Decode(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.Backend.Type != BackendAgentCoreMemory {
		t.Errorf("Backend.Type = %q, want agentcore-memory", cfg.Backend.Type)
	}
	if cfg.Backend.Region != "eu-west-2" {
		t.Errorf("Backend.Region = %q", cfg.Backend.Region)
	}
	if cfg.Backend.MemoryID != "mem-123" {
		t.Errorf("Backend.MemoryID = %q", cfg.Backend.MemoryID)
	}
	if cfg.Backend.CredentialsRef != "secret://AWS_PROFILE" {
		t.Errorf("Backend.CredentialsRef = %q", cfg.Backend.CredentialsRef)
	}
	if cfg.Namespace != "prod" {
		t.Errorf("Namespace = %q", cfg.Namespace)
	}
	if cfg.MCP.Listen != ":9000" {
		t.Errorf("MCP.Listen = %q", cfg.MCP.Listen)
	}
	if !cfg.RPC.Enabled || cfg.RPC.Listen != ":9001" {
		t.Errorf("RPC = %+v, want enabled on :9001", cfg.RPC)
	}
	if cfg.Budget.MonthlyGBP != 50 {
		t.Errorf("Budget.MonthlyGBP = %v", cfg.Budget.MonthlyGBP)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}

	var buf bytes.Buffer
	if err := cfg.EncodeJSON(&buf); err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	roundtripped, err := Decode(&buf)
	if err != nil {
		t.Fatalf("Decode(EncodeJSON output): %v", err)
	}
	if roundtripped != cfg {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", roundtripped, cfg)
	}
}

func TestDecodeUnknownFieldRejected(t *testing.T) {
	_, err := Decode(strings.NewReader(`{"backendd": {"type": "memory"}}`))
	if err == nil {
		t.Fatal("Decode accepted an unknown field; want error")
	}
}

func TestDecodePartialOverlaysDefaults(t *testing.T) {
	cfg, err := Decode(strings.NewReader(`{"namespace": "custom"}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.Namespace != "custom" {
		t.Errorf("Namespace = %q, want custom", cfg.Namespace)
	}
	if cfg.MCP.Listen != DefaultMCPListen {
		t.Errorf("MCP.Listen = %q, want default %q to survive a partial overlay", cfg.MCP.Listen, DefaultMCPListen)
	}
	if !cfg.MCP.Enabled {
		t.Error("MCP.Enabled = false, want the default true to survive a partial overlay")
	}
	if cfg.Backend.Type != BackendMemory {
		t.Errorf("Backend.Type = %q, want default %q to survive a partial overlay", cfg.Backend.Type, BackendMemory)
	}
}

func TestValidateAgentCoreMemoryRequiresNamespace(t *testing.T) {
	cfg := Default()
	cfg.Backend.Type = BackendAgentCoreMemory
	cfg.Backend.Region = "eu-west-2"
	cfg.Backend.MemoryID = "mem-123"
	cfg.Namespace = ""

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted agentcore-memory with an empty namespace")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Errorf("error = %v, want it to mention namespace", err)
	}
}

func TestValidateAgentCoreMemoryRequiresRegionAndMemoryID(t *testing.T) {
	tests := []struct {
		name string
		cfg  func() BilletConfig
	}{
		{"missing region", func() BilletConfig {
			c := Default()
			c.Backend.Type = BackendAgentCoreMemory
			c.Backend.MemoryID = "mem-123"
			return c
		}},
		{"missing memoryId", func() BilletConfig {
			c := Default()
			c.Backend.Type = BackendAgentCoreMemory
			c.Backend.Region = "eu-west-2"
			return c
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg().Validate(); err == nil {
				t.Fatal("Validate accepted an incomplete agentcore-memory config")
			}
		})
	}
}

func TestValidateUnknownBackendType(t *testing.T) {
	cfg := Default()
	cfg.Backend.Type = "made-up-backend"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted an unknown backend type")
	}
}

func TestValidateCredentialsRefMustBeSecretRef(t *testing.T) {
	cfg := Default()
	cfg.Backend.Type = BackendAgentCoreMemory
	cfg.Backend.Region = "eu-west-2"
	cfg.Backend.MemoryID = "mem-123"
	cfg.Backend.CredentialsRef = "AKIALITERALVALUE"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted a literal credentialsRef")
	}
}

// TestValidateCredentialsRefErrorNeverEchoesValue pins SECURITY.md's claim
// that a rejected credentialsRef is never echoed into the error: the
// offending value may itself be a credential.
func TestValidateCredentialsRefErrorNeverEchoesValue(t *testing.T) {
	const literal = "totally-secret-literal-value-12345"
	cfg := Default()
	cfg.Backend.Type = BackendAgentCoreMemory
	cfg.Backend.Region = "eu-west-2"
	cfg.Backend.MemoryID = "mem-123"
	cfg.Backend.CredentialsRef = literal

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a literal credentialsRef")
	}
	if strings.Contains(err.Error(), literal) {
		t.Fatalf("error echoes the literal credentialsRef value: %v", err)
	}
}

func TestValidateNamespaceRejectsUnsafeShape(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
	}{
		{"too short", "ab"},
		{"leading hyphen", "-acme"},
		{"contains whitespace", "acme corp"},
		{"contains a slash", "acme/corp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Backend.Type = BackendAgentCoreMemory
			cfg.Backend.Region = "eu-west-2"
			cfg.Backend.MemoryID = "mem-123"
			cfg.Namespace = tt.namespace
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate accepted namespace %q", tt.namespace)
			}
		})
	}
}

func TestValidateNamespaceAcceptsSafeShape(t *testing.T) {
	cfg := Default()
	cfg.Backend.Type = BackendAgentCoreMemory
	cfg.Backend.Region = "eu-west-2"
	cfg.Backend.MemoryID = "mem-123"
	cfg.Namespace = "acme-corp_1"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected a safely-shaped namespace: %v", err)
	}
}

func TestValidateNegativeBudgetRejected(t *testing.T) {
	cfg := Default()
	cfg.Budget.MonthlyGBP = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted a negative budget")
	}
}

func TestValidateTransportRules(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*BilletConfig)
		wantErr bool
	}{
		{"empty mcp listen while enabled", func(c *BilletConfig) {
			c.MCP.Listen = ""
		}, true},
		{"empty rpc listen while enabled", func(c *BilletConfig) {
			c.RPC.Enabled = true
			c.RPC.Listen = ""
		}, true},
		{"no transport enabled", func(c *BilletConfig) {
			c.MCP.Enabled = false
			c.RPC.Enabled = false
		}, true},
		{"both transports on one address", func(c *BilletConfig) {
			c.RPC.Enabled = true
			c.RPC.Listen = c.MCP.Listen
		}, true},
		{"rpc only", func(c *BilletConfig) {
			c.MCP.Enabled = false
			c.RPC.Enabled = true
		}, false},
		{"both on distinct addresses", func(c *BilletConfig) {
			c.RPC.Enabled = true
		}, false},
		{"disabled mcp may have an empty listen", func(c *BilletConfig) {
			c.MCP.Enabled = false
			c.MCP.Listen = ""
			c.RPC.Enabled = true
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate accepted an invalid transport config")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate rejected a valid transport config: %v", err)
			}
		})
	}
}

// TestDecodeLegacyListenKeyRejected pins the loud failure mode for
// configs written against the single-transport surface: the top-level
// listen key no longer exists and must error, not silently vanish.
func TestDecodeLegacyListenKeyRejected(t *testing.T) {
	if _, err := Decode(strings.NewReader(`{"listen": ":9000"}`)); err == nil {
		t.Fatal("Decode accepted the removed top-level listen key")
	}
}

func TestRedactScrubsCredentialsRef(t *testing.T) {
	cfg := Default()
	cfg.Backend.CredentialsRef = "secret://AWS_PROFILE"
	redacted := cfg.Redact()
	if redacted.Backend.CredentialsRef != "secret://[REDACTED]" {
		t.Errorf("Redact() CredentialsRef = %q, want redacted", redacted.Backend.CredentialsRef)
	}
	if cfg.Backend.CredentialsRef != "secret://AWS_PROFILE" {
		t.Error("Redact mutated the receiver")
	}
}

func TestApplyFlagsOverlaysOnlySetFlags(t *testing.T) {
	base := Default()
	base.Namespace = "from-base-config"

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs)
	if err := fs.Parse([]string{"--listen", ":9999"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if err := ApplyFlags(&base, fs); err != nil {
		t.Fatalf("ApplyFlags: %v", err)
	}

	if base.MCP.Listen != ":9999" {
		t.Errorf("MCP.Listen = %q, want :9999 (explicit flag)", base.MCP.Listen)
	}
	if base.Namespace != "from-base-config" {
		t.Errorf("Namespace = %q, want base config value to survive (flag not set)", base.Namespace)
	}
}

func TestApplyFlagsAllFields(t *testing.T) {
	cfg := Default()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs)
	args := []string{
		"--mcp=false",
		"--listen", ":1234",
		"--rpc",
		"--rpc-listen", ":4321",
		"--namespace", "ns",
		"--backend", "agentcore-memory",
		"--region", "eu-west-2",
		"--memory-id", "mem-1",
		"--credentials-ref", "secret://AWS_PROFILE",
		"--budget", "12.5",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := ApplyFlags(&cfg, fs); err != nil {
		t.Fatalf("ApplyFlags: %v", err)
	}

	want := BilletConfig{
		Backend: BackendConfig{ //nolint:gosec // CredentialsRef below is a secret:// reference name, not a literal credential
			Type:           BackendAgentCoreMemory,
			Region:         "eu-west-2",
			MemoryID:       "mem-1",
			CredentialsRef: "secret://AWS_PROFILE",
		},
		Namespace: "ns",
		MCP:       TransportConfig{Enabled: false, Listen: ":1234"},
		RPC:       TransportConfig{Enabled: true, Listen: ":4321"},
		Budget:    BudgetConfig{MonthlyGBP: 12.5},
	}
	if cfg != want {
		t.Errorf("ApplyFlags result = %+v, want %+v", cfg, want)
	}
}
