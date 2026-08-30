package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/billet/internal/config"
)

// addConfigFlags binds the shared BilletConfig flag surface plus the
// CLI-level --config flag, which names the base config rather than
// being part of it.
func addConfigFlags(cmd *cobra.Command) {
	config.RegisterFlags(cmd.Flags())
	cmd.Flags().String("config", "", `base BilletConfig path ("-" or piped stdin for composition)`)
}

// resolveConfig produces the run's BilletConfig: defaults, overlaid by
// the base config (--config file, or stdin when piped), overlaid by
// explicitly set flags. Validation is the caller's responsibility: a
// pipeline stage composing a partial config (e.g. `billet config`
// without --validate) must not be forced to already be complete.
func resolveConfig(cmd *cobra.Command) (config.BilletConfig, error) {
	cfg, err := loadBase(cmd)
	if err != nil {
		return config.BilletConfig{}, err
	}

	if err := config.ApplyFlags(&cfg, cmd.Flags()); err != nil {
		return config.BilletConfig{}, err
	}

	return cfg, nil
}

// loadBase reads the base config from --config (a path, or "-" for
// stdin), or from stdin when it is piped — the pipeline-composition
// path. With neither, the defaults are the base.
func loadBase(cmd *cobra.Command) (config.BilletConfig, error) {
	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return config.BilletConfig{}, err
	}

	switch {
	case path == "-":
		return config.Decode(cmd.InOrStdin())
	case path != "":
		f, err := os.Open(path)
		if err != nil {
			return config.BilletConfig{}, fmt.Errorf("open base config: %w", err)
		}
		defer f.Close()
		return config.Decode(f)
	case stdinIsPiped(cmd.InOrStdin()):
		return config.Decode(cmd.InOrStdin())
	default:
		return config.Default(), nil
	}
}

// stdinIsPiped reports whether the command's input is a pipe or file
// rather than a terminal, so an interactive `billet serve` never blocks
// waiting on stdin. An unknown reader type is not treated as an implicit
// config source.
func stdinIsPiped(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}
