package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"
)

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

const (
	endpointCacheTTL = 5 * time.Minute
	cacheKeyPrefix   = "router:endpoint:"
	podReadyWait     = 5 * time.Minute // Karpenter cold-start (new metal node) can take 4+ minutes
	openclawWebhookPort = 8787         // OpenClaw's internal webhook HTTP server port
)

type Router struct {
	rdb              *redis.Client
	orchestratorAddr string
	publicBaseURL    string       // e.g. https://<DOMAIN>
	httpClient       *http.Client // wake + orchestrator API calls
	testWebhookPort  string       // override webhook port in tests (empty = use openclawWebhookPort)
	testGatewayPort  string       // override webhook port in tests (empty = use 8787)
}

// ── Telegram webhook receiver + proxy ────────────────────────────

// webhookHandler receives Telegram updates for a specific tenant.
// Path: POST /tg/{tenantID}
//
// Flow:
//  1. Read the update body
//  2. Always return 200 OK immediately (Telegram requires 2xx within 5s,
//     treats 4xx/5xx as errors and enters exponential backoff)
//  3. Async: forwardUpdate handles wake + notify user + forward to pod
func (rt *Router) webhookHandler(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantID")
	if tenantID == "" {
		http.Error(w, "tenantID required", http.StatusBadRequest)
		return
	}

	// Read body before acking (body is closed after handler returns)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB max
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	secretHeader := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")

	// Always ack Telegram with 200 OK — Telegram treats anything else as error
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Async: wake pod (if needed) + notify user + forward update once pod is ready
	go rt.forwardUpdate(tenantID, body, secretHeader)
}

// waitForGateway polls the OpenClaw webhook port until it accepts connections.
// It periodically re-checks the pod IP from the orchestrator in case the pod was
// recreated (e.g. Kata SandboxChanged) and got a new IP.
// Returns the final working podIP and nil error, or empty string and error on timeout.
func (rt *Router) waitForGateway(ctx context.Context, podIP, tenantID string) (string, error) {
	port := rt.testGatewayPort
	if port == "" {
		port = "8787" // OpenClaw webhook port — ready when Telegram handler is listening
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	// Counter to check IP refresh every N ticks (e.g. every 5 ticks = 15s)
	tickCount := 0
	ipRefreshInterval := 5

	// Try immediately first
	url := fmt.Sprintf("http://%s:%s/healthz", podIP, port)
	if rt.probeHealthz(ctx, url) {
		return podIP, nil
	}

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("gateway not ready within timeout for tenant %s", tenantID)
		case <-ticker.C:
			tickCount++

			// Periodically refresh pod IP from orchestrator
			if tickCount%ipRefreshInterval == 0 {
				newIP, err := rt.refreshPodIP(ctx, tenantID)
				if err == nil && newIP != "" && newIP != podIP {
					slog.Info("pod IP changed during gateway wait", "tenant", tenantID, "old_ip", podIP, "new_ip", newIP)
					podIP = newIP
					// Update Redis cache with new IP
					rt.rdb.Set(ctx, cacheKeyPrefix+tenantID, podIP, endpointCacheTTL)
				}
			}

			url = fmt.Sprintf("http://%s:%s/healthz", podIP, port)
			if rt.probeHealthz(ctx, url) {
				slog.Info("gateway ready", "tenant", tenantID, "pod_ip", podIP)
				return podIP, nil
			}
			slog.Debug("gateway not ready yet", "tenant", tenantID, "pod_ip", podIP)
		}
	}
}

