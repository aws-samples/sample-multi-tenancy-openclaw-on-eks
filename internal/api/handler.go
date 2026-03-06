package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"
	k8sclient "github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/k8s"
	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/lock"
	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/registry"
	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/telegram"
)

// validTenantID matches valid DNS labels: lowercase alphanumeric and hyphens,
// 1-63 chars, must start and end with an alphanumeric character.
var validTenantID = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

const routerEndpointCachePrefix = "router:endpoint:"

// Config holds orchestrator API configuration
type Config struct {
	Namespace    string
	S3Bucket     string
	WakeLockTTL  time.Duration
	PodReadyWait time.Duration
}

// Handler is the main orchestrator HTTP handler
type Handler struct {
	reg  registry.Client
	k8s  *k8sclient.Client
	lock lock.Locker
	rdb  *redis.Client
	tg   *telegram.Client // nil if ROUTER_PUBLIC_URL not set
	wp   WarmPoolManager   // nil until SetWarmPool called
	cfg  Config
}

func New(reg registry.Client, k8s *k8sclient.Client, locker lock.Locker, rdb *redis.Client, tg *telegram.Client, cfg Config) *Handler {
	if cfg.WakeLockTTL == 0 {
		cfg.WakeLockTTL = 240 * time.Second
	}
	if cfg.PodReadyWait == 0 {
		cfg.PodReadyWait = 210 * time.Second
	}
	return &Handler{reg: reg, k8s: k8s, lock: locker, rdb: rdb, tg: tg, cfg: cfg}
}

// Router returns the chi router with all routes registered
func (h *Handler) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)

	// healthz before Logger so probe traffic doesn't flood logs
	r.Get("/healthz", h.Healthz)

	r.Group(func(r chi.Router) {
		r.Use(middleware.Logger)
		r.Post("/tenants", h.CreateTenant)
		r.Get("/tenants", h.ListTenants)
		r.Get("/tenants/{tenantID}", h.GetTenant)
		r.Get("/tenants/{tenantID}/bot_token", h.GetBotToken)
		r.Get("/tenants/{tenantID}/hooks_token", h.GetHooksToken)
		r.Patch("/tenants/{tenantID}", h.UpdateTenant)
		r.Delete("/tenants/{tenantID}", h.DeleteTenant)
		r.Put("/tenants/{tenantID}/activity", h.UpdateActivity)
		r.Post("/wake/{tenantID}", h.Wake)
		r.Get("/config", h.GetConfig)
		r.Put("/config", h.UpdateConfig)
	})

	return r
}

