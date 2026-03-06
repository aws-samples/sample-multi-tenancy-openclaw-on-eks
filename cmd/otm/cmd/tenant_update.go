package cmd

import (
	stdcontext "context"
	"fmt"
	"time"

	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/cli/api"
	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/cli/output"
	"github.com/spf13/cobra"
)

var (
	updateBotToken    string
	updateIdleTimeout int
	updateBotTokenSet bool
	updateTimeoutSet  bool
)

func newTenantUpdateCmd(client api.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <tenant-id>",
		Short: "Update tenant configuration",
		Long: `Update bot token and/or idle timeout for an existing tenant.

At least one of --bot-token or --idle-timeout must be specified.`,
		Args: cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			updateBotTokenSet = cmd.Flags().Changed("bot-token")
			updateTimeoutSet = cmd.Flags().Changed("idle-timeout")

			if !updateBotTokenSet && !updateTimeoutSet {
				return fmt.Errorf("at least one of --bot-token or --idle-timeout must be specified")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			tenantID := args[0]
			styler := output.NewStyler(noColor)
			styler.PrintInfo(fmt.Sprintf("Updating tenant '%s'...", tenantID))

			req := &api.UpdateTenantRequest{}
			if updateBotTokenSet {
				req.BotToken = &updateBotToken
			}
			if updateTimeoutSet {
				req.IdleTimeoutS = &updateIdleTimeout
			}

			ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 30*time.Second)
			defer cancel()

			tenant, err := client.UpdateTenant(ctx, tenantID, req)
			if err != nil {
				styler.PrintError(fmt.Sprintf("Failed to update tenant: %v", err))
				return err
			}

			styler.PrintSuccess(fmt.Sprintf("Tenant '%s' updated", tenantID))

			// Format output
			if outputFormat == "json" {
				jsonStr, err := output.FormatJSON(tenant)
				if err != nil {
					return fmt.Errorf("failed to format output: %w", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), jsonStr)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "\nTenant ID:     %s\n", tenant.TenantID)
				fmt.Fprintf(cmd.OutOrStdout(), "Status:        %s\n", tenant.Status)
				fmt.Fprintf(cmd.OutOrStdout(), "Idle Timeout:  %ds\n", tenant.IdleTimeoutS)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&updateBotToken, "bot-token", "", "New Telegram bot token")
	cmd.Flags().IntVar(&updateIdleTimeout, "idle-timeout", 0, "New idle timeout in seconds")

	return cmd
}
