package cmd

import (
	stdcontext "context"
	"fmt"
	"time"

	"github.com/shawn/openclaw-tenancy/internal/cli/api"
	"github.com/shawn/openclaw-tenancy/internal/cli/output"
	"github.com/spf13/cobra"
)

var idleTimeout int

func newTenantCreateCmd(client api.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <tenant-id> <bot-token>",
		Short: "Create a new tenant",
		Long: `Create a new tenant with the specified ID and Telegram bot token.

The tenant will be registered in DynamoDB and the Telegram webhook will
be auto-registered if ROUTER_PUBLIC_URL is configured on the orchestrator.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			tenantID := args[0]
			botToken := args[1]

			styler := output.NewStyler(noColor)
			styler.PrintInfo(fmt.Sprintf("Creating tenant '%s'...", tenantID))

			ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 30*time.Second)
			defer cancel()

			tenant, err := client.CreateTenant(ctx, &api.CreateTenantRequest{
				TenantID:     tenantID,
				BotToken:     botToken,
				IdleTimeoutS: idleTimeout,
			})
			if err != nil {
				styler.PrintError(fmt.Sprintf("Failed to create tenant: %v", err))
				return err
			}

			styler.PrintSuccess(fmt.Sprintf("Tenant '%s' created", tenantID))

			// Format output
			if outputFormat == "json" {
				jsonStr, err := output.FormatJSON(tenant)
				if err != nil {
					return fmt.Errorf("failed to format output: %w", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), jsonStr)
			} else {
				// Table format
				fmt.Fprintf(cmd.OutOrStdout(), "\nTenant ID:     %s\n", tenant.TenantID)
				fmt.Fprintf(cmd.OutOrStdout(), "Status:        %s\n", tenant.Status)
				fmt.Fprintf(cmd.OutOrStdout(), "Idle Timeout:  %ds\n", tenant.IdleTimeoutS)
				if !tenant.CreatedAt.IsZero() {
					fmt.Fprintf(cmd.OutOrStdout(), "Created At:    %s\n", tenant.CreatedAt.Format(time.RFC3339))
				}
			}

			return nil
		},
	}

	cmd.Flags().IntVar(&idleTimeout, "idle-timeout", 600, "Idle timeout in seconds")

	return cmd
}
