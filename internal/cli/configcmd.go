package cli

import (
	"github.com/spf13/cobra"
)

func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Resolve and print the BilletConfig as JSON, without starting a server",
		Long: `Resolve the base config (stdin or --config) plus any flags and emit the
result as JSON. Composable in a pipeline:

  billet config --backend agentcore-memory --region eu-west-2 \
    | billet config --namespace prod \
    | billet serve --config -

Without --validate, a partial or chained config is emitted as-is so a
pipeline stage can complete it downstream. With --validate, the resolved
config is checked with BilletConfig.Validate and the command exits
non-zero on failure.

--redact defaults to true: a real backend.credentialsRef is rewritten to
secret://[REDACTED] before the config is emitted, so a stray "billet
config" in a terminal or log never prints a live credential reference in
cleartext. A pipeline stage that needs the real value to flow through to
the next stage (or to "billet serve") must pass --redact=false
explicitly.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}

			// Redact before reporting a validation failure: a future
			// Validate() error that echoes a value must not undo --redact's
			// purpose on the failure path.
			if redact, _ := cmd.Flags().GetBool("redact"); redact {
				cfg = cfg.Redact()
			}

			if validate, _ := cmd.Flags().GetBool("validate"); validate {
				if err := cfg.Validate(); err != nil {
					return err
				}
			}

			return cfg.EncodeJSON(cmd.OutOrStdout())
		},
	}
	addConfigFlags(cmd)
	cmd.Flags().Bool("validate", false, "run BilletConfig.Validate on the resolved config and exit non-zero on failure")
	cmd.Flags().Bool("redact", true, "rewrite backend.credentialsRef to secret://[REDACTED] before emitting; pass --redact=false to keep the real value for pipeline composition")
	return cmd
}
