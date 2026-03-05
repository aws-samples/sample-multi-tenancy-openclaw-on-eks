package reconciler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	k8sclient "github.com/shawn/openclaw-tenancy/internal/k8s"
	"github.com/shawn/openclaw-tenancy/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// noopRDB returns a redis client that connects to nothing (errors are non-fatal in reconciler)
func noopRDB() *redis.Client {
	return redis.NewClient(&redis.Options{Addr: "localhost:59999"})
}

func TestReconcile_MissingPodResetsState(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMock()
	k8s := k8sclient.New(fake.NewSimpleClientset(), k8sclient.Config{})

	err := reg.CreateTenant(ctx, &registry.TenantRecord{
		TenantID: "abc123", Status: registry.StatusRunning,
		PodName: "abc123", PodIP: "10.0.0.1",
		Namespace: "tenants", CreatedAt: time.Now(),
		LastActiveAt: time.Now().Add(-20 * time.Minute), // well past idle timeout
		IdleTimeoutS: 600,
	})
	require.NoError(t, err)

	// No waker — should always reset to idle
	New(reg, k8s, fake.NewSimpleClientset(), noopRDB(), "tenants", 0, nil).reconcile(ctx)

	tenant, err := reg.GetTenant(ctx, "abc123")
	require.NoError(t, err)
	assert.Equal(t, registry.StatusIdle, tenant.Status)
	assert.Empty(t, tenant.PodName)
	assert.Empty(t, tenant.PodIP)
}

func TestReconcile_ExistingPodNotReset(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMock()
	fakeCS := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "def456", Namespace: "tenants",
			Labels: map[string]string{"app": "openclaw"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.2"},
	})

	err := reg.CreateTenant(ctx, &registry.TenantRecord{
		TenantID: "def456", Status: registry.StatusRunning,
		PodName: "def456", PodIP: "10.0.0.2",
		Namespace: "tenants", CreatedAt: time.Now(), LastActiveAt: time.Now(),
	})
	require.NoError(t, err)

	New(reg, k8sclient.New(fakeCS, k8sclient.Config{}), fakeCS, noopRDB(), "tenants", 0, nil).reconcile(ctx)

	tenant, err := reg.GetTenant(ctx, "def456")
	require.NoError(t, err)
	assert.Equal(t, registry.StatusRunning, tenant.Status)
	assert.Equal(t, "def456", tenant.PodName)
}

func TestReconcile_PodIPChanged_SyncsCache(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMock()
	fakeCS := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ipchange", Namespace: "tenants",
			Labels: map[string]string{"app": "openclaw"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.99"},
	})

	err := reg.CreateTenant(ctx, &registry.TenantRecord{
		TenantID: "ipchange", Status: registry.StatusRunning,
		PodName: "ipchange", PodIP: "10.0.0.1", // old IP
		Namespace: "tenants", CreatedAt: time.Now(), LastActiveAt: time.Now(),
	})
	require.NoError(t, err)

	New(reg, k8sclient.New(fakeCS, k8sclient.Config{}), fakeCS, noopRDB(), "tenants", 0, nil).reconcile(ctx)

	tenant, err := reg.GetTenant(ctx, "ipchange")
	require.NoError(t, err)
	assert.Equal(t, registry.StatusRunning, tenant.Status)
	assert.Equal(t, "10.0.0.99", tenant.PodIP)
}

func TestReconcile_OrphanPodDeleted(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMock()

	// Pod exists in k8s but no running tenant in DynamoDB
	fakeCS := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: "tenants",
			Labels: map[string]string{"app": "openclaw"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.5"},
	})

	New(reg, k8sclient.New(fakeCS, k8sclient.Config{}), fakeCS, noopRDB(), "tenants", 0, nil).reconcile(ctx)

	_, err := fakeCS.CoreV1().Pods("tenants").Get(ctx, "orphan", metav1.GetOptions{})
	assert.Error(t, err, "orphan pod should have been deleted")
}

func TestReconcile_IdleTenantIgnored(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMock()

	err := reg.CreateTenant(ctx, &registry.TenantRecord{
		TenantID: "idle1", Status: registry.StatusIdle,
		Namespace: "tenants", CreatedAt: time.Now(), LastActiveAt: time.Now(),
	})
	require.NoError(t, err)

	New(reg, k8sclient.New(fake.NewSimpleClientset(), k8sclient.Config{}), fake.NewSimpleClientset(), noopRDB(), "tenants", 0, nil).reconcile(ctx)

	tenant, err := reg.GetTenant(ctx, "idle1")
	require.NoError(t, err)
	assert.Equal(t, registry.StatusIdle, tenant.Status)
}

