package k8s

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildKubectlArgs_Basic(t *testing.T) {
	cfg := &Config{
		Namespace:  "tenants",
		Context:    "",
		Deployment: "orchestrator",
		Port:       8080,
	}

	args := buildKubectlArgs(cfg, "GET", "/tenants", nil)

	assert.Contains(t, args, "exec")
	assert.Contains(t, args, "-n")
	assert.Contains(t, args, "tenants")
	assert.Contains(t, args, "deployment/orchestrator")
	assert.Contains(t, args, "wget")
	assert.Contains(t, args, "--method=GET")
	assert.Contains(t, args, "http://localhost:8080/tenants")
}

func TestBuildKubectlArgs_WithContext(t *testing.T) {
	cfg := &Config{
		Namespace:  "tenants",
		Context:    "prod-cluster",
		Deployment: "orchestrator",
		Port:       8080,
	}

	args := buildKubectlArgs(cfg, "GET", "/tenants", nil)

	assert.Equal(t, "--context", args[0])
	assert.Equal(t, "prod-cluster", args[1])
}

func TestBuildKubectlArgs_WithBody(t *testing.T) {
	cfg := &Config{
		Namespace:  "tenants",
		Deployment: "orchestrator",
		Port:       8080,
	}

	body := []byte(`{"tenant_id":"alice"}`)
	args := buildKubectlArgs(cfg, "POST", "/tenants", body)

	assert.Contains(t, args, "--header=Content-Type: application/json")
	assert.Contains(t, args, "--body-data={\"tenant_id\":\"alice\"}")
}

func TestParseResponse_Success(t *testing.T) {
	output := []byte(`{"tenant_id":"alice","status":"idle"}`)

	result, err := parseResponse(output, nil)

	assert.NoError(t, err)
	assert.Equal(t, output, result)
}

func TestParseResponse_KubectlError(t *testing.T) {
	output := []byte("Error from server (NotFound): deployments.apps \"orchestrator\" not found")

	_, err := parseResponse(output, assert.AnError)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "kubectl exec failed")
	assert.Contains(t, err.Error(), "NotFound")
}
