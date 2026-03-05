package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKubectlClient_BuildCreateTenantBody(t *testing.T) {
	req := &CreateTenantRequest{
		TenantID:     "alice",
		BotToken:     "token:123",
		IdleTimeoutS: 600,
	}

	body, err := json.Marshal(req)
	assert.NoError(t, err)

	var decoded CreateTenantRequest
	err = json.Unmarshal(body, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, "alice", decoded.TenantID)
	assert.Equal(t, "token:123", decoded.BotToken)
	assert.Equal(t, 600, decoded.IdleTimeoutS)
}

func TestKubectlClient_BuildUpdateTenantBody(t *testing.T) {
	newToken := "new-token"
	newTimeout := 1800

	req := &UpdateTenantRequest{
		BotToken:     &newToken,
		IdleTimeoutS: &newTimeout,
	}

	body, err := json.Marshal(req)
	assert.NoError(t, err)

	var decoded UpdateTenantRequest
	err = json.Unmarshal(body, &decoded)
	assert.NoError(t, err)
	assert.NotNil(t, decoded.BotToken)
	assert.Equal(t, "new-token", *decoded.BotToken)
	assert.NotNil(t, decoded.IdleTimeoutS)
	assert.Equal(t, 1800, *decoded.IdleTimeoutS)
}

func TestKubectlClient_ParseTenant(t *testing.T) {
	responseJSON := `{
		"tenant_id": "alice",
		"status": "running",
		"idle_timeout_s": 3600,
		"pod_name": "alice",
		"pod_ip": "10.0.1.5"
	}`

	var tenant Tenant
	err := json.Unmarshal([]byte(responseJSON), &tenant)
	assert.NoError(t, err)
	assert.Equal(t, "alice", tenant.TenantID)
	assert.Equal(t, "running", tenant.Status)
	assert.Equal(t, 3600, tenant.IdleTimeoutS)
}