// refreshPodIP queries the orchestrator for the current pod IP of a tenant.
// Returns the pod IP or an error if the orchestrator is unreachable or returns
// an unexpected response. Callers should treat errors as non-fatal (keep using
// the current IP).
func (rt *Router) refreshPodIP(ctx context.Context, tenantID string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		fmt.Sprintf("%s/tenants/%s", rt.orchestratorAddr, tenantID), nil)
	if err != nil {
		return "", fmt.Errorf("build refresh request: %w", err)
	}

	resp, err := rt.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("refresh pod IP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("refresh pod IP: status %d", resp.StatusCode)
	}

	var result struct {
		PodIP string `json:"pod_ip"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode refresh response: %w", err)
	}
	return result.PodIP, nil
}

func (rt *Router) probeHealthz(ctx context.Context, url string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := rt.httpClient.Do(req)
	if err != nil {
		return false // connection refused = not ready
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return true // any HTTP response = webhook port is listening and ready
}

// forwardUpdate ensures the pod is awake then POSTs the Telegram update to it.
// If the cached pod IP is stale (pod was deleted), it clears the cache, notifies
// the user, wakes a new pod, and retries forwarding.
func (rt *Router) forwardUpdate(tenantID string, body []byte, secretHeader string) {
	ctx, cancel := context.WithTimeout(context.Background(), podReadyWait+30*time.Second)
	defer cancel()

	// Fast path: try cached IP first
	podIP, _ := rt.getCachedPodIP(ctx, tenantID)
	hadCachedIP := podIP != ""
	if podIP != "" {
		switch rt.postToPod(ctx, podIP, tenantID, body, secretHeader) {
		case postOK:
			rt.updateActivity(tenantID)
			return
		case postRetry:
			// Pod reachable but OpenClaw not ready — wait for gateway then deliver
			slog.Info("pod not ready yet, waiting for gateway", "tenant", tenantID)
			podIP, err := rt.waitForGateway(ctx, podIP, tenantID)
			if err != nil {
				slog.Error("gateway wait timeout", "tenant", tenantID, "err", err)
				return
			}
			// Gateway ready — deliver (podIP may have been updated)
			if rt.postToPod(ctx, podIP, tenantID, body, secretHeader) == postOK {
				rt.updateActivity(tenantID)
				return
			}
		case postFatal:
			slog.Info("stale pod IP, waking new pod", "tenant", tenantID)
			podIP = ""
		}
	}

	if podIP == "" {
		// Only send "starting up" notification on true cold starts
		// (no cached IP at all), not when cache was just invalidated
		if !hadCachedIP {
			rt.notifyStarting(tenantID, body)
		}

		var err error
		podIP, err = rt.wakePod(ctx, tenantID)
		if err != nil {
			slog.Error("wake failed, update dropped", "tenant", tenantID, "err", err)
			return
		}
		rt.rdb.Set(ctx, cacheKeyPrefix+tenantID, podIP, endpointCacheTTL)
		rt.updateActivity(tenantID)
		slog.Info("pod awake", "tenant", tenantID, "pod_ip", podIP)
	}

	// Wait for OpenClaw gateway to be ready before forwarding
	var gwErr error
	podIP, gwErr = rt.waitForGateway(ctx, podIP, tenantID)
	if gwErr != nil {
		slog.Error("gateway not ready, update dropped", "tenant", tenantID, "err", gwErr)
		return
	}

	// Gateway is ready — forward the message with a small retry window (just in case)
	// Re-set cache before retrying: postToPod may have cleared it on transient errors,
	// but we know the IP is valid since gateway healthz just passed.
	rt.rdb.Set(ctx, cacheKeyPrefix+tenantID, podIP, endpointCacheTTL)

	delays := []time.Duration{0, 2, 3, 5}
	for attempt, delay := range delays {
		if delay > 0 {
			time.Sleep(delay * time.Second)
		}
		result := rt.postToPod(ctx, podIP, tenantID, body, secretHeader)
		if result == postOK {
			rt.updateActivity(tenantID)
			return
		}
		// Re-set cache after each retry — postToPod clears it on connection errors,
		// but the IP is confirmed valid by healthz, so keep it cached.
		rt.rdb.Set(ctx, cacheKeyPrefix+tenantID, podIP, endpointCacheTTL)
		slog.Info("forward retry after gateway ready", "tenant", tenantID, "attempt", attempt+1, "result", result)
	}
	slog.Error("forward failed after gateway ready, update dropped", "tenant", tenantID)
}

// notifyStarting sends a "bot is starting" message to the user via Bot API.
func (rt *Router) notifyStarting(tenantID string, body []byte) {
	chatID := extractChatID(body)
	if chatID == 0 {
		return
	}
	botToken := rt.getBotToken(context.Background(), tenantID)
	if botToken == "" {
		return
	}
	rt.sendTelegramMessage(botToken, chatID, "🏗 Starting up, please wait...")
	slog.Info("sent starting notification", "tenant", tenantID, "chat_id", chatID)
}

// postResult indicates the outcome of a postToPod attempt.
type postResult int

const (
	postOK       postResult = iota // 2xx — delivered
	postRetry                      // 5xx or connection refused — pod starting, retry
	postFatal                      // connection error (stale IP) — invalidate cache
)

// postToPod POSTs the Telegram update body to the pod's OpenClaw webhook server.
func (rt *Router) postToPod(ctx context.Context, podIP, tenantID string, body []byte, secretHeader string) postResult {
	port := rt.testWebhookPort
	if port == "" {
		port = fmt.Sprintf("%d", openclawWebhookPort)
	}
	url := fmt.Sprintf("http://%s:%s/telegram-webhook", podIP, port)

	// Short timeout: if pod doesn't respond in 5s it's likely stale or overloaded
	postCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(postCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		slog.Error("build pod request failed", "tenant", tenantID, "err", err)
		return postFatal
	}
	req.Header.Set("Content-Type", "application/json")
	if secretHeader != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secretHeader)
	}

	resp, err := rt.httpClient.Do(req)
	if err != nil {
		// Connection error = stale IP (pod deleted/restarted) — invalidate cache
		slog.Warn("post to pod failed, invalidating cache", "tenant", tenantID, "err", err)
		rt.rdb.Del(ctx, cacheKeyPrefix+tenantID)
		return postFatal
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		slog.Info("update forwarded to pod", "tenant", tenantID, "pod_ip", podIP, "status", resp.StatusCode)
		return postOK
	}
	// 5xx = pod reachable but OpenClaw not ready yet (session loading)
	slog.Warn("pod returned error, will retry", "tenant", tenantID, "status", resp.StatusCode)
	return postRetry
}

func (rt *Router) getCachedPodIP(ctx context.Context, tenantID string) (string, error) {
	return rt.rdb.Get(ctx, cacheKeyPrefix+tenantID).Result()
}

func (rt *Router) wakePod(ctx context.Context, tenantID string) (string, error) {
	resp, err := rt.httpClient.Post(
		fmt.Sprintf("%s/wake/%s", rt.orchestratorAddr, tenantID),
		"application/json", nil,
	)
	if err != nil {
		return "", fmt.Errorf("orchestrator wake: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("wake status %d: %s", resp.StatusCode, body)
	}
	var result struct {
		PodIP string `json:"pod_ip"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode wake response: %w", err)
	}
	return result.PodIP, nil
}

