package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	k8sclient "github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/k8s"
	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/registry"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// WakeFunc is called to restart a tenant pod. It should trigger the same logic
// as POST /wake/{tenantID}. If nil, auto-restart is disabled and missing pods
// are simply reset to idle.
type WakeFunc func(ctx context.Context, tenantID string) (podIP string, err error)

// Reconciler watches pod events via a K8s SharedInformer and reconciles
// state drift between DynamoDB and k8s. A low-frequency full reconcile
// runs as a safety net to catch any missed events.
//
// If a tenant is marked as "running" in DynamoDB but its pod no longer exists
// in k8s, the reconciler either auto-restarts the pod (if within idle timeout)
// or resets the tenant state to "idle" and cleans up stale Redis endpoint
// cache entries.
type Reconciler struct {
	reg       registry.Client
	k8s       *k8sclient.Client
	cs        kubernetes.Interface
	rdb       *redis.Client
	namespace string
	interval  time.Duration
	wake      WakeFunc
}

// New creates a new Reconciler. waker may be nil to disable auto-restart.
// cs is the kubernetes clientset used to create the pod informer.
// interval controls the safety-net full-reconcile frequency (default 300s).
func New(reg registry.Client, k8s *k8sclient.Client, cs kubernetes.Interface, rdb *redis.Client, namespace string, interval time.Duration, waker WakeFunc) *Reconciler {
	if interval <= 0 {
		interval = 300 * time.Second
	}
	return &Reconciler{
		reg:       reg,
		k8s:       k8s,
		cs:        cs,
		rdb:       rdb,
		namespace: namespace,
		interval:  interval,
		wake:      waker,
	}
}

// Run starts the informer-driven reconciliation loop. It blocks until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	slog.Info("reconciler: starting", "interval", r.interval, "namespace", r.namespace)

	// Run a full reconcile immediately on startup
	r.reconcile(ctx)

	// Create a SharedInformerFactory filtered to namespace + label app=openclaw
	factory := informers.NewSharedInformerFactoryWithOptions(
		r.cs,
		0, // no resync — we have the safety-net ticker for that
		informers.WithNamespace(r.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = labels.Set{"app": "openclaw"}.String()
		}),
	)

	podInformer := factory.Core().V1().Pods().Informer()

	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			r.handlePodUpdate(ctx, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			r.handlePodDelete(ctx, obj)
		},
	})

	// Start the informer (runs in background goroutines)
	factory.Start(ctx.Done())

	// Wait for initial cache sync
	slog.Info("reconciler: waiting for informer cache sync")
	if !cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced) {
		slog.Error("reconciler: informer cache sync failed")
		return
	}
	slog.Info("reconciler: informer cache synced")

	// Safety-net full reconcile ticker
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("reconciler: shutting down")
			return
		case <-ticker.C:
			r.reconcile(ctx)
		}
	}
}

// handlePodUpdate is called by the informer on pod UPDATE events.
// We only act on meaningful changes: phase transitions to Failed/Succeeded, or IP changes.
func (r *Reconciler) handlePodUpdate(ctx context.Context, oldObj, newObj interface{}) {
	oldPod, ok := oldObj.(*corev1.Pod)
	if !ok {
		return
	}
	newPod, ok := newObj.(*corev1.Pod)
	if !ok {
		return
	}

	tenantID := newPod.Name

	phaseChanged := oldPod.Status.Phase != newPod.Status.Phase
	ipChanged := oldPod.Status.PodIP != newPod.Status.PodIP

	// Only reconcile on meaningful changes
	if phaseChanged {
		// React to terminal phases or transitions that need attention
		switch newPod.Status.Phase {
		case corev1.PodFailed, corev1.PodSucceeded:
			slog.Info("reconciler: pod entered terminal phase",
				"tenant", tenantID, "phase", newPod.Status.Phase,
				"old_phase", oldPod.Status.Phase)
			r.reconcileTenant(ctx, tenantID)
			return
		case corev1.PodRunning:
			// Pod became Running — may need to promote from provisioning or sync IP
			slog.Debug("reconciler: pod became running", "tenant", tenantID)
			r.reconcileTenant(ctx, tenantID)
			return
		}
	}

	if ipChanged && newPod.Status.PodIP != "" {
		slog.Info("reconciler: pod IP changed",
			"tenant", tenantID,
			"old_ip", oldPod.Status.PodIP,
			"new_ip", newPod.Status.PodIP)
		r.reconcileTenant(ctx, tenantID)
	}
}

