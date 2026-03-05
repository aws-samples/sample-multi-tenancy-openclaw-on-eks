package api

import (
	"context"
)

// Client is the interface for interacting with the Orchestrator API
type Client interface {
	// Tenant lifecycle
	CreateTenant(ctx context.Context, req *CreateTenantRequest) (*Tenant, error)
	DeleteTenant(ctx context.Context, id string) error
	ListTenants(ctx context.Context) ([]Tenant, error)
	GetTenant(ctx context.Context, id string) (*Tenant, error)
	UpdateTenant(ctx context.Context, id string, req *UpdateTenantRequest) (*Tenant, error)

	// Pod operations
	ProbeGateway(ctx context.Context, tenantID string) (*ProbeResponse, error)

	// Runtime config
	GetConfig(ctx context.Context) (*RuntimeConfig, error)
	UpdateConfig(ctx context.Context, cfg *RuntimeConfig) (*RuntimeConfig, error)
}
