package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shawn/openclaw-tenancy/internal/api"
	k8sclient "github.com/shawn/openclaw-tenancy/internal/k8s"
	"github.com/shawn/openclaw-tenancy/internal/lock"
	"github.com/shawn/openclaw-tenancy/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestHandler(t *testing.T) (*api.Handler, *registry.MockClient, *lock.MockLocker, *fake.Clientset) {
	t.Helper()
	reg := registry.NewMock()
	locker := lock.NewMock()
	cs := fake.NewSimpleClientset()
	k8s := k8sclient.New(cs, k8sclient.Config{
		KataRuntimeClass: "kata-qemu",
		OpenClawImage:    "openclaw:test",
		S3Bucket:         "test-bucket",
	})
	h := api.New(reg, k8s, locker, nil, nil, api.Config{
		Namespace:    "tenants",
		PodReadyWait: 5 * time.Second,
	})
	return h, reg, locker, cs
}

// simulatePodReady makes a fake pod appear as Running with an IP
func simulatePodReady(cs *fake.Clientset, tenantID, namespace, ip string) {
	go func() {
		time.Sleep(100 * time.Millisecond)
		podName := tenantID
		pod, err := cs.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			return
		}
		pod.Status.Phase = corev1.PodRunning
		pod.Status.PodIP = ip
		cs.CoreV1().Pods(namespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
	}()
}

func TestHealthz(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestCreateTenant(t *testing.T) {
	h, reg, _, _ := newTestHandler(t)

	body, _ := json.Marshal(map[string]interface{}{
		"tenant_id":      "tenant-001",
		"idle_timeout_s": 300,
	})
	req := httptest.NewRequest(http.MethodPost, "/tenants", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)

	// Verify registry has HooksToken set
	tenant, err := reg.GetTenant(context.Background(), "tenant-001")
	require.NoError(t, err)
	require.NotNil(t, tenant)
	assert.Equal(t, registry.StatusIdle, tenant.Status)
	assert.NotEmpty(t, tenant.HooksToken, "HooksToken must be auto-generated")

	// Verify CreateTenant response does NOT expose secrets
	var resp map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&resp)
	assert.Empty(t, resp["bot_token"], "bot_token must be redacted in response")
	assert.Empty(t, resp["hooks_token"], "hooks_token must be redacted in response")
}


func TestCreateTenant_InvalidTenantID(t *testing.T) {
	h, _, _, _ := newTestHandler(t)

	tests := []struct {
		name     string
		tenantID string
	}{
		{"empty", ""},
		{"uppercase", "UPPERCASE"},
		{"starts-with-dash", "-starts-with-dash"},
		{"ends-with-dash", "ends-with-dash-"},
		{"has spaces", "has spaces"},
		{"has dots", "has.dots"},
		{"has underscores", "has_underscores"},
		{"too long", "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz0123"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]interface{}{"tenant_id": tc.tenantID})
			req := httptest.NewRequest(http.MethodPost, "/tenants", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.Router().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "tenant_id=%q should be rejected", tc.tenantID)
		})
	}
}

func TestCreateTenant_ValidTenantID(t *testing.T) {
	h, _, _, _ := newTestHandler(t)

	validIDs := []string{"shawn", "tenant-1", "a", "abc-def-123"}
	for _, id := range validIDs {
		t.Run(id, func(t *testing.T) {
			body, _ := json.Marshal(map[string]interface{}{"tenant_id": id})
			req := httptest.NewRequest(http.MethodPost, "/tenants", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.Router().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusCreated, rec.Code, "tenant_id=%q should be accepted", id)
		})
	}
}

