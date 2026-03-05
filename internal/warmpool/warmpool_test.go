package warmpool

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	k8sclient "github.com/shawn/openclaw-tenancy/internal/k8s"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestK8s() *k8sclient.Client {
	cs := fake.NewSimpleClientset()
	return k8sclient.New(cs, k8sclient.Config{
		KataRuntimeClass: "kata-qemu",
		OpenClawImage:    "openclaw:test",
	})
}

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return mr, rdb
}

// TestWarmPool_EnsureDeployment verifies the warm-pool Deployment is created
func TestWarmPool_EnsureDeployment(t *testing.T) {
	cs := fake.NewSimpleClientset()
	k8s := k8sclient.New(cs, k8sclient.Config{
		KataRuntimeClass: "kata-qemu",
		OpenClawImage:    "openclaw:test",
	})

	wp := New(k8s, nil, "tenants", 2)
	wp.reconcile(context.Background())

	deploy, err := cs.AppsV1().Deployments("tenants").Get(context.Background(), "warm-pool", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, int32(2), *deploy.Spec.Replicas)
}

// TestWarmPool_IdempotentReconcile verifies reconcile is idempotent
func TestWarmPool_IdempotentReconcile(t *testing.T) {
	cs := fake.NewSimpleClientset()
	k8s := k8sclient.New(cs, k8sclient.Config{
		KataRuntimeClass: "kata-qemu",
		OpenClawImage:    "openclaw:test",
	})

	wp := New(k8s, nil, "tenants", 1)
	wp.reconcile(context.Background())
	wp.reconcile(context.Background()) // second call should not fail

	deploy, err := cs.AppsV1().Deployments("tenants").Get(context.Background(), "warm-pool", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, int32(1), *deploy.Spec.Replicas)
}

// TestWarmPool_SetTarget verifies atomic target updates (nil Redis = local-only)
func TestWarmPool_SetTarget(t *testing.T) {
	k8s := newTestK8s()

	wp := New(k8s, nil, "tenants", 5)
	assert.Equal(t, int32(5), wp.Target())

	wp.SetTarget(10)
	assert.Equal(t, int32(10), wp.Target())

	wp.SetTarget(0)
	assert.Equal(t, int32(0), wp.Target())
}

// TestWarmPool_SetTargetConcurrent verifies thread-safe target updates
func TestWarmPool_SetTargetConcurrent(t *testing.T) {
	k8s := newTestK8s()

	wp := New(k8s, nil, "tenants", 0)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			wp.SetTarget(int32(i))
		}
		close(done)
	}()

	// Read concurrently while writing
	for i := 0; i < 1000; i++ {
		_ = wp.Target() // should not race
	}
	<-done

	// Final value should be 999
	assert.Equal(t, int32(999), wp.Target())
}

// TestSetTarget_Redis verifies SetTarget writes to Redis
func TestSetTarget_Redis(t *testing.T) {
	mr, rdb := newTestRedis(t)
	_ = mr
	k8s := newTestK8s()

	wp := New(k8s, rdb, "tenants", 3)

	// Constructor seeds Redis via SetNX
	val, err := mr.Get(redisKey)
	require.NoError(t, err)
	assert.Equal(t, "3", val)

	// SetTarget updates Redis
	wp.SetTarget(42)
	val, err = mr.Get(redisKey)
	require.NoError(t, err)
	assert.Equal(t, "42", val)

	// Local cache is also updated
	assert.Equal(t, int32(42), wp.target.Load())
}

// TestTarget_ReadFromRedis verifies Target() reads from Redis and updates local cache
func TestTarget_ReadFromRedis(t *testing.T) {
	mr, rdb := newTestRedis(t)
	k8s := newTestK8s()

	wp := New(k8s, rdb, "tenants", 5)

	// Simulate another replica writing to Redis directly
	mr.Set(redisKey, "99")

	// Target() should return the Redis value
	assert.Equal(t, int32(99), wp.Target())

	// Local cache should be updated
	assert.Equal(t, int32(99), wp.target.Load())
}

// TestTarget_RedisFallback verifies that nil Redis client falls back to atomic value
func TestTarget_RedisFallback(t *testing.T) {
	k8s := newTestK8s()

	wp := New(k8s, nil, "tenants", 7)
	assert.Equal(t, int32(7), wp.Target())

	wp.SetTarget(15)
	assert.Equal(t, int32(15), wp.Target())
}

// TestTarget_RedisDown verifies graceful degradation when Redis goes away
func TestTarget_RedisDown(t *testing.T) {
	mr, rdb := newTestRedis(t)
	k8s := newTestK8s()

	wp := New(k8s, rdb, "tenants", 10)

	// Verify Redis works
	assert.Equal(t, int32(10), wp.Target())

	// Update via SetTarget so local cache has 20
	wp.SetTarget(20)
	assert.Equal(t, int32(20), wp.Target())

	// Kill Redis — Target() should fall back to local cache
	mr.Close()
	assert.Equal(t, int32(20), wp.Target())
}

// TestNew_DoesNotClobberExistingRedis verifies SetNX semantics in constructor
func TestNew_DoesNotClobberExistingRedis(t *testing.T) {
	mr, rdb := newTestRedis(t)
	k8s := newTestK8s()

	// Pre-set a value in Redis (simulating another replica)
	mr.Set(redisKey, "50")

	// Constructor with target=3 should NOT overwrite the existing 50
	wp := New(k8s, rdb, "tenants", 3)

	// Redis still has 50
	val, err := mr.Get(redisKey)
	require.NoError(t, err)
	assert.Equal(t, "50", val)

	// Target() reads from Redis = 50
	assert.Equal(t, int32(50), wp.Target())
}

// TestSetTarget_Redis_Concurrent verifies concurrent SetTarget with Redis is safe
func TestSetTarget_Redis_Concurrent(t *testing.T) {
	_, rdb := newTestRedis(t)
	k8s := newTestK8s()

	wp := New(k8s, rdb, "tenants", 0)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			wp.SetTarget(int32(i))
		}
		close(done)
	}()

	for i := 0; i < 500; i++ {
		_ = wp.Target()
	}
	<-done

	// Final value from Target should match what Redis has
	target := wp.Target()
	rVal, _ := rdb.Get(context.Background(), redisKey).Result()
	assert.Equal(t, fmt.Sprintf("%d", target), rVal)
}
