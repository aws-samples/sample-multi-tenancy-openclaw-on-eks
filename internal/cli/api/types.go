package api

import (
	"time"
)

type Tenant struct {
	TenantID     string    `json:"tenant_id"`
	Status       string    `json:"status"`
	IdleTimeoutS int       `json:"idle_timeout_s"`
	PodName      string    `json:"pod_name,omitempty"`
	PodIP        string    `json:"pod_ip,omitempty"`
	LastActiveAt time.Time `json:"last_active_at,omitempty"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
	// Secrets (BotToken, HooksToken) are never populated in list/get responses — redacted server-side
}

type CreateTenantRequest struct {
	TenantID     string `json:"tenant_id"`
	BotToken     string `json:"bot_token"`
	IdleTimeoutS int    `json:"idle_timeout_s"`
}

type UpdateTenantRequest struct {
	BotToken     *string `json:"bot_token,omitempty"`
	IdleTimeoutS *int    `json:"idle_timeout_s,omitempty"`
}

// ProbeResponse is returned by the pod probe command
type ProbeResponse struct {
	TenantID string `json:"tenant_id"`
	PodIP    string `json:"pod_ip"`
	Healthy  bool   `json:"healthy"`
	Message  string `json:"message,omitempty"`
}

// RuntimeConfig represents hot-configurable orchestrator settings
type RuntimeConfig struct {
	WarmPoolTarget int `json:"warm_pool_target"`
}
