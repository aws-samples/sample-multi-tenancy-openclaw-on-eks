package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/cli/k8s"
)

type KubectlClient struct {
	orchestratorCfg *k8s.Config
}

func NewKubectlClient(namespace, context string) *KubectlClient {
	return &KubectlClient{
		orchestratorCfg: k8s.NewConfig(namespace, context, "orchestrator", 8080),
	}
}

func (c *KubectlClient) CreateTenant(ctx context.Context, req *CreateTenantRequest) (*Tenant, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	resp, err := k8s.ExecAPICall(ctx, c.orchestratorCfg, "POST", "/tenants", body)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	var tenant Tenant
	if err := json.Unmarshal(resp, &tenant); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return &tenant, nil
}

func (c *KubectlClient) DeleteTenant(ctx context.Context, id string) error {
	_, err := k8s.ExecAPICall(ctx, c.orchestratorCfg, "DELETE", fmt.Sprintf("/tenants/%s", id), nil)
	return err
}

func (c *KubectlClient) ListTenants(ctx context.Context) ([]Tenant, error) {
	resp, err := k8s.ExecAPICall(ctx, c.orchestratorCfg, "GET", "/tenants", nil)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	var tenants []Tenant
	if err := json.Unmarshal(resp, &tenants); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return tenants, nil
}

func (c *KubectlClient) GetTenant(ctx context.Context, id string) (*Tenant, error) {
	resp, err := k8s.ExecAPICall(ctx, c.orchestratorCfg, "GET", fmt.Sprintf("/tenants/%s", id), nil)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	var tenant Tenant
	if err := json.Unmarshal(resp, &tenant); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return &tenant, nil
}

func (c *KubectlClient) UpdateTenant(ctx context.Context, id string, req *UpdateTenantRequest) (*Tenant, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	resp, err := k8s.ExecAPICall(ctx, c.orchestratorCfg, "PATCH", fmt.Sprintf("/tenants/%s", id), body)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	var tenant Tenant
	if err := json.Unmarshal(resp, &tenant); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return &tenant, nil
}

// ProbeGateway checks the OpenClaw Gateway healthz endpoint on the tenant pod (port 18789).
// It fetches the pod IP from the Orchestrator, then calls /healthz directly.
func (c *KubectlClient) ProbeGateway(ctx context.Context, tenantID string) (*ProbeResponse, error) {
	tenant, err := c.GetTenant(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("get tenant: %w", err)
	}
	result := &ProbeResponse{TenantID: tenantID, PodIP: tenant.PodIP}

	if tenant.PodIP == "" {
		result.Healthy = false
		result.Message = fmt.Sprintf("tenant is %s — no pod IP available", tenant.Status)
		return result, nil
	}

	url := fmt.Sprintf("http://%s:18789/healthz", tenant.PodIP)
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(url)
	if err != nil {
		result.Healthy = false
		result.Message = fmt.Sprintf("healthz unreachable: %v", err)
		return result, nil
	}
	defer resp.Body.Close()

	result.Healthy = resp.StatusCode == http.StatusOK
	result.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
	return result, nil
}

// GetConfig retrieves the current runtime configuration from the orchestrator.
func (c *KubectlClient) GetConfig(ctx context.Context) (*RuntimeConfig, error) {
	resp, err := k8s.ExecAPICall(ctx, c.orchestratorCfg, "GET", "/config", nil)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	var cfg RuntimeConfig
	if err := json.Unmarshal(resp, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return &cfg, nil
}

// UpdateConfig updates the runtime configuration on the orchestrator.
func (c *KubectlClient) UpdateConfig(ctx context.Context, cfg *RuntimeConfig) (*RuntimeConfig, error) {
	body, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	resp, err := k8s.ExecAPICall(ctx, c.orchestratorCfg, "PUT", "/config", body)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	var result RuntimeConfig
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return &result, nil
}
