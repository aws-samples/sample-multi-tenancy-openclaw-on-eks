package lifecycle

import (
	"context"
	"log/slog"
	"time"

	k8sclient "github.com/shawn/openclaw-tenancy/internal/k8s"
	"github.com/shawn/openclaw-tenancy/internal/registry"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Controller manages idle tenant lifecycle with leader election
type Controller struct {
	reg       registry.Client
	k8s       *k8sclient.Client
	cs        kubernetes.Interface
	namespace string
	leaderID  string
}

// NewForTest creates a Controller for unit testing (no leader election)
func NewForTest(reg registry.Client, k8s *k8sclient.Client) *Controller {
	return &Controller{reg: reg, k8s: k8s, namespace: "tenants"}
}

// CheckIdleTenants is exported for testing
func (c *Controller) CheckIdleTenants(ctx context.Context) {
	c.checkIdleTenants(ctx)
}

func New(reg registry.Client, k8s *k8sclient.Client, cs kubernetes.Interface, namespace, leaderID string) *Controller {
	return &Controller{
		reg:       reg,
		k8s:       k8s,
		cs:        cs,
		namespace: namespace,
		leaderID:  leaderID,
	}
}

// Run starts the leader election loop. Only the elected leader runs idle timeout.
func (c *Controller) Run(ctx context.Context) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      "orchestrator-leader",
			Namespace: c.namespace,
		},
		Client: c.cs.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: c.leaderID,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				slog.Info("leader election: became leader, starting idle timeout loop", "id", c.leaderID)
				c.runIdleLoop(ctx)
			},
			OnStoppedLeading: func() {
				slog.Info("leader election: lost leadership", "id", c.leaderID)
			},
			OnNewLeader: func(identity string) {
				if identity != c.leaderID {
					slog.Info("leader election: new leader", "leader", identity)
				}
			},
		},
	})
}

// runIdleLoop is only run by the current leader
func (c *Controller) runIdleLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	c.checkIdleTenants(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.checkIdleTenants(ctx)
		}
	}
}

func (c *Controller) checkIdleTenants(ctx context.Context) {
	// Use per-tenant idle_timeout_s; scan for any tenant idle > 5min as default
	tenants, err := c.reg.ListIdleTenants(ctx, 5*time.Minute)
	if err != nil {
		slog.Error("idle check: list tenants failed", "err", err)
		return
	}
	for _, t := range tenants {
		timeout := time.Duration(t.IdleTimeoutS) * time.Second
		if timeout == 0 {
			timeout = 5 * time.Minute
		}
		if time.Since(t.LastActiveAt) < timeout {
			continue
		}
		slog.Info("idle check: terminating idle tenant", "tenant", t.TenantID, "idle_for", time.Since(t.LastActiveAt))
		if err := c.k8s.DeletePod(ctx, t.PodName, t.Namespace, 120); err != nil {
			slog.Error("idle check: delete pod failed", "tenant", t.TenantID, "err", err)
			continue
		}
		if err := c.reg.UpdateStatus(ctx, t.TenantID, registry.StatusIdle, "", ""); err != nil {
			slog.Error("idle check: update status failed", "tenant", t.TenantID, "err", err)
		}
	}
}
