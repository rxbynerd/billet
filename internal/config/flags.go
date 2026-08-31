package config

import (
	"fmt"

	"github.com/spf13/pflag"
)

// RegisterFlags registers the BilletConfig flag surface on fs with its
// documented defaults. Flag defaults exist for help text; resolution
// always starts from Default() and ApplyFlags overlays only the flags
// the user explicitly set, so a piped base config is never clobbered by
// a default.
func RegisterFlags(fs *pflag.FlagSet) {
	d := Default()
	fs.Bool("mcp", d.MCP.Enabled, "serve the MCP Streamable HTTP endpoint (direct agent-environment access)")
	fs.String("listen", d.MCP.Listen, "address the MCP Streamable HTTP endpoint binds")
	fs.Bool("rpc", d.RPC.Enabled, "serve the billet.v1.MemoryService Connect RPC endpoint (control-plane proxied access)")
	fs.String("rpc-listen", d.RPC.Listen, "address the Connect RPC endpoint binds")
	fs.String("namespace", d.Namespace, "long-term recall scope (AgentCore actorId); required for the agentcore-memory backend")
	fs.String("backend", d.Backend.Type, fmt.Sprintf("backend type: %q or %q", BackendMemory, BackendAgentCoreMemory))
	fs.String("region", "", "AWS region (agentcore-memory backend)")
	fs.String("memory-id", "", "AgentCore Memory resource ID (agentcore-memory backend)")
	fs.String("credentials-ref", "", "secret:// reference selecting AWS credentials (agentcore-memory backend)")
	fs.Float64("budget", 0, "rough monthly cost estimate cap in GBP; unset means uncapped")
}

// ApplyFlags overlays the flags the user explicitly set onto cfg. Unset
// flags leave cfg untouched — the merge semantics that make pipeline
// composition work.
func ApplyFlags(cfg *BilletConfig, fs *pflag.FlagSet) error {
	fs.Visit(func(f *pflag.Flag) {
		switch f.Name {
		case "mcp":
			cfg.MCP.Enabled, _ = fs.GetBool(f.Name)
		case "listen":
			cfg.MCP.Listen, _ = fs.GetString(f.Name)
		case "rpc":
			cfg.RPC.Enabled, _ = fs.GetBool(f.Name)
		case "rpc-listen":
			cfg.RPC.Listen, _ = fs.GetString(f.Name)
		case "namespace":
			cfg.Namespace, _ = fs.GetString(f.Name)
		case "backend":
			cfg.Backend.Type, _ = fs.GetString(f.Name)
		case "region":
			cfg.Backend.Region, _ = fs.GetString(f.Name)
		case "memory-id":
			cfg.Backend.MemoryID, _ = fs.GetString(f.Name)
		case "credentials-ref":
			cfg.Backend.CredentialsRef, _ = fs.GetString(f.Name)
		case "budget":
			cfg.Budget.MonthlyGBP, _ = fs.GetFloat64(f.Name)
		}
	})
	return nil
}
