package registry

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MockClient is an in-memory registry for testing
type MockClient struct {
	mu      sync.RWMutex
	tenants map[string]*TenantRecord
}

func NewMock() *MockClient {
	return &MockClient{tenants: make(map[string]*TenantRecord)}
}

func (m *MockClient) GetTenant(_ context.Context, tenantID string) (*TenantRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.tenants[tenantID]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (m *MockClient) CreateTenant(_ context.Context, record *TenantRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tenants[record.TenantID]; ok {
		return &ConditionalCheckFailed{TenantID: record.TenantID}
	}
	cp := *record
	m.tenants[record.TenantID] = &cp
	return nil
}

func (m *MockClient) UpdateStatus(_ context.Context, tenantID string, status TenantStatus, podName, podIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.tenants[tenantID]
	if !ok {
		return fmt.Errorf("tenant %s not found", tenantID)
	}
	r.Status = status
	r.PodName = podName
	r.PodIP = podIP
	r.LastActiveAt = time.Now()
	return nil
}

func (m *MockClient) UpdateActivity(_ context.Context, tenantID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.tenants[tenantID]
	if !ok {
		return nil
	}
	r.LastActiveAt = time.Now()
	return nil
}

func (m *MockClient) UpdateBotToken(_ context.Context, tenantID, botToken string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.tenants[tenantID]
	if !ok {
		return &ConditionalCheckFailed{}
	}
	r.BotToken = botToken
	return nil
}

func (m *MockClient) UpdateHooksToken(_ context.Context, tenantID, hooksToken string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.tenants[tenantID]
	if !ok {
		return &ConditionalCheckFailed{}
	}
	r.HooksToken = hooksToken
	return nil
}

func (m *MockClient) UpdateWebhookSecret(_ context.Context, tenantID, secret string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.tenants[tenantID]
	if !ok {
		return &ConditionalCheckFailed{}
	}
	r.WebhookSecret = secret
	return nil
}

func (m *MockClient) UpdateIdleTimeout(_ context.Context, tenantID string, timeoutS int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.tenants[tenantID]
	if !ok {
		return &ConditionalCheckFailed{}
	}
	r.IdleTimeoutS = timeoutS
	return nil
}

func (m *MockClient) ListAll(_ context.Context) ([]*TenantRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var records []*TenantRecord
	for _, r := range m.tenants {
		cp := *r
		records = append(records, &cp)
	}
	return records, nil
}

func (m *MockClient) ListByStatus(_ context.Context, status TenantStatus) ([]*TenantRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*TenantRecord
	for _, r := range m.tenants {
		if r.Status == status {
			cp := *r
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (m *MockClient) ListIdleTenants(_ context.Context, olderThan time.Duration) ([]*TenantRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cutoff := time.Now().Add(-olderThan)
	var result []*TenantRecord
	for _, r := range m.tenants {
		if r.Status == StatusRunning && r.LastActiveAt.Before(cutoff) {
			cp := *r
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (m *MockClient) DeleteTenant(_ context.Context, tenantID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tenants, tenantID)
	return nil
}

// ConditionalCheckFailed is returned when a conditional write fails
type ConditionalCheckFailed struct {
	TenantID string
}

func (e *ConditionalCheckFailed) Error() string {
	return "tenant already exists: " + e.TenantID
}
