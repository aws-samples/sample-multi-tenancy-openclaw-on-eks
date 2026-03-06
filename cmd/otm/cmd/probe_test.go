package cmd

import (
	"bytes"
	stdcontext "context"
	"testing"

	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/cli/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPodProbeCommand_Healthy(t *testing.T) {
	mockClient := &api.MockClient{
		ProbeGatewayFunc: func(ctx stdcontext.Context, tenantID string) (*api.ProbeResponse, error) {
			assert.Equal(t, "alice", tenantID)
			return &api.ProbeResponse{
				TenantID: tenantID,
				PodIP:    "10.0.0.42",
				Healthy:  true,
				Message:  "HTTP 200",
			}, nil
		},
	}

	cmd := newPodProbeCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"alice"})

	err := cmd.Execute()
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "10.0.0.42:18789")
}

func TestPodProbeCommand_Unhealthy(t *testing.T) {
	mockClient := &api.MockClient{
		ProbeGatewayFunc: func(ctx stdcontext.Context, tenantID string) (*api.ProbeResponse, error) {
			return &api.ProbeResponse{
				TenantID: tenantID,
				PodIP:    "10.0.0.42",
				Healthy:  false,
				Message:  "healthz unreachable: connection refused",
			}, nil
		},
	}

	cmd := newPodProbeCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"bob"})

	err := cmd.Execute()
	assert.Error(t, err, "unhealthy probe should return error")
}