func TestGetHooksToken(t *testing.T) {
	h, reg, _, _ := newTestHandler(t)

	// Create tenant with known hooks token
	reg.CreateTenant(context.Background(), &registry.TenantRecord{
		TenantID:   "hooks-tenant",
		Status:     registry.StatusIdle,
		Namespace:  "tenants",
		HooksToken: "super-secret-token",
	})

	req := httptest.NewRequest(http.MethodGet, "/tenants/hooks-tenant/hooks_token", nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	assert.Equal(t, "super-secret-token", resp["HooksToken"])
}

func TestGetTenant_SecretsRedacted(t *testing.T) {
	h, reg, _, _ := newTestHandler(t)

	reg.CreateTenant(context.Background(), &registry.TenantRecord{
		TenantID:   "redact-tenant",
		Status:     registry.StatusIdle,
		Namespace:  "tenants",
		BotToken:   "secret-bot-token",
		HooksToken: "secret-hooks-token",
	})

	req := httptest.NewRequest(http.MethodGet, "/tenants/redact-tenant", nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&resp)
	assert.Empty(t, resp["bot_token"], "bot_token must be redacted")
	assert.Empty(t, resp["hooks_token"], "hooks_token must be redacted")
}

func TestCreateTenant_Conflict(t *testing.T) {
	h, _, _, _ := newTestHandler(t)

	body, _ := json.Marshal(map[string]interface{}{"tenant_id": "dup-tenant"})
	req1 := httptest.NewRequest(http.MethodPost, "/tenants", bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	h.Router().ServeHTTP(httptest.NewRecorder(), req1)

	body, _ = json.Marshal(map[string]interface{}{"tenant_id": "dup-tenant"})
	req2 := httptest.NewRequest(http.MethodPost, "/tenants", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	h.Router().ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusConflict, rec2.Code)
}

// TestWakeTenant_NewTenant: first wake creates PVC + Pod + registry record
func TestWakeTenant_NewTenant(t *testing.T) {
	h, reg, _, cs := newTestHandler(t)
	tenantID := "new-tenant"

	simulatePodReady(cs, tenantID, "tenants", "10.0.0.1")

	req := httptest.NewRequest(http.MethodPost, "/wake/"+tenantID, nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var result map[string]string
	json.NewDecoder(rec.Body).Decode(&result)
	assert.Equal(t, "10.0.0.1", result["pod_ip"])

	// Verify registry updated
	tenant, err := reg.GetTenant(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, registry.StatusRunning, tenant.Status)
	assert.Equal(t, "10.0.0.1", tenant.PodIP)
}

// TestWakeTenant_AlreadyRunning: returns IP immediately, no new Pod created
func TestWakeTenant_AlreadyRunning(t *testing.T) {
	h, reg, _, cs := newTestHandler(t)
	tenantID := "running-tenant"

	// Pre-seed registry as running
	reg.CreateTenant(context.Background(), &registry.TenantRecord{
		TenantID:     tenantID,
		Status:       registry.StatusRunning,
		PodName:      tenantID,
		PodIP:        "10.0.0.5",
		Namespace:    "tenants",
		LastActiveAt: time.Now(),
		IdleTimeoutS: 300,
	})

	// Pre-seed k8s with the running pod (so PodExists returns true)
	cs.CoreV1().Pods("tenants").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantID,
			Namespace: "tenants",
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.5"},
	}, metav1.CreateOptions{})

	req := httptest.NewRequest(http.MethodPost, "/wake/"+tenantID, nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var result map[string]string
	json.NewDecoder(rec.Body).Decode(&result)
	assert.Equal(t, "10.0.0.5", result["pod_ip"])

	// No NEW pods should be created (only the pre-seeded one)
	pods, _ := cs.CoreV1().Pods("tenants").List(context.Background(), metav1.ListOptions{})
	assert.Len(t, pods.Items, 1, "no new pods should be created for already-running tenant")
}

// TestWakeTenant_IdleTenant: reuses PVC, creates new Pod
func TestWakeTenant_IdleTenant(t *testing.T) {
	h, reg, _, cs := newTestHandler(t)
	tenantID := "idle-tenant"

	// Pre-seed as idle (PVC exists, no pod)
	reg.CreateTenant(context.Background(), &registry.TenantRecord{
		TenantID:     tenantID,
		Status:       registry.StatusIdle,
		Namespace:    "tenants",
		S3Prefix:     "tenants/idle-tenant/",
		LastActiveAt: time.Now().Add(-10 * time.Minute),
		IdleTimeoutS: 300,
	})

	simulatePodReady(cs, tenantID, "tenants", "10.0.0.2")

	req := httptest.NewRequest(http.MethodPost, "/wake/"+tenantID, nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var result map[string]string
	json.NewDecoder(rec.Body).Decode(&result)
	assert.Equal(t, "10.0.0.2", result["pod_ip"])

	tenant, _ := reg.GetTenant(context.Background(), tenantID)
	assert.Equal(t, registry.StatusRunning, tenant.Status)
}

// TestWakeTenant_ConcurrentWake: 10 goroutines wake same tenant → only 1 Pod created
func TestWakeTenant_ConcurrentWake(t *testing.T) {
	h, _, _, cs := newTestHandler(t)
	tenantID := "concurrent-tenant"

	simulatePodReady(cs, tenantID, "tenants", "10.0.0.3")

	var wg sync.WaitGroup
	var successCount int64
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/wake/"+tenantID, nil)
			rec := httptest.NewRecorder()
			h.Router().ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				atomic.AddInt64(&successCount, 1)
			}
		}()
	}
	wg.Wait()

	// All requests should eventually succeed
	assert.Greater(t, successCount, int64(0))

	// Only 1 pod should exist
	pods, _ := cs.CoreV1().Pods("tenants").List(context.Background(), metav1.ListOptions{
		LabelSelector: "tenant=" + tenantID,
	})
	assert.LessOrEqual(t, len(pods.Items), 1, "at most 1 pod should be created for concurrent wakes")
}