// Healthz returns 200 OK
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// CreateTenant creates a new tenant record
func (h *Handler) CreateTenant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TenantID     string `json:"tenant_id"`
		IdleTimeoutS int64  `json:"idle_timeout_s"`
		BotToken     string `json:"bot_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !validTenantID.MatchString(req.TenantID) {
		http.Error(w, "bad request: tenant_id must be a valid DNS label (lowercase alphanumeric and hyphens, 1-63 chars, must start/end with alphanumeric)", http.StatusBadRequest)
		return
	}
	if req.IdleTimeoutS == 0 {
		req.IdleTimeoutS = 300
	}

	// Auto-generate Hooks API token + Telegram webhook secret for OpenClaw
	hooksToken, err := registry.GenerateHooksToken()
	if err != nil {
		slog.Error("generate hooks token failed", "tenant", req.TenantID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	webhookSecret, err := registry.GenerateHooksToken()
	if err != nil {
		slog.Error("generate webhook secret failed", "tenant", req.TenantID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rec := &registry.TenantRecord{
		TenantID:      req.TenantID,
		Status:        registry.StatusIdle,
		Namespace:     h.cfg.Namespace,
		S3Prefix:      fmt.Sprintf("tenants/%s/", req.TenantID),
		BotToken:      req.BotToken,
		HooksToken:    hooksToken,
		WebhookSecret: webhookSecret,
		CreatedAt:     time.Now().UTC(),
		LastActiveAt:  time.Now().UTC(),
		IdleTimeoutS:  req.IdleTimeoutS,
	}
	if err := h.reg.CreateTenant(r.Context(), rec); err != nil {
		slog.Error("create tenant failed", "tenant", req.TenantID, "err", err)
		http.Error(w, "conflict", http.StatusConflict)
		return
	}
	// Auto-register Telegram webhook if router URL is configured and bot token provided
	if h.tg != nil && req.BotToken != "" {
		if err := h.tg.RegisterWebhook(r.Context(), req.BotToken, req.TenantID); err != nil {
			slog.Warn("webhook registration failed (tenant created, fix manually)", "tenant", req.TenantID, "err", err)
		} else {
			slog.Info("webhook registered", "tenant", req.TenantID)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	rec.BotToken = ""
	rec.HooksToken = ""
	rec.WebhookSecret = ""
	json.NewEncoder(w).Encode(rec)
}

// ListTenants returns all tenant records (secrets redacted)
func (h *Handler) ListTenants(w http.ResponseWriter, r *http.Request) {
	records, err := h.reg.ListAll(r.Context())
	if err != nil {
		slog.Error("list tenants failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if records == nil {
		records = []*registry.TenantRecord{}
	}
	// Redact secrets from all records
	for _, rec := range records {
		rec.BotToken = ""
		rec.HooksToken = ""
		rec.WebhookSecret = ""
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(records)
}

// GetTenant returns a tenant record (secrets redacted)
func (h *Handler) GetTenant(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantID")
	rec, err := h.reg.GetTenant(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if rec == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rec.BotToken = ""
	rec.HooksToken = ""
	rec.WebhookSecret = ""
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rec)
}

// GetBotToken returns only the bot_token for a tenant (internal use by Router)
func (h *Handler) GetBotToken(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantID")
	rec, err := h.reg.GetTenant(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if rec == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"BotToken": rec.BotToken})
}

// GetHooksToken returns only the hooks_token for a tenant (internal use by Router)
func (h *Handler) GetHooksToken(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantID")
	rec, err := h.reg.GetTenant(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if rec == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"HooksToken": rec.HooksToken})
}

// UpdateTenant updates mutable tenant fields (currently: bot_token, idle_timeout_s)
func (h *Handler) UpdateTenant(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantID")
	var req struct {
		BotToken     *string `json:"bot_token"`
		IdleTimeoutS *int64  `json:"idle_timeout_s"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.BotToken != nil {
		if err := h.reg.UpdateBotToken(r.Context(), tenantID, *req.BotToken); err != nil {
			slog.Error("update bot_token failed", "tenant", tenantID, "err", err)
			http.Error(w, "not found or internal error", http.StatusNotFound)
			return
		}
		// Re-register webhook with new token
		if h.tg != nil && *req.BotToken != "" {
			if err := h.tg.RegisterWebhook(r.Context(), *req.BotToken, tenantID); err != nil {
				slog.Warn("webhook re-registration failed (token updated, fix manually)", "tenant", tenantID, "err", err)
			} else {
				slog.Info("webhook re-registered", "tenant", tenantID)
			}
		}
	}
	if req.IdleTimeoutS != nil {
		if err := h.reg.UpdateIdleTimeout(r.Context(), tenantID, *req.IdleTimeoutS); err != nil {
			slog.Error("update idle_timeout_s failed", "tenant", tenantID, "err", err)
			http.Error(w, "not found or internal error", http.StatusNotFound)
			return
		}
	}
	rec, err := h.reg.GetTenant(r.Context(), tenantID)
	if err != nil || rec == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rec.BotToken = ""
	rec.HooksToken = ""
	rec.WebhookSecret = ""
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rec)
}

