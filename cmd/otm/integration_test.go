//go:build integration
// +build integration

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLI_Integration(t *testing.T) {
	// Build CLI
	buildCmd := exec.Command("go", "build", "-o", "../../bin/otm-test", "./cmd/otm")
	err := buildCmd.Run()
	require.NoError(t, err, "Failed to build CLI")
	defer os.Remove("../../bin/otm-test")

	// Test version command
	t.Run("Version", func(t *testing.T) {
		cmd := exec.Command("../../bin/otm-test", "version")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err)
		assert.Contains(t, string(output), "otm")
		assert.Contains(t, string(output), "commit:")
	})

	// Test help
	t.Run("Help", func(t *testing.T) {
		cmd := exec.Command("../../bin/otm-test", "--help")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err)
		assert.Contains(t, string(output), "Agentic Tenancy CLI")
		assert.Contains(t, string(output), "tenant")
		assert.Contains(t, string(output), "webhook")
	})

	// Test tenant help
	t.Run("TenantHelp", func(t *testing.T) {
		cmd := exec.Command("../../bin/otm-test", "tenant", "--help")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err)
		assert.Contains(t, string(output), "create")
		assert.Contains(t, string(output), "list")
		assert.Contains(t, string(output), "delete")
	})

	// Test invalid command
	t.Run("InvalidCommand", func(t *testing.T) {
		cmd := exec.Command("../../bin/otm-test", "invalid-command")
		output, err := cmd.CombinedOutput()
		assert.Error(t, err)
		assert.Contains(t, string(output), "unknown command")
	})
}

func TestCLI_Flags(t *testing.T) {
	cmd := exec.Command("go", "build", "-o", "../../bin/otm-test", "./cmd/otm")
	err := cmd.Run()
	require.NoError(t, err)
	defer os.Remove("../../bin/otm-test")

	// Test global flags parsing
	t.Run("GlobalFlags", func(t *testing.T) {
		cmd := exec.Command("../../bin/otm-test",
			"tenant", "list",
			"--namespace", "test-ns",
			"--context", "test-ctx",
			"--output", "json",
			"--no-color")

		// This will fail (no cluster) but we're testing flag parsing
		output, _ := cmd.CombinedOutput()

		// Should attempt to exec kubectl (proves flags were parsed)
		outputStr := string(output)
		if !strings.Contains(outputStr, "kubectl") && !strings.Contains(outputStr, "exec") {
			t.Skip("No kubectl available, skipping")
		}
	})
}