func TestDeleteTenant(t *testing.T) {
	h, reg, _, _ := newTestHandler(t)
	tenantID := "delete-me"

	reg.CreateTenant(context.Background(), &registry.TenantRecord{
		TenantID:  tenantID,
		Status:    registry.StatusIdle,
		Namespace: "tenants",
	})

	req := httptest.NewRequest(http.MethodDelete, "/tenants/"+tenantID, nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	tenant, _ := reg.GetTenant(context.Background(), tenantID)
	assert.Nil(t, tenant)
}

func TestUpdateActivity(t *testing.T) {
	h, reg, _, _ := newTestHandler(t)
	tenantID := "active-tenant"

	before := time.Now().Add(-1 * time.Minute)
	reg.CreateTenant(context.Background(), &registry.TenantRecord{
		TenantID:     tenantID,
		Status:       registry.StatusRunning,
		Namespace:    "tenants",
		LastActiveAt: before,
	})

	req := httptest.NewRequest(http.MethodPut, "/tenants/"+tenantID+"/activity", nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	tenant, _ := reg.GetTenant(context.Background(), tenantID)
	assert.True(t, tenant.LastActiveAt.After(before))
}

// --- Config API tests ---

// mockWarmPool implements api.WarmPoolManager for testing
type mockWarmPool struct {
	target int32
}

func (m *mockWarmPool) Target() int32 {
	return m.target
}

func (m *mockWarmPool) SetTarget(target int32) {
	m.target = target
}

func TestGetConfig(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	wp := &mockWarmPool{target: 5}
	h.SetWarmPool(wp)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var cfg api.RuntimeConfig
	json.NewDecoder(rec.Body).Decode(&cfg)
	assert.Equal(t, 5, cfg.WarmPoolTarget)
}

func TestGetConfig_NoWarmPool(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// Don't set warm pool — should return zero value

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var cfg api.RuntimeConfig
	json.NewDecoder(rec.Body).Decode(&cfg)
	assert.Equal(t, 0, cfg.WarmPoolTarget)
}

func TestUpdateConfig(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	wp := &mockWarmPool{target: 5}
	h.SetWarmPool(wp)

	body, _ := json.Marshal(api.RuntimeConfig{WarmPoolTarget: 20})
	req := httptest.NewRequest(http.MethodPut, "/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var cfg api.RuntimeConfig
	json.NewDecoder(rec.Body).Decode(&cfg)
	assert.Equal(t, 20, cfg.WarmPoolTarget)
	assert.Equal(t, int32(20), wp.Target())
}

func TestUpdateConfig_InvalidJSON(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	wp := &mockWarmPool{target: 5}
	h.SetWarmPool(wp)

	req := httptest.NewRequest(http.MethodPut, "/config", bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdateConfig_NegativeTarget(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	wp := &mockWarmPool{target: 5}
	h.SetWarmPool(wp)

	body, _ := json.Marshal(map[string]int{"warm_pool_target": -1})
	req := httptest.NewRequest(http.MethodPut, "/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// Target should not have changed
	assert.Equal(t, int32(5), wp.Target())
}

func TestUpdateConfig_NoWarmPool(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// Don't set warm pool

	body, _ := json.Marshal(api.RuntimeConfig{WarmPoolTarget: 10})
	req := httptest.NewRequest(http.MethodPut, "/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
