package lifecycle_test

import (
	"context"
	"testing"
	"time"

	k8sclient "github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/k8s"
	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/lifecycle"
	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestIdleTimeout_TerminatesIdlePod verifies the controller terminates pods
// that have exceeded their idle timeout
func TestIdleTimeout_TerminatesIdlePod(t *testing.T) {
	cs := fake.NewSimpleClientset()
	reg := registry.NewMock()
	k8s := k8sclient.New(cs, k8sclient.Config{
		KataRuntimeClass: "kata-qemu",
		OpenClawImage:    "openclaw:test",
	})

	// Create a running tenant that went idle 10 minutes ago
	tenantID := "idle-victim"
	podName := tenantID
	namespace := "tenants"

	reg.CreateTenant(context.Background(), &registry.TenantRecord{
		TenantID:     tenantID,
		Status:       registry.StatusRunning,
		PodName:      podName,
		PodIP:        "10.0.0.9",
		Namespace:    namespace,
		LastActiveAt: time.Now().Add(-10 * time.Minute),
		IdleTimeoutS: 300, // 5 minutes
	})

	// Create the actual pod in fake k8s
	cs.CoreV1().Pods(namespace).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}, metav1.CreateOptions{})

	// Run one idle check cycle directly (without leader election for unit test)
	ctrl := lifecycle.NewForTest(reg, k8s)
	ctrl.CheckIdleTenants(context.Background())

	// Pod should be deleted
	_, err := cs.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
	assert.Error(t, err, "pod should have been deleted")

	// Registry should show idle
	tenant, err := reg.GetTenant(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, registry.StatusIdle, tenant.Status)
	assert.Empty(t, tenant.PodIP)
}

// TestIdleTimeout_DoesNotTerminateActivePod verifies active pods are not touched
func TestIdleTimeout_DoesNotTerminateActivePod(t *testing.T) {
	cs := fake.NewSimpleClientset()
	reg := registry.NewMock()
	k8s := k8sclient.New(cs, k8sclient.Config{})

	tenantID := "active-tenant"
	podName := tenantID
	namespace := "tenants"

	reg.CreateTenant(context.Background(), &registry.TenantRecord{
		TenantID:     tenantID,
		Status:       registry.StatusRunning,
		PodName:      podName,
		Namespace:    namespace,
		LastActiveAt: time.Now(), // just active
		IdleTimeoutS: 300,
	})

	cs.CoreV1().Pods(namespace).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
	}, metav1.CreateOptions{})

	ctrl := lifecycle.NewForTest(reg, k8s)
	ctrl.CheckIdleTenants(context.Background())

	// Pod should still exist
	pod, err := cs.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotNil(t, pod)
}