// DeleteTenant removes a tenant and all its resources
func (h *Handler) DeleteTenant(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantID")
	rec, err := h.reg.GetTenant(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if rec == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if rec.PodName != "" && h.k8s != nil {
		if err := h.k8s.DeletePod(r.Context(), rec.PodName, rec.Namespace, 120); err != nil {
			slog.Error("delete pod failed", "tenant", tenantID, "err", err)
		}
	}
	if err := h.reg.DeleteTenant(r.Context(), tenantID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Clear Redis endpoint cache so Router doesn't serve stale IP
	if h.rdb != nil {
		cacheKey := routerEndpointCachePrefix + tenantID
		if err := h.rdb.Del(r.Context(), cacheKey).Err(); err != nil {
			slog.Warn("delete tenant: failed to clear Redis cache", "tenant", tenantID, "err", err)
		}
	}
	// Remove Telegram webhook
	if h.tg != nil && rec.BotToken != "" {
		if err := h.tg.DeleteWebhook(r.Context(), rec.BotToken); err != nil {
			slog.Warn("delete tenant: failed to remove webhook", "tenant", tenantID, "err", err)
		} else {
			slog.Info("webhook deleted", "tenant", tenantID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// UpdateActivity updates last_active_at for a tenant
func (h *Handler) UpdateActivity(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantID")
	if err := h.reg.UpdateActivity(r.Context(), tenantID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Wake ensures a tenant pod is running and returns its IP
func (h *Handler) Wake(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantID")
	ctx := r.Context()

	podIP, err := h.WakeOrGet(ctx, tenantID)
	if err != nil {
		slog.Error("wake failed", "tenant", tenantID, "err", err)
		http.Error(w, "failed to wake tenant", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"pod_ip": podIP})
}

// WakeOrGet returns the pod IP, starting the pod if needed.
// Exported so it can be passed to the reconciler as a WakeFunc for auto-restart.
func (h *Handler) WakeOrGet(ctx context.Context, tenantID string) (string, error) {
	if h.k8s == nil {
		return "", fmt.Errorf("k8s not available in local mode")
	}
	// Fast path: already running — verify pod actually exists before trusting cached IP
	rec, err := h.reg.GetTenant(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if rec != nil && rec.Status == registry.StatusRunning && rec.PodIP != "" {
		podName := tenantID
		ns := h.cfg.Namespace
		if ns == "" {
			ns = "tenants"
		}
		exists, err := h.k8s.PodExists(ctx, podName, ns)
		if err == nil && exists {
			return rec.PodIP, nil
		}
		// Pod is gone — fall through to slow path to recreate
		slog.Warn("wake fast path: pod missing despite running status, recreating",
			"tenant", tenantID, "stale_ip", rec.PodIP)
	}

	// Slow path: try to acquire wake lock
	acquired, err := h.lock.AcquireWakeLock(ctx, tenantID, h.cfg.WakeLockTTL)
	if err != nil {
		return "", fmt.Errorf("acquire lock: %w", err)
	}

	if !acquired {
		// Another replica is waking this tenant — poll until running
		return h.pollUntilRunning(ctx, tenantID)
	}
	defer h.lock.ReleaseWakeLock(ctx, tenantID)

	// We have the lock — ensure PVC exists, create pod, wait ready
	// Auto-create tenant if not exists (wake without prior registration)
	if rec == nil {
		hooksToken, err := registry.GenerateHooksToken()
		if err != nil {
			return "", fmt.Errorf("generate hooks token: %w", err)
		}
		rec = &registry.TenantRecord{
			TenantID:     tenantID,
			Status:       registry.StatusProvisioning,
			Namespace:    h.cfg.Namespace,
			S3Prefix:     fmt.Sprintf("tenants/%s/", tenantID),
			HooksToken:   hooksToken,
			CreatedAt:    time.Now().UTC(),
			LastActiveAt: time.Now().UTC(),
			IdleTimeoutS: 300,
		}
		_ = h.reg.CreateTenant(ctx, rec)
	} else if rec.Status != registry.StatusProvisioning {
		// Mark existing tenant as provisioning so the reconciler won't
		// treat its pod as an orphan while kata-runtime is still installing.
		if err := h.reg.UpdateStatus(ctx, tenantID, registry.StatusProvisioning, "", ""); err != nil {
			slog.Warn("failed to set provisioning status", "tenant", tenantID, "err", err)
		}
	}

	// Ensure tenant has a HooksToken (legacy tenants created before this field existed)
	if rec.HooksToken == "" {
		newToken, err := registry.GenerateHooksToken()
		if err != nil {
			return "", fmt.Errorf("generate hooks token: %w", err)
		}
		if err := h.reg.UpdateHooksToken(ctx, tenantID, newToken); err != nil {
			slog.Warn("failed to persist hooks token (will still create pod)", "tenant", tenantID, "err", err)
		}
		rec.HooksToken = newToken
		slog.Info("auto-generated hooks token for legacy tenant", "tenant", tenantID)
	}

	// Ensure tenant has a WebhookSecret (legacy tenants created before this field existed)
	if rec.WebhookSecret == "" {
		newSecret, err := registry.GenerateHooksToken()
		if err != nil {
			return "", fmt.Errorf("generate webhook secret: %w", err)
		}
		if err := h.reg.UpdateWebhookSecret(ctx, tenantID, newSecret); err != nil {
			slog.Warn("failed to persist webhook secret (will still create pod)", "tenant", tenantID, "err", err)
		}
		rec.WebhookSecret = newSecret
		slog.Info("auto-generated webhook secret for legacy tenant", "tenant", tenantID)
	}

	ns := h.cfg.Namespace
	if rec.Namespace != "" {
		ns = rec.Namespace
	}

	// Check for a warm pod — if one is available, delete it and pin the
	// tenant pod to the same node to skip Karpenter provisioning.
	nodeName := ""
	if warmPod, err := h.k8s.GetWarmPod(ctx, ns); err == nil && warmPod != nil {
		nodeName = warmPod.Spec.NodeName
		slog.Info("warm pool hit: reusing node", "tenant", tenantID, "node", nodeName, "warm_pod", warmPod.Name)
		// Delete the warm pod to free resources before creating tenant pod
		_ = h.k8s.DeletePod(ctx, warmPod.Name, ns, 0)
	} else {
		slog.Info("warm pool miss: cold start", "tenant", tenantID)
	}

	// Create pod (pinned to warm node if available)
	pod, err := h.k8s.CreateTenantPod(ctx, tenantID, ns, rec.BotToken, rec.HooksToken, rec.WebhookSecret, nodeName)
	if err != nil {
		return "", fmt.Errorf("create pod: %w", err)
	}

	// Wait ready
	podIP, err := h.k8s.WaitPodReady(ctx, tenantID, ns, h.cfg.PodReadyWait)
	if err != nil {
		return "", fmt.Errorf("wait pod ready: %w", err)
	}

	// Update registry
	if err := h.reg.UpdateStatus(ctx, tenantID, registry.StatusRunning, pod.Name, podIP); err != nil {
		return "", fmt.Errorf("update status: %w", err)
	}

	return podIP, nil
}

// pollUntilRunning waits for another replica to finish waking the tenant
func (h *Handler) pollUntilRunning(ctx context.Context, tenantID string) (string, error) {
	deadline := time.Now().Add(h.cfg.WakeLockTTL)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
		rec, err := h.reg.GetTenant(ctx, tenantID)
		if err != nil {
			continue
		}
		if rec != nil && rec.Status == registry.StatusRunning && rec.PodIP != "" {
			return rec.PodIP, nil
		}
	}
	return "", fmt.Errorf("timeout waiting for tenant %s to become running", tenantID)
}

// --- Runtime Config API ---

// RuntimeConfig represents hot-configurable orchestrator settings
type RuntimeConfig struct {
	WarmPoolTarget int `json:"warm_pool_target"`
}

// SetWarmPool sets the warm pool manager reference for hot-config updates.
func (h *Handler) SetWarmPool(wp WarmPoolManager) {
	h.wp = wp
}

// WarmPoolManager is the interface needed by the config API to read/update the warm pool target.
type WarmPoolManager interface {
	Target() int32
	SetTarget(target int32)
}

// GetConfig returns the current runtime configuration.
func (h *Handler) GetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := RuntimeConfig{}
	if h.wp != nil {
		cfg.WarmPoolTarget = int(h.wp.Target())
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}

// UpdateConfig accepts partial JSON to update runtime configuration.
func (h *Handler) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	var req RuntimeConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: invalid JSON", http.StatusBadRequest)
		return
	}
	if req.WarmPoolTarget < 0 {
		http.Error(w, "bad request: warm_pool_target must be >= 0", http.StatusBadRequest)
		return
	}
	if h.wp == nil {
		http.Error(w, "warm pool not available", http.StatusServiceUnavailable)
		return
	}
	h.wp.SetTarget(int32(req.WarmPoolTarget))

	// Return updated config
	cfg := RuntimeConfig{
		WarmPoolTarget: int(h.wp.Target()),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}
