package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "otm %s\n", version)
			fmt.Fprintf(cmd.OutOrStdout(), "commit: %s\n", commit)
			fmt.Fprintf(cmd.OutOrStdout(), "built: %s\n", buildDate)
			fmt.Fprintf(cmd.OutOrStdout(), "go: %s\n", runtime.Version())
		},
	}
}

func init() {
	rootCmd.AddCommand(newVersionCmd())
}