func (rt *Router) updateActivity(tenantID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/tenants/%s/activity", rt.orchestratorAddr, tenantID), nil)
	rt.httpClient.Do(req)
}

// getBotToken fetches the bot token for a tenant from the orchestrator.
func (rt *Router) getBotToken(ctx context.Context, tenantID string) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := rt.httpClient.Get(fmt.Sprintf("%s/tenants/%s/bot_token", rt.orchestratorAddr, tenantID))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var rec struct {
		BotToken string `json:"BotToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return ""
	}
	return rec.BotToken
}

// sendTelegramMessage sends a message directly to a Telegram chat via Bot API.
func (rt *Router) sendTelegramMessage(botToken string, chatID int64, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken),
		bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rt.httpClient.Do(req)
}

// extractChatID extracts chat.id from a Telegram Update JSON body.
func extractChatID(body []byte) int64 {
	var update struct {
		Message *struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
		CallbackQuery *struct {
			Message *struct {
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			} `json:"message"`
		} `json:"callback_query"`
	}
	if err := json.Unmarshal(body, &update); err != nil {
		return 0
	}
	if update.Message != nil {
		return update.Message.Chat.ID
	}
	if update.CallbackQuery != nil && update.CallbackQuery.Message != nil {
		return update.CallbackQuery.Message.Chat.ID
	}
	return 0
}

// ── Webhook registration ─────────────────────────────────────────

// RegisterWebhook tells Telegram to push updates to our router for a tenant.
// drop_pending_updates=false: OpenClaw processes queued messages after pod wakes.
func (rt *Router) RegisterWebhook(botToken, tenantID string) error {
	webhookURL := fmt.Sprintf("%s/tg/%s", rt.publicBaseURL, tenantID)
	body, _ := json.Marshal(map[string]any{
		"url":                  webhookURL,
		"drop_pending_updates": false,
	})
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("https://api.telegram.org/bot%s/setWebhook", botToken),
		strings.NewReader(string(body)),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := rt.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if ok, _ := result["ok"].(bool); !ok {
		return fmt.Errorf("setWebhook failed: %v", result)
	}
	slog.Info("webhook registered", "tenant", tenantID, "url", webhookURL)
	return nil
}

// ── Main ─────────────────────────────────────────────────────────

func main() {
	redisAddr := getenv("REDIS_ADDR", "localhost:6379")
	orchestratorAddr := getenv("ORCHESTRATOR_ADDR", "http://localhost:8080")
	publicBaseURL := getenv("PUBLIC_BASE_URL", "https://<YOUR_ROUTER_DOMAIN>")
	port := getenv("PORT", "9090")

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})

	rt := &Router{
		rdb:              rdb,
		orchestratorAddr: orchestratorAddr,
		publicBaseURL:    publicBaseURL,
		httpClient:       &http.Client{Timeout: podReadyWait + 30*time.Second},
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	// healthz before Logger so probe traffic doesn't flood logs
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	r.Group(func(r chi.Router) {
		r.Use(middleware.Logger)
		// Telegram webhook receiver — wake pod + proxy to OpenClaw webhook server
		r.Post("/tg/{tenantID}", rt.webhookHandler)
	})

	srv := &http.Server{Addr: ":" + port, Handler: r}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	go func() {
		slog.Info("router listening", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("router error", "err", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)
}