// handlePodDelete is called by the informer on pod DELETE events.
func (r *Reconciler) handlePodDelete(ctx context.Context, obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		// Handle DeletedFinalStateUnknown (informer missed the delete)
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			slog.Warn("reconciler: unexpected delete event object type")
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			slog.Warn("reconciler: tombstone contained unexpected object type")
			return
		}
	}

	tenantID := pod.Name
	slog.Info("reconciler: pod deleted, reconciling tenant", "tenant", tenantID)
	r.reconcileTenant(ctx, tenantID)
}

// reconcileTenant performs a single-tenant reconciliation.
// It checks the tenant's DynamoDB state against the actual k8s pod state
// and corrects any drift (missing pod, IP change, stuck provisioning).
func (r *Reconciler) reconcileTenant(ctx context.Context, tenantID string) {
	t, err := r.reg.GetTenant(ctx, tenantID)
	if err != nil {
		slog.Error("reconciler: failed to get tenant", "tenant", tenantID, "err", err)
		return
	}
	if t == nil {
		// Tenant not in DynamoDB — nothing to do; orphan cleanup happens in full reconcile
		slog.Debug("reconciler: tenant not found in registry, skipping", "tenant", tenantID)
		return
	}

	switch t.Status {
	case registry.StatusRunning:
		r.reconcileRunningTenant(ctx, t)
	case registry.StatusProvisioning:
		r.reconcileProvisioningTenant(ctx, t)
	default:
		// idle / terminated — no action needed for event-driven reconcile
		slog.Debug("reconciler: tenant in non-active state, skipping",
			"tenant", tenantID, "status", t.Status)
	}
}

// reconcileRunningTenant handles a single running tenant (extracted from syncRunningTenants).
func (r *Reconciler) reconcileRunningTenant(ctx context.Context, t *registry.TenantRecord) {
	podName := t.TenantID

	// Check actual pod state
	currentIP, podFound, podStarting := r.getPodState(ctx, podName)

	if currentIP == "" {
		if podFound && podStarting {
			slog.Debug("reconciler: pod starting, skipping reset", "tenant", t.TenantID)
			return
		}
		if podFound {
			// Pod exists but not running with IP — leave it alone
			return
		}
		// Pod truly missing — auto-restart or reset to idle
		if r.shouldAutoRestart(t) {
			slog.Info("reconciler: pod missing within idle window, auto-restarting",
				"tenant", t.TenantID,
				"last_active", t.LastActiveAt.Format(time.RFC3339),
				"idle_timeout_s", t.IdleTimeoutS)
			r.rdb.Del(ctx, fmt.Sprintf("router:endpoint:%s", t.TenantID))
			if _, err := r.wake(ctx, t.TenantID); err != nil {
				slog.Error("reconciler: auto-restart failed, resetting to idle",
					"tenant", t.TenantID, "err", err)
				if err := r.reg.UpdateStatus(ctx, t.TenantID, registry.StatusIdle, "", ""); err != nil {
					slog.Error("reconciler: failed to reset tenant state", "tenant", t.TenantID, "err", err)
				}
			}
			return
		}
		slog.Warn("reconciler: pod missing, resetting to idle", "tenant", t.TenantID)
		if err := r.reg.UpdateStatus(ctx, t.TenantID, registry.StatusIdle, "", ""); err != nil {
			slog.Error("reconciler: failed to reset tenant state", "tenant", t.TenantID, "err", err)
		}
		r.rdb.Del(ctx, fmt.Sprintf("router:endpoint:%s", t.TenantID))
		return
	}

	// Pod running — sync IP if it changed
	if currentIP != t.PodIP {
		slog.Info("reconciler: pod IP changed, syncing",
			"tenant", t.TenantID, "old_ip", t.PodIP, "new_ip", currentIP)
		if err := r.reg.UpdateStatus(ctx, t.TenantID, registry.StatusRunning, podName, currentIP); err != nil {
			slog.Error("reconciler: failed to update pod IP", "tenant", t.TenantID, "err", err)
			return
		}
		r.rdb.Set(ctx, fmt.Sprintf("router:endpoint:%s", t.TenantID), currentIP, 5*time.Minute)
		slog.Info("reconciler: router cache updated", "tenant", t.TenantID, "ip", currentIP)
	}
}

