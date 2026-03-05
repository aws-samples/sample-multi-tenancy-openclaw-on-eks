package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// buildLogsArgs constructs the kubectl logs argument list from the given parameters.
func buildLogsArgs(namespace, context, podName, container string, tail int64, follow, previous bool) []string {
	args := []string{"logs"}

	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	if context != "" {
		args = append(args, "--context", context)
	}

	args = append(args, podName)

	if container != "" {
		args = append(args, "-c", container)
	}
	if follow {
		args = append(args, "-f")
	}
	if previous {
		args = append(args, "--previous")
	}
	if tail > 0 {
		args = append(args, "--tail", fmt.Sprintf("%d", tail))
	}

	return args
}

func newLogsCmd() *cobra.Command {
	var container string
	var tail int64
	var follow bool
	var previous bool

	cmd := &cobra.Command{
		Use:   "logs <tenant-id>",
		Short: "Stream logs from a tenant pod",
		Long: `Stream logs from the OpenClaw tenant pod.

By default streams the openclaw container with follow mode.
Uses kubectl directly — requires kubectl to be on your PATH.`,
		Example: `  otm logs shawn                        # default: openclaw container, follow mode
  otm logs shawn -c s3-sync             # specific container
  otm logs shawn -c s3-restore          # init container
  otm logs shawn --tail 100             # last 100 lines, no follow
  otm logs shawn --no-follow            # don't stream, just dump
  otm logs shawn --previous             # previous terminated container`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tenantID := args[0]
			podName := tenantID

			kubectlArgs := buildLogsArgs(namespace, context, podName, container, tail, follow, previous)

			kubectl := exec.Command("kubectl", kubectlArgs...)
			kubectl.Stdin = os.Stdin
			kubectl.Stdout = os.Stdout
			kubectl.Stderr = os.Stderr

			return kubectl.Run()
		},
	}

	cmd.Flags().StringVarP(&container, "container", "c", "openclaw", "Container name")
	cmd.Flags().Int64Var(&tail, "tail", 0, "Number of recent lines to show (0 = all)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", true, "Follow log output")
	cmd.Flags().BoolVar(&previous, "previous", false, "Show logs from previous terminated container")

	return cmd
}
