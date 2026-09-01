// Package config defines BilletConfig: the single declarative
// configuration a Billet server is built from. It is JSON/YAML
// serialisable and composable — documented defaults, overlaid by a base
// config (file or stdin), overlaid by explicit flags — mirroring the
// layering used by Stirrup's RunConfig and Chiron's ResearchConfig.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Backend type identifiers for BackendConfig.Type.
const (
	// BackendMemory is the in-process, ephemeral default: no external
	// dependencies, no billing surface, safe to run with zero config.
	BackendMemory = "memory"
	// BackendAgentCoreMemory is the AWS Bedrock AgentCore Memory adapter.
	BackendAgentCoreMemory = "agentcore-memory"
	// BackendBolt is the local, persistent bbolt-file-backed backend.
	BackendBolt = "bolt"
)

// Default listen addresses for the two transports. Loopback-only:
// exposing an endpoint beyond localhost (`--listen :8140` or an explicit
// non-loopback address) is an operator opt-in, not the default, since
// both endpoints are unauthenticated (docs/security.md).
const (
	DefaultMCPListen = "127.0.0.1:8140"
	DefaultRPCListen = "127.0.0.1:8141"
)

// DefaultNamespace is the namespace used when one is not configured and
// the backend does not require one (the memory backend). agentcore-memory
// requires an explicit namespace (see Validate).
const DefaultNamespace = "default"

