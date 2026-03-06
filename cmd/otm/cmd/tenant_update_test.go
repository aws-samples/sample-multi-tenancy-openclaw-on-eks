package cmd

import (
	"bytes"
	stdcontext "context"
	"testing"

	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/cli/api"
	"github.com/stretchr/testify/assert"
)

func TestTenantUpdateCommand_BotToken(t *testing.T) {
	mockClient := &api.MockClient{
		UpdateTenantFunc: func(ctx stdcontext.Context, id string, req *api.UpdateTenantRequest) (*api.Tenant, error) {
			assert.Equal(t, "alice", id)
			assert.NotNil(t, req.BotToken)
			assert.Equal(t, "new-token:456", *req.BotToken)
			assert.Nil(t, req.IdleTimeoutS)

			return &api.Tenant{
				TenantID:     id,
				Status:       "idle",
				IdleTimeoutS: 600,
			}, nil
		},
	}

	cmd := newTenantUpdateCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"alice", "--bot-token", "new-token:456"})

	err := cmd.Execute()
	assert.NoError(t, err)
}

func TestTenantUpdateCommand_IdleTimeout(t *testing.T) {
	mockClient := &api.MockClient{
		UpdateTenantFunc: func(ctx stdcontext.Context, id string, req *api.UpdateTenantRequest) (*api.Tenant, error) {
			assert.Equal(t, "alice", id)
			assert.Nil(t, req.BotToken)
			assert.NotNil(t, req.IdleTimeoutS)
			assert.Equal(t, 1800, *req.IdleTimeoutS)

			return &api.Tenant{
				TenantID:     id,
				Status:       "idle",
				IdleTimeoutS: 1800,
			}, nil
		},
	}

	cmd := newTenantUpdateCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"alice", "--idle-timeout", "1800"})

	err := cmd.Execute()
	assert.NoError(t, err)
}

func TestTenantUpdateCommand_Both(t *testing.T) {
	mockClient := &api.MockClient{
		UpdateTenantFunc: func(ctx stdcontext.Context, id string, req *api.UpdateTenantRequest) (*api.Tenant, error) {
			assert.NotNil(t, req.BotToken)
			assert.NotNil(t, req.IdleTimeoutS)
			return &api.Tenant{TenantID: id, Status: "idle"}, nil
		},
	}

	cmd := newTenantUpdateCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"alice", "--bot-token", "token", "--idle-timeout", "900"})

	err := cmd.Execute()
	assert.NoError(t, err)
}

func TestTenantUpdateCommand_NoFlags(t *testing.T) {
	mockClient := &api.MockClient{}

	cmd := newTenantUpdateCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"alice"})

	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "at least one")
}
