package cmd

import (
	stdcontext "context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/shawn/openclaw-tenancy/internal/cli/api"
	"github.com/shawn/openclaw-tenancy/internal/cli/output"
	"github.com/spf13/cobra"
)

func newTenantListCmd(client api.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all tenants",
		RunE: func(cmd *cobra.Command, args []string) error {
			styler := output.NewStyler(noColor)
			styler.FprintInfo(cmd.OutOrStdout(), "Listing tenants...")

			ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 30*time.Second)
			defer cancel()

			tenants, err := client.ListTenants(ctx)
			if err != nil {
				styler.FprintError(cmd.OutOrStderr(), fmt.Sprintf("Failed to list tenants: %v", err))
				return err
			}

			if outputFormat == "json" {
				jsonStr, err := output.FormatJSON(tenants)
				if err != nil {
					return fmt.Errorf("failed to format output: %w", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), jsonStr)
				return nil
			}

			// Table format
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TENANT ID\tSTATUS\tLAST ACTIVE\tIDLE TIMEOUT")
			for _, t := range tenants {
				lastActive := "never"
				if !t.LastActiveAt.IsZero() {
					lastActive = t.LastActiveAt.Format("2006-01-02 15:04:05")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%ds\n", t.TenantID, t.Status, lastActive, t.IdleTimeoutS)
			}
			w.Flush()

			return nil
		},
	}
}

func newTenantGetCmd(client api.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "get <tenant-id>",
		Short: "Get tenant details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tenantID := args[0]

			ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 30*time.Second)
			defer cancel()

			tenant, err := client.GetTenant(ctx, tenantID)
			if err != nil {
				styler := output.NewStyler(noColor)
				styler.FprintError(cmd.OutOrStderr(), fmt.Sprintf("Failed to get tenant: %v", err))
				return err
			}

			if outputFormat == "json" {
				jsonStr, err := output.FormatJSON(tenant)
				if err != nil {
					return fmt.Errorf("failed to format output: %w", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), jsonStr)
				return nil
			}

			// Table format
			fmt.Fprintf(cmd.OutOrStdout(), "Tenant ID:     %s\n", tenant.TenantID)
			fmt.Fprintf(cmd.OutOrStdout(), "Status:        %s\n", tenant.Status)
			fmt.Fprintf(cmd.OutOrStdout(), "Idle Timeout:  %ds\n", tenant.IdleTimeoutS)
			if tenant.PodName != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Pod Name:      %s\n", tenant.PodName)
			}
			if tenant.PodIP != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Pod IP:        %s\n", tenant.PodIP)
			}
			if !tenant.LastActiveAt.IsZero() {
				fmt.Fprintf(cmd.OutOrStdout(), "Last Active:   %s\n", tenant.LastActiveAt.Format(time.RFC3339))
			}
			if !tenant.CreatedAt.IsZero() {
				fmt.Fprintf(cmd.OutOrStdout(), "Created At:    %s\n", tenant.CreatedAt.Format(time.RFC3339))
			}

			return nil
		},
	}
}

func newTenantDeleteCmd(client api.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <tenant-id>",
		Short: "Delete a tenant",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tenantID := args[0]
			styler := output.NewStyler(noColor)
			styler.FprintInfo(cmd.OutOrStdout(), fmt.Sprintf("Deleting tenant '%s'...", tenantID))

			ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 30*time.Second)
			defer cancel()

			err := client.DeleteTenant(ctx, tenantID)
			if err != nil {
				styler.FprintError(cmd.OutOrStderr(), fmt.Sprintf("Failed to delete tenant: %v", err))
				return err
			}

			styler.FprintSuccess(cmd.OutOrStdout(), fmt.Sprintf("Tenant '%s' deleted", tenantID))
			return nil
		},
	}
}