// reconcileProvisioningTenant handles a single provisioning tenant (extracted from syncProvisioningTenants).
func (r *Reconciler) reconcileProvisioningTenant(ctx context.Context, t *registry.TenantRecord) {
	currentIP, podFound, _ := r.getPodState(ctx, t.TenantID)

	if !podFound {
		age := time.Since(t.LastActiveAt)
		if age > 10*time.Minute {
			slog.Warn("reconciler: provisioning tenant has no pod, resetting to idle",
				"tenant", t.TenantID, "age", age.Round(time.Second))
			if err := r.reg.UpdateStatus(ctx, t.TenantID, registry.StatusIdle, "", ""); err != nil {
				slog.Error("reconciler: failed to reset provisioning tenant", "tenant", t.TenantID, "err", err)
			}
		}
		return
	}

	if currentIP != "" {
		slog.Info("reconciler: promoting provisioning tenant to running",
			"tenant", t.TenantID, "ip", currentIP)
		if err := r.reg.UpdateStatus(ctx, t.TenantID, registry.StatusRunning, t.TenantID, currentIP); err != nil {
			slog.Error("reconciler: failed to promote tenant", "tenant", t.TenantID, "err", err)
			return
		}
		r.rdb.Set(ctx, fmt.Sprintf("router:endpoint:%s", t.TenantID), currentIP, 5*time.Minute)
	}
}

// getPodState queries k8s for the current state of a tenant pod.
// Returns (ip, found, starting) where:
//   - ip: non-empty if pod is Running with an IP
//   - found: true if the pod exists at all
//   - starting: true if pod exists but is still in an init/pending state
func (r *Reconciler) getPodState(ctx context.Context, podName string) (ip string, found bool, starting bool) {
	pod, err := r.cs.CoreV1().Pods(r.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", false, false
	}
	if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
		return pod.Status.PodIP, true, false
	}
	// Pod exists but not fully running — check if it's still starting
	isStarting := pod.Status.Phase == corev1.PodPending || pod.Status.Phase == ""
	return "", true, isStarting
}

// reconcile performs a single full reconciliation pass (safety net).
// It handles two scenarios:
//
//  1. Running tenant whose pod is missing/rescheduled — sync IP or reset to idle
//  2. Orphan pods — tenant pods in k8s not backed by a running tenant in DynamoDB
func (r *Reconciler) reconcile(ctx context.Context) {
	slog.Debug("reconciler: running full reconcile pass")
	pods, err := r.k8s.ListTenantPods(ctx, r.namespace)
	if err != nil {
		slog.Error("reconciler: failed to list tenant pods", "err", err)
		return
	}
	podMap := make(map[string]k8sclient.TenantPodInfo, len(pods))
	for _, p := range pods {
		podMap[p.Name] = p
	}
	r.syncRunningTenants(ctx, podMap)
	r.syncProvisioningTenants(ctx, podMap)
	r.cleanOrphanPods(ctx, podMap)
}

