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
non-zero on failure.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}

			if validate, _ := cmd.Flags().GetBool("validate"); validate {
				if err := cfg.Validate(); err != nil {
					return err
				}
			}

			if redact, _ := cmd.Flags().GetBool("redact"); redact {
				cfg = cfg.Redact()
			}

			return cfg.EncodeJSON(cmd.OutOrStdout())
		},
	}
	addConfigFlags(cmd)
	cmd.Flags().Bool("validate", false, "run BilletConfig.Validate on the resolved config and exit non-zero on failure")
	cmd.Flags().Bool("redact", false, "rewrite backend.credentialsRef to secret://[REDACTED] before emitting")
	return cmd
}
