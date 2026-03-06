package cmd

import (
	stdcontext "context"
	"fmt"
	"time"

	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/cli/api"
	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/cli/output"
	"github.com/spf13/cobra"
)

func newPodProbeCmd(client api.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "probe <tenant-id>",
		Short: "Check OpenClaw Gateway health for a tenant pod",
		Long: `Check the OpenClaw Gateway /healthz endpoint (port 18789) for a tenant pod.

Returns healthy if the pod is running and the Gateway is accepting requests.
Useful for debugging cold-start issues or confirming a pod is ready.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tenantID := args[0]
			styler := output.NewStyler(noColor)
			styler.PrintInfo(fmt.Sprintf("Probing gateway for tenant '%s'...", tenantID))

			ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 15*time.Second)
			defer cancel()

			resp, err := client.ProbeGateway(ctx, tenantID)
			if err != nil {
				styler.PrintError(fmt.Sprintf("Probe failed: %v", err))
				return err
			}

			if outputFormat == "json" {
				jsonStr, err := output.FormatJSON(resp)
				if err != nil {
					return fmt.Errorf("failed to format output: %w", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), jsonStr)
				return nil
			}

			if resp.Healthy {
				styler.PrintSuccess(fmt.Sprintf("Gateway healthy — %s", resp.Message))
			} else {
				styler.PrintError(fmt.Sprintf("Gateway unhealthy — %s", resp.Message))
			}
			if resp.PodIP != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Pod IP:   %s:18789\n", resp.PodIP)
			}

			if !resp.Healthy {
				return fmt.Errorf("gateway unhealthy")
			}
			return nil
		},
	}
}

func newPodCmd(client api.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pod",
		Short: "Inspect tenant pods",
		Long:  `Commands to inspect and probe OpenClaw tenant pods.`,
	}
	cmd.AddCommand(newPodProbeCmd(client))
	return cmd
}