// syncRunningTenants checks all DynamoDB-running tenants against actual k8s pod state.
// podMap is a pre-fetched map of pod name → TenantPodInfo from a single ListTenantPods call.
func (r *Reconciler) syncRunningTenants(ctx context.Context, podMap map[string]k8sclient.TenantPodInfo) {
	tenants, err := r.reg.ListByStatus(ctx, registry.StatusRunning)
	if err != nil {
		slog.Error("reconciler: failed to list running tenants", "err", err)
		return
	}
	if len(tenants) == 0 {
		return
	}
	slog.Debug("reconciler: checking running tenants", "count", len(tenants))

	for _, t := range tenants {
		if ctx.Err() != nil {
			return
		}

		podName := t.TenantID
		pod, podFound := podMap[podName]

		// Determine current IP: pod must exist, be Running, and have an IP
		currentIP := ""
		if podFound && pod.Phase == corev1.PodRunning && pod.IP != "" {
			currentIP = pod.IP
		}

		if currentIP == "" {
			// IP empty — check if pod exists but is still starting (Init/Pending/ContainerCreating)
			if podFound {
				// Pod is starting up — leave it alone, it will become ready soon
				slog.Debug("reconciler: pod starting, skipping reset", "tenant", t.TenantID)
				continue
			}
			// Pod truly missing — auto-restart if within idle timeout, otherwise reset to idle
			if r.shouldAutoRestart(t) {
				slog.Info("reconciler: pod missing within idle window, auto-restarting",
					"tenant", t.TenantID,
					"last_active", t.LastActiveAt.Format(time.RFC3339),
					"idle_timeout_s", t.IdleTimeoutS)
				r.rdb.Del(ctx, fmt.Sprintf("router:endpoint:%s", t.TenantID))
				if _, err := r.wake(ctx, t.TenantID); err != nil {
					slog.Error("reconciler: auto-restart failed, resetting to idle",
						"tenant", t.TenantID, "err", err)
					if err := r.reg.UpdateStatus(ctx, t.TenantID, registry.StatusIdle, "", ""); err != nil {
						slog.Error("reconciler: failed to reset tenant state", "tenant", t.TenantID, "err", err)
					}
				}
				continue
			}
			slog.Warn("reconciler: pod missing, resetting to idle", "tenant", t.TenantID)
			if err := r.reg.UpdateStatus(ctx, t.TenantID, registry.StatusIdle, "", ""); err != nil {
				slog.Error("reconciler: failed to reset tenant state", "tenant", t.TenantID, "err", err)
			}
			r.rdb.Del(ctx, fmt.Sprintf("router:endpoint:%s", t.TenantID))
			continue
		}

		// Pod running — sync IP if it changed (e.g. pod rescheduled with new IP)
		if currentIP != t.PodIP {
			slog.Info("reconciler: pod IP changed, syncing",
				"tenant", t.TenantID, "old_ip", t.PodIP, "new_ip", currentIP)
			if err := r.reg.UpdateStatus(ctx, t.TenantID, registry.StatusRunning, podName, currentIP); err != nil {
				slog.Error("reconciler: failed to update pod IP", "tenant", t.TenantID, "err", err)
				continue
			}
			r.rdb.Set(ctx, fmt.Sprintf("router:endpoint:%s", t.TenantID), currentIP, 5*time.Minute)
			slog.Info("reconciler: router cache updated", "tenant", t.TenantID, "ip", currentIP)
		}
	}
}

