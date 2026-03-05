package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMockClient_CreateTenant(t *testing.T) {
	mock := &MockClient{
		CreateTenantFunc: func(ctx context.Context, req *CreateTenantRequest) (*Tenant, error) {
			return &Tenant{
				TenantID:     req.TenantID,
				Status:       "idle",
				IdleTimeoutS: req.IdleTimeoutS,
			}, nil
		},
	}

	tenant, err := mock.CreateTenant(context.Background(), &CreateTenantRequest{
		TenantID:     "test",
		BotToken:     "token",
		IdleTimeoutS: 600,
	})

	assert.NoError(t, err)
	assert.Equal(t, "test", tenant.TenantID)
	assert.Equal(t, "idle", tenant.Status)
}
