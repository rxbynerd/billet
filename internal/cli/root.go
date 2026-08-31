// Package cli defines the billet command tree and binds flags onto the
// declarative BilletConfig. Commands stay thin: they resolve
// configuration and hand off to the server; no protocol or backend
// behaviour lives here.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Execute runs the billet command tree and returns the process exit code
// for main to pass to os.Exit: 0 on success, 1 for any usage,
// configuration, or startup failure.
func Execute() int {
	if err := NewRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "billet:", err)
		return 1
	}
	return 0
}

// NewRootCommand builds the billet command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "billet",
		Short: "Billet is the Equestrianism suite's memory sidecar",
		Long: `Billet exposes save_memory and search_memory over two transports —
MCP Streamable HTTP for direct agent access, and billet.v1.MemoryService
Connect RPC for control-plane-proxied deployments — backed by a
pluggable storage backend (an in-process default, or AWS Bedrock
AgentCore Memory). It owns the protocols, safety posture, and backend
pluggability; it does not implement memory extraction or consolidation
itself.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newServeCommand(),
		newConfigCommand(),
	)

	return root
}
