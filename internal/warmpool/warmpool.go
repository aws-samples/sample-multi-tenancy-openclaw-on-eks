package warmpool

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	k8sclient "github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/k8s"
)

const redisKey = "config:warm_pool_target"

// Manager maintains a warm-pool Deployment of low-priority OpenClaw pods.
// The Deployment keeps `target` replicas running at all times.
// When a warm pod is claimed by a tenant (label changed to warm=consuming),
// the Deployment automatically creates a replacement pod.
type Manager struct {
	k8s       *k8sclient.Client
	rdb       *redis.Client
	namespace string
	target    atomic.Int32 // local cache / fallback
	interval  time.Duration
}

func New(k8s *k8sclient.Client, rdb *redis.Client, namespace string, target int) *Manager {
	m := &Manager{
		k8s:       k8s,
		rdb:       rdb,
		namespace: namespace,
		interval:  30 * time.Second,
	}
	if target < 0 {
		target = 0
	} else if target > math.MaxInt32 {
		target = math.MaxInt32
	}
	m.target.Store(int32(target))

	// Seed Redis with the initial value if not already set.
	if rdb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// SetNX: only set if key doesn't exist, so we don't clobber a value
		// that was previously persisted by another replica.
		rdb.SetNX(ctx, redisKey, target, 0)
	}
	return m
}

// Target returns the current warm pool target (thread-safe).
// Reads from Redis first; falls back to local atomic cache if Redis is unavailable.
func (m *Manager) Target() int32 {
	if m.rdb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		val, err := m.rdb.Get(ctx, redisKey).Result()
		if err == nil {
			if n, err := strconv.ParseInt(val, 10, 32); err == nil {
				m.target.Store(int32(n)) // refresh local cache
				return int32(n)
			}
		}
		// Redis read failed — fall back to local cache
		slog.Warn("warm pool: Redis read failed, using local cache", "err", err)
	}
	return m.target.Load()
}

// SetTarget updates the warm pool target at runtime (thread-safe).
// Writes to Redis for cross-replica consistency, then updates the local cache.
func (m *Manager) SetTarget(target int32) {
	if m.rdb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := m.rdb.Set(ctx, redisKey, fmt.Sprintf("%d", target), 0).Err(); err != nil {
			slog.Error("warm pool: Redis write failed", "target", target, "err", err)
		}
	}
	m.target.Store(target)
	slog.Info("warm pool: target updated", "target", target)
}

// Run ensures the warm-pool Deployment exists and stays at the desired replica count.
func (m *Manager) Run(ctx context.Context) {
	slog.Info("warm pool: starting", "target", m.Target(), "namespace", m.namespace)

	m.reconcile(ctx)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("warm pool: shutting down")
			return
		case <-ticker.C:
			m.reconcile(ctx)
		}
	}
}

func (m *Manager) reconcile(ctx context.Context) {
	if err := m.k8s.EnsureWarmPoolDeployment(ctx, m.namespace, m.Target()); err != nil {
		slog.Error("warm pool: ensure deployment failed", "err", err)
	}
}
