// Command billet is the Equestrianism suite's MCP memory sidecar: it
// exposes save_memory and search_memory over Streamable HTTP, backed by
// a pluggable storage backend.
package main

import (
	"os"

	"github.com/rxbynerd/billet/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