// namespacePattern bounds the shape of a namespace accepted for the
// agentcore-memory backend: it must be safe to use, trimmed, as an
// AgentCore actorId and as a RetrieveMemoryRecords namespace prefix.
// Rejecting anything outside this shape (in particular, leading/trailing
// whitespace) closes off a class of typo that would otherwise create a
// silent isolation gap (docs/DECISIONS.md).
var namespacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{2,63}$`)

// BackendConfig selects and configures the storage backend.
type BackendConfig struct {
	// Type is BackendMemory or BackendAgentCoreMemory. Defaults to
	// BackendMemory — Billet never defaults to a billable cloud backend.
	Type string `json:"type,omitempty" yaml:"type,omitempty"`
	// Region is the AWS region for the agentcore-memory backend.
	Region string `json:"region,omitempty" yaml:"region,omitempty"`
	// MemoryID is the AgentCore Memory resource ID for the
	// agentcore-memory backend.
	MemoryID string `json:"memoryId,omitempty" yaml:"memoryId,omitempty"`
	// CredentialsRef is a secret:// reference used to select AWS
	// credentials for the agentcore-memory backend (for example
	// secret://AWS_PROFILE, naming an environment variable that holds a
	// shared-config profile name). Never a literal credential.
	CredentialsRef string `json:"credentialsRef,omitempty" yaml:"credentialsRef,omitempty"`
	// Path is the database file for the bolt backend. Created if absent,
	// but its parent directory must already exist — a missing directory
	// fails backend construction rather than being silently created.
	// Namespace is not used by this backend.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
}

// TransportConfig configures one of Billet's two transports. Each
// enabled transport binds its own listener, so the two deployment
// models can expose different network surfaces: MCP reachable from the
// agent environment (direct model), or RPC reachable only by the
// control plane (proxied model), or both.
type TransportConfig struct {
	// Enabled controls whether `billet serve` binds this transport.
	// Serialised without omitempty on purpose: a piped `billet config`
	// output must carry an explicit false rather than re-defaulting
	// downstream.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Listen is the address this transport binds when enabled.
	Listen string `json:"listen,omitempty" yaml:"listen,omitempty"`
}

// BudgetConfig bounds the rough, call-count-based cost estimate (see
// internal/cost); it is not tied to real AgentCore billing in v1.
type BudgetConfig struct {
	// MonthlyGBP is the estimated-cost cap in GBP. Zero means uncapped.
	MonthlyGBP float64 `json:"monthlyGbp,omitempty" yaml:"monthlyGbp,omitempty"`
}

// BilletConfig declares one Billet server. Zero values mean "unset";
// Default supplies the documented defaults, and Decode overlays a base
// config on top of them, so an absent key never clobbers a default.
type BilletConfig struct {
	Backend BackendConfig `json:"backend,omitempty" yaml:"backend,omitempty"`
	// Namespace scopes every memory this server saves and recalls — the
	// long-term recall scope (AgentCore's actorId). Bound once at
	// startup, never accepted from a caller (docs/DECISIONS.md).
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	// MCP is the MCP Streamable HTTP transport (direct agent-environment
	// access, Stirrup's path). Enabled by default.
	MCP TransportConfig `json:"mcp" yaml:"mcp"`
	// RPC is the billet.v1.MemoryService Connect RPC transport
	// (control-plane-proxied access). Disabled by default.
	RPC    TransportConfig `json:"rpc" yaml:"rpc"`
	Budget BudgetConfig    `json:"budget,omitempty" yaml:"budget,omitempty"`
}

// Default returns the documented defaults: an in-process memory backend,
// namespace "default", the MCP transport enabled on DefaultMCPListen,
// the RPC transport disabled (its listen address pre-filled with
// DefaultRPCListen so enabling it is a one-key change), uncapped budget.
func Default() BilletConfig {
	return BilletConfig{
		Backend:   BackendConfig{Type: BackendMemory},
		Namespace: DefaultNamespace,
		MCP:       TransportConfig{Enabled: true, Listen: DefaultMCPListen},
		RPC:       TransportConfig{Enabled: false, Listen: DefaultRPCListen},
	}
}

// Decode reads a base BilletConfig (JSON or YAML — JSON is a YAML
// subset, so one strict decoder covers both) overlaid on the defaults.
// Empty input yields the defaults, so an empty stdin pipe is harmless.
// Unknown keys are an error: a silent typo must not silently change
// which backend a deployment talks to.
func Decode(r io.Reader) (BilletConfig, error) {
	cfg := Default()
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return Default(), nil
		}
		return BilletConfig{}, fmt.Errorf("decode billet config: %w", err)
	}
	return cfg, nil
}

// EncodeJSON writes the resolved config as indented JSON, the wire form
// emitted by `billet config` for pipeline composition.
func (c BilletConfig) EncodeJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("encode billet config: %w", err)
	}
	return nil
}

// Redact returns a copy of c with CredentialsRef rewritten to
// secret://[REDACTED], safe to share or commit but no longer runnable
// as-is.
func (c BilletConfig) Redact() BilletConfig {
	if c.Backend.CredentialsRef != "" {
		c.Backend.CredentialsRef = "secret://[REDACTED]"
	}
	return c
}

// Validate checks the declarative invariants: backend type enumeration,
// the agentcore-memory namespace requirement, the secret:// rule for
// CredentialsRef, and non-negative budget.
func (c BilletConfig) Validate() error {
	switch c.Backend.Type {
	case "", BackendMemory:
	case BackendAgentCoreMemory:
		ns := strings.TrimSpace(c.Namespace)
		if ns == "" {
			return errors.New("namespace: required when backend.type is \"agentcore-memory\" — it is the durable recall scope (AgentCore actorId) and must not be left to a default")
		}
		if !namespacePattern.MatchString(ns) {
			return errors.New("namespace: does not match the required shape ^[A-Za-z0-9][A-Za-z0-9_-]{2,63}$ — this keeps namespace prefixes from colliding under AgentCore's RetrieveMemoryRecords filter")
		}
		if strings.TrimSpace(c.Backend.Region) == "" {
			return errors.New("backend.region: required when backend.type is \"agentcore-memory\"")
		}
		if strings.TrimSpace(c.Backend.MemoryID) == "" {
			return errors.New("backend.memoryId: required when backend.type is \"agentcore-memory\"")
		}
	case BackendBolt:
		if strings.TrimSpace(c.Backend.Path) == "" {
			return errors.New("backend.path: required when backend.type is \"bolt\"")
		}
	default:
		return fmt.Errorf("backend.type: %q is not %q, %q, or %q", c.Backend.Type, BackendMemory, BackendAgentCoreMemory, BackendBolt)
	}

	if c.Backend.CredentialsRef != "" && !strings.HasPrefix(c.Backend.CredentialsRef, "secret://") {
		return errors.New("backend.credentialsRef: not a secret:// reference — literal credentials never live in config")
	}

	if !c.MCP.Enabled && !c.RPC.Enabled {
		return errors.New("mcp.enabled/rpc.enabled: at least one transport must be enabled — a Billet with neither serves nothing")
	}
	if c.MCP.Enabled && strings.TrimSpace(c.MCP.Listen) == "" {
		return errors.New("mcp.listen: must not be empty when mcp.enabled is true")
	}
	if c.RPC.Enabled && strings.TrimSpace(c.RPC.Listen) == "" {
		return errors.New("rpc.listen: must not be empty when rpc.enabled is true")
	}
	if c.MCP.Enabled && c.RPC.Enabled && strings.TrimSpace(c.MCP.Listen) == strings.TrimSpace(c.RPC.Listen) {
		return errors.New("mcp.listen/rpc.listen: the two transports bind separate listeners and must not share an address")
	}

	if c.Budget.MonthlyGBP < 0 {
		return fmt.Errorf("budget.monthlyGbp: %v is negative", c.Budget.MonthlyGBP)
	}

	return nil
}