// syncProvisioningTenants promotes provisioning tenants to running when their pod
// is actually up. This handles the case where the wake flow's WaitPodReady times out
// (e.g. waiting for kata-runtime on a new node), but the pod eventually starts.
func (r *Reconciler) syncProvisioningTenants(ctx context.Context, podMap map[string]k8sclient.TenantPodInfo) {
	tenants, err := r.reg.ListByStatus(ctx, registry.StatusProvisioning)
	if err != nil {
		slog.Error("reconciler: failed to list provisioning tenants", "err", err)
		return
	}
	for _, t := range tenants {
		if ctx.Err() != nil {
			return
		}
		pod, found := podMap[t.TenantID]
		if !found {
			// No pod at all — if provisioning for too long, reset to idle
			age := time.Since(t.LastActiveAt)
			if age > 10*time.Minute {
				slog.Warn("reconciler: provisioning tenant has no pod, resetting to idle",
					"tenant", t.TenantID, "age", age.Round(time.Second))
				if err := r.reg.UpdateStatus(ctx, t.TenantID, registry.StatusIdle, "", ""); err != nil {
					slog.Error("reconciler: failed to reset provisioning tenant", "tenant", t.TenantID, "err", err)
				}
			}
			continue
		}
		if pod.Phase == corev1.PodRunning && pod.IP != "" {
			slog.Info("reconciler: promoting provisioning tenant to running",
				"tenant", t.TenantID, "ip", pod.IP)
			if err := r.reg.UpdateStatus(ctx, t.TenantID, registry.StatusRunning, t.TenantID, pod.IP); err != nil {
				slog.Error("reconciler: failed to promote tenant", "tenant", t.TenantID, "err", err)
				continue
			}
			r.rdb.Set(ctx, fmt.Sprintf("router:endpoint:%s", t.TenantID), pod.IP, 5*time.Minute)
		}
	}
}

// cleanOrphanPods deletes tenant pods that have no corresponding running tenant in DynamoDB.
// This handles cases like: manual pod creation, crashed wake flows, or leftover pods after
// tenant deletion.
// Grace period: pods younger than orphanGracePeriod are never deleted — they may be in the
// process of being registered (wake flow: pod created → DynamoDB updated). Without this,
// a reconciler replica can delete a pod that another replica is still waking.
const orphanGracePeriod = 90 * time.Second

func (r *Reconciler) cleanOrphanPods(ctx context.Context, podMap map[string]k8sclient.TenantPodInfo) {
	if len(podMap) == 0 {
		return
	}

	// Build set of tenantIDs with running or provisioning status in DynamoDB.
	// Provisioning tenants have pods that may still be starting (e.g. waiting
	// for kata-runtime DaemonSet on a new node). Deleting them would break
	// the wake flow.
	running, err := r.reg.ListByStatus(ctx, registry.StatusRunning)
	if err != nil {
		slog.Error("reconciler: failed to list running tenants for orphan check", "err", err)
		return
	}
	provisioning, err := r.reg.ListByStatus(ctx, registry.StatusProvisioning)
	if err != nil {
		slog.Error("reconciler: failed to list provisioning tenants for orphan check", "err", err)
		return
	}
	activeSet := make(map[string]bool, len(running)+len(provisioning))
	for _, t := range running {
		activeSet[t.TenantID] = true
	}
	for _, t := range provisioning {
		activeSet[t.TenantID] = true
	}

	for _, pod := range podMap {
		tenantID := pod.Name
		if activeSet[tenantID] {
			continue // backed by a running or provisioning tenant — OK
		}
		// Grace period: skip recently created pods (wake flow may still be in progress)
		age := time.Since(pod.CreatedAt)
		if age < orphanGracePeriod {
			slog.Debug("reconciler: orphan pod within grace period, skipping",
				"pod", pod.Name, "age", age.Round(time.Second))
			continue
		}
		slog.Warn("reconciler: orphan pod found, deleting", "pod", pod.Name, "tenant", tenantID, "age", age.Round(time.Second))
		if err := r.k8s.DeletePod(ctx, pod.Name, r.namespace, 10); err != nil {
			slog.Error("reconciler: failed to delete orphan pod", "pod", pod.Name, "err", err)
		}
	}
}

// shouldAutoRestart returns true if the tenant's pod disappeared unexpectedly
// (i.e. the tenant was still within its idle timeout window) and auto-restart
// is enabled (wake function is set).
func (r *Reconciler) shouldAutoRestart(t *registry.TenantRecord) bool {
	if r.wake == nil {
		return false
	}
	if t.IdleTimeoutS <= 0 {
		return false
	}
	elapsed := time.Since(t.LastActiveAt)
	return elapsed < time.Duration(t.IdleTimeoutS)*time.Second
}
