// Package k8s provides utilities for executing kubectl commands to interact
// with Kubernetes deployments.
package k8s

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Config holds the kubectl execution configuration.
type Config struct {
	Namespace  string
	Context    string
	Deployment string
	Port       int
}

// ExecAPICall executes a kubectl exec command to call an API endpoint on a deployment.
func ExecAPICall(ctx context.Context, cfg *Config, method, path string, body []byte) ([]byte, error) {
	args := buildKubectlArgs(cfg, method, path, body)

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	output, err := cmd.CombinedOutput()

	return parseResponse(output, err)
}

func buildKubectlArgs(cfg *Config, method, path string, body []byte) []string {
	var args []string

	// Add context if specified
	if cfg.Context != "" {
		args = append(args, "--context", cfg.Context)
	}

	// kubectl exec command
	args = append(args,
		"exec",
		"-n", cfg.Namespace,
		fmt.Sprintf("deployment/%s", cfg.Deployment),
		"--",
		"wget",
		"-qO-",
		fmt.Sprintf("--method=%s", method),
	)

	// Add headers and body for POST/PATCH/PUT
	if body != nil && len(body) > 0 {
		args = append(args,
			"--header=Content-Type: application/json",
			fmt.Sprintf("--body-data=%s", string(body)),
		)
	}

	// Target URL (pod-local)
	url := fmt.Sprintf("http://localhost:%d%s", cfg.Port, path)
	args = append(args, url)

	return args
}

func parseResponse(output []byte, execErr error) ([]byte, error) {
	if execErr != nil {
		// kubectl exec failed
		errMsg := string(output)
		if strings.Contains(errMsg, "not found") {
			return nil, fmt.Errorf("kubectl exec failed: deployment not found. Make sure the deployment is running and namespace is correct.\n%s", errMsg)
		}
		if strings.Contains(errMsg, "No such file or directory") {
			return nil, fmt.Errorf("kubectl not found in PATH. Please install kubectl")
		}
		return nil, fmt.Errorf("kubectl exec failed: %w\n%s", execErr, errMsg)
	}

	return output, nil
}

// NewConfig creates a new Config.
func NewConfig(namespace, context, deployment string, port int) *Config {
	return &Config{
		Namespace:  namespace,
		Context:    context,
		Deployment: deployment,
		Port:       port,
	}
}
