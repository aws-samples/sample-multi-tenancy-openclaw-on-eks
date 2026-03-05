package cmd

import (
	"bytes"
	stdcontext "context"
	"testing"

	"github.com/shawn/openclaw-tenancy/internal/cli/api"
	"github.com/stretchr/testify/assert"
)

func TestTenantListCommand(t *testing.T) {
	mockClient := &api.MockClient{
		ListTenantsFunc: func(ctx stdcontext.Context) ([]api.Tenant, error) {
			return []api.Tenant{
				{TenantID: "alice", Status: "running", IdleTimeoutS: 3600},
				{TenantID: "bob", Status: "idle", IdleTimeoutS: 600},
			}, nil
		},
	}

	cmd := newTenantListCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	assert.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "alice")
	assert.Contains(t, output, "bob")
	assert.Contains(t, output, "running")
	assert.Contains(t, output, "idle")
}

func TestTenantGetCommand(t *testing.T) {
	mockClient := &api.MockClient{
		GetTenantFunc: func(ctx stdcontext.Context, id string) (*api.Tenant, error) {
			assert.Equal(t, "alice", id)
			return &api.Tenant{
				TenantID:     "alice",
				Status:       "running",
				IdleTimeoutS: 3600,
				PodIP:        "10.0.1.5",
			}, nil
		},
	}

	cmd := newTenantGetCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"alice"})

	err := cmd.Execute()
	assert.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "alice")
	assert.Contains(t, output, "running")
	assert.Contains(t, output, "10.0.1.5")
}

func TestTenantDeleteCommand(t *testing.T) {
	deleted := false
	mockClient := &api.MockClient{
		DeleteTenantFunc: func(ctx stdcontext.Context, id string) error {
			assert.Equal(t, "alice", id)
			deleted = true
			return nil
		},
	}

	cmd := newTenantDeleteCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"alice"})

	err := cmd.Execute()
	assert.NoError(t, err)
	assert.True(t, deleted)

	output := buf.String()
	assert.Contains(t, output, "deleted")
}
