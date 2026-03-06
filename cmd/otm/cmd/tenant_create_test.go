package cmd

import (
	"bytes"
	stdcontext "context"
	"testing"

	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/cli/api"
	"github.com/stretchr/testify/assert"
)

func TestTenantCreateCommand(t *testing.T) {
	mockClient := &api.MockClient{
		CreateTenantFunc: func(ctx stdcontext.Context, req *api.CreateTenantRequest) (*api.Tenant, error) {
			assert.Equal(t, "alice", req.TenantID)
			assert.Equal(t, "token:123", req.BotToken)
			assert.Equal(t, 600, req.IdleTimeoutS)

			return &api.Tenant{
				TenantID:     req.TenantID,
				Status:       "idle",
				IdleTimeoutS: req.IdleTimeoutS,
			}, nil
		},
	}

	cmd := newTenantCreateCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"alice", "token:123", "--idle-timeout", "600"})

	err := cmd.Execute()
	assert.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "alice")
	assert.Contains(t, output, "idle")
}

func TestTenantCreateCommand_MissingArgs(t *testing.T) {
	mockClient := &api.MockClient{}

	cmd := newTenantCreateCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	assert.Error(t, err)
}

func TestTenantCreateCommand_DefaultTimeout(t *testing.T) {
	mockClient := &api.MockClient{
		CreateTenantFunc: func(ctx stdcontext.Context, req *api.CreateTenantRequest) (*api.Tenant, error) {
			assert.Equal(t, 600, req.IdleTimeoutS) // Default
			return &api.Tenant{TenantID: req.TenantID, Status: "idle"}, nil
		},
	}

	cmd := newTenantCreateCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"alice", "token:123"})

	err := cmd.Execute()
	assert.NoError(t, err)
}
