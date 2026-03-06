package cmd

import (
	"os"

	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/cli/api"
	"github.com/spf13/cobra"
)

var (
	version   string
	commit    string
	buildDate string

	// Global flags
	namespace       string
	context         string
	orchestratorURL string
	outputFormat    string
	noColor         bool
)

var rootCmd = &cobra.Command{
	Use:   "otm",
	Short: "OpenClaw Tenancy CLI",
	Long: `otm is a CLI for managing multi-tenant OpenClaw agent instances.

It provides commands to create, list, update, and delete tenants,
as well as probe tenant pod health.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	// Global flags
	rootCmd.PersistentFlags().StringVar(&namespace, "namespace", getEnvOrDefault("OTM_NAMESPACE", "tenants"), "Kubernetes namespace")
	rootCmd.PersistentFlags().StringVar(&context, "context", os.Getenv("OTM_KUBE_CONTEXT"), "kubectl context")
	rootCmd.PersistentFlags().StringVar(&orchestratorURL, "orchestrator-url", os.Getenv("OTM_ORCHESTRATOR_URL"), "Orchestrator HTTP URL (bypasses kubectl)")
	rootCmd.PersistentFlags().StringVar(&outputFormat, "output", "table", "Output format: json|table")
	rootCmd.PersistentFlags().BoolVar(&noColor, "no-color", false, "Disable colored output")
}

func initClient() api.Client {
	// For now, always use kubectl client
	// HTTP client can be added later when --orchestrator-url is provided
	return api.NewKubectlClient(namespace, context)
}

func Execute() error {
	// Wire up client for all commands
	client := initClient()

	// Add command groups with client
	rootCmd.AddCommand(newTenantCmd(client))
	rootCmd.AddCommand(newPodCmd(client))
	rootCmd.AddCommand(newConfigCmd(client))
	rootCmd.AddCommand(newLogsCmd())

	return rootCmd.Execute()
}

func SetVersion(v, c, d string) {
	version = v
	commit = c
	buildDate = d
}

func getEnvOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
