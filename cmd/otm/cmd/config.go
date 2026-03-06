package cmd

import (
	stdcontext "context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/cli/api"
	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/cli/output"
	"github.com/spf13/cobra"
)

func newConfigCmd(client api.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage orchestrator runtime configuration",
		Long:  `Get or set orchestrator runtime configuration such as warm pool target.`,
	}

	cmd.AddCommand(newConfigGetCmd(client))
	cmd.AddCommand(newConfigSetCmd(client))

	return cmd
}

func newConfigGetCmd(client api.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Get current runtime configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			styler := output.NewStyler(noColor)
			ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 15*time.Second)
			defer cancel()

			cfg, err := client.GetConfig(ctx)
			if err != nil {
				styler.PrintError(fmt.Sprintf("Failed to get config: %v", err))
				return err
			}

			if outputFormat == "json" {
				jsonStr, err := output.FormatJSON(cfg)
				if err != nil {
					return fmt.Errorf("failed to format output: %w", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), jsonStr)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Warm Pool Target:  %d\n", cfg.WarmPoolTarget)
			}

			return nil
		},
	}
}

func newConfigSetCmd(client api.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a runtime configuration value",
		Long: `Set a runtime configuration value. Supported keys:
  warm-pool-target  Number of warm pool pods to maintain`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			value := args[1]
			styler := output.NewStyler(noColor)

			ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 15*time.Second)
			defer cancel()

			switch key {
			case "warm-pool-target":
				target, err := strconv.Atoi(value)
				if err != nil {
					styler.PrintError(fmt.Sprintf("Invalid value for warm-pool-target: %s", value))
					return fmt.Errorf("invalid value: %w", err)
				}
				if target < 0 {
					styler.PrintError("warm-pool-target must be >= 0")
					return fmt.Errorf("warm-pool-target must be >= 0")
				}

				cfg, err := client.UpdateConfig(ctx, &api.RuntimeConfig{
					WarmPoolTarget: target,
				})
				if err != nil {
					styler.PrintError(fmt.Sprintf("Failed to update config: %v", err))
					return err
				}

				styler.PrintSuccess(fmt.Sprintf("warm-pool-target set to %d", cfg.WarmPoolTarget))

				if outputFormat == "json" {
					jsonStr, err := output.FormatJSON(cfg)
					if err != nil {
						return fmt.Errorf("failed to format output: %w", err)
					}
					fmt.Fprintln(cmd.OutOrStdout(), jsonStr)
				}
			default:
				styler.PrintError(fmt.Sprintf("Unknown config key: %s", key))
				return fmt.Errorf("unknown config key: %s (supported: warm-pool-target)", key)
			}

			return nil
		},
	}
}