// TestReconcile_StartingPodNotReset — pod exists but not yet Running (Init/Pending),
// reconciler should NOT reset to idle (it would kill the starting pod).
func TestReconcile_StartingPodNotReset(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMock()

	// Pod exists but is in Init stage — no IP yet
	fakeCS := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "starting",
			Namespace: "tenants",
			Labels:    map[string]string{"app": "openclaw"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending, // still starting, no IP
			PodIP: "",
		},
	})

	err := reg.CreateTenant(ctx, &registry.TenantRecord{
		TenantID: "starting", Status: registry.StatusRunning,
		PodName: "starting", PodIP: "",
		Namespace: "tenants", CreatedAt: time.Now(), LastActiveAt: time.Now(),
	})
	require.NoError(t, err)

	New(reg, k8sclient.New(fakeCS, k8sclient.Config{}), fakeCS, noopRDB(), "tenants", 0, nil).reconcile(ctx)

	tenant, err := reg.GetTenant(ctx, "starting")
	require.NoError(t, err)
	// Must still be running — pod is starting, not missing
	assert.Equal(t, registry.StatusRunning, tenant.Status, "starting pod must not be reset to idle")
}

// TestReconcile_AutoRestart_WithinIdleWindow — pod missing but tenant was active
// recently (within idle timeout). Should call wake to auto-restart.
func TestReconcile_AutoRestart_WithinIdleWindow(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMock()
	k8s := k8sclient.New(fake.NewSimpleClientset(), k8sclient.Config{})

	err := reg.CreateTenant(ctx, &registry.TenantRecord{
		TenantID: "restart1", Status: registry.StatusRunning,
		PodName: "restart1", PodIP: "10.0.0.1",
		Namespace: "tenants", CreatedAt: time.Now(),
		LastActiveAt: time.Now().Add(-2 * time.Minute), // 2 min ago — within 600s window
		IdleTimeoutS: 600,
	})
	require.NoError(t, err)

	wakeCalled := false
	waker := func(ctx context.Context, tenantID string) (string, error) {
		wakeCalled = true
		assert.Equal(t, "restart1", tenantID)
		// Simulate successful wake — update registry
		reg.UpdateStatus(ctx, tenantID, registry.StatusRunning, "restart1", "10.0.0.99")
		return "10.0.0.99", nil
	}

	New(reg, k8s, fake.NewSimpleClientset(), noopRDB(), "tenants", 0, waker).reconcile(ctx)

	assert.True(t, wakeCalled, "waker should have been called for tenant within idle window")
	tenant, err := reg.GetTenant(ctx, "restart1")
	require.NoError(t, err)
	assert.Equal(t, registry.StatusRunning, tenant.Status)
}

// TestReconcile_NoAutoRestart_PastIdleWindow — pod missing and tenant has been
// idle longer than idle_timeout_s. Should reset to idle, NOT auto-restart.
func TestReconcile_NoAutoRestart_PastIdleWindow(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMock()
	k8s := k8sclient.New(fake.NewSimpleClientset(), k8sclient.Config{})

	err := reg.CreateTenant(ctx, &registry.TenantRecord{
		TenantID: "norst1", Status: registry.StatusRunning,
		PodName: "norst1", PodIP: "10.0.0.1",
		Namespace: "tenants", CreatedAt: time.Now(),
		LastActiveAt: time.Now().Add(-15 * time.Minute), // 15 min ago — past 600s window
		IdleTimeoutS: 600,
	})
	require.NoError(t, err)

	wakeCalled := false
	waker := func(ctx context.Context, tenantID string) (string, error) {
		wakeCalled = true
		return "", nil
	}

	New(reg, k8s, fake.NewSimpleClientset(), noopRDB(), "tenants", 0, waker).reconcile(ctx)

	assert.False(t, wakeCalled, "waker should NOT be called for tenant past idle window")
	tenant, err := reg.GetTenant(ctx, "norst1")
	require.NoError(t, err)
	assert.Equal(t, registry.StatusIdle, tenant.Status)
}

// TestReconcile_AutoRestart_WakeFails_ResetsToIdle — auto-restart attempted but
// wake fails. Should fall back to resetting to idle.
func TestReconcile_AutoRestart_WakeFails_ResetsToIdle(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMock()
	k8s := k8sclient.New(fake.NewSimpleClientset(), k8sclient.Config{})

	err := reg.CreateTenant(ctx, &registry.TenantRecord{
		TenantID: "failwake", Status: registry.StatusRunning,
		PodName: "failwake", PodIP: "10.0.0.1",
		Namespace: "tenants", CreatedAt: time.Now(),
		LastActiveAt: time.Now().Add(-1 * time.Minute), // 1 min ago — within window
		IdleTimeoutS: 600,
	})
	require.NoError(t, err)

	waker := func(ctx context.Context, tenantID string) (string, error) {
		return "", fmt.Errorf("simulated wake failure")
	}

	New(reg, k8s, fake.NewSimpleClientset(), noopRDB(), "tenants", 0, waker).reconcile(ctx)

	tenant, err := reg.GetTenant(ctx, "failwake")
	require.NoError(t, err)
	assert.Equal(t, registry.StatusIdle, tenant.Status, "failed auto-restart should reset to idle")
}
