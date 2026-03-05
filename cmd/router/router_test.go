package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rewriteTransport redirects all outgoing requests to a fixed target base URL.
type rewriteTransport struct {
	target    string
	transport http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	newURL := t.target + req.URL.Path
	if req.URL.RawQuery != "" {
		newURL += "?" + req.URL.RawQuery
	}
	newReq := req.Clone(req.Context())
	newReq.URL, _ = req.URL.Parse(newURL)
	rt := t.transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(newReq)
}

func newTestRouter(orchURL string) *Router {
	mr, _ := miniredis.Run()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return &Router{
		rdb:              rdb,
		orchestratorAddr: orchURL,
		publicBaseURL:    "https://test.example.com",
		httpClient:       &http.Client{Timeout: 10 * time.Second},
	}
}

// newTestRouterWithMR returns both the router and the miniredis instance
// for tests that need to pre-populate the cache.
func newTestRouterWithMR(orchURL string) (*Router, *miniredis.Miniredis) {
	mr, _ := miniredis.Run()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rt := &Router{
		rdb:              rdb,
		orchestratorAddr: orchURL,
		publicBaseURL:    "https://test.example.com",
		httpClient:       &http.Client{Timeout: 10 * time.Second},
	}
	return rt, mr
}

// ── webhookHandler ────────────────────────────────────────────────────────────

// TestWebhookHandler_ProxiesToPod verifies the update body is forwarded
// to the pod's OpenClaw webhook server at /telegram-webhook.
// When pod is already in cache (ready), Router returns 200 immediately.
func TestWebhookHandler_ForwardsToPod(t *testing.T) {
	var receivedPath string
	var receivedBody []byte
	var receivedSecret string
	done := make(chan struct{})

	// Mock OpenClaw pod webhook server (acts as pod:8787)
	podServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedBody, _ = io.ReadAll(r.Body)
		receivedSecret = r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
		w.WriteHeader(http.StatusOK)
		select {
		case done <- struct{}{}:
		default:
		}
	}))
	defer podServer.Close()

	podAddr := podServer.Listener.Addr().String()
	podIP := strings.Split(podAddr, ":")[0]
	podPort := strings.Split(podAddr, ":")[1]

	orchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/wake/") {
			json.NewEncoder(w).Encode(map[string]string{"pod_ip": podIP})
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer orchServer.Close()

	rt := newTestRouter(orchServer.URL)
	rt.testWebhookPort = podPort

	// Pre-warm cache so pod is "ready"
	rt.rdb.Set(context.Background(), cacheKeyPrefix+"test-tenant", podIP, endpointCacheTTL)

	r := chi.NewRouter()
	r.Post("/tg/{tenantID}", rt.webhookHandler)

	update := `{"update_id":1,"message":{"message_id":1,"chat":{"id":42},"text":"hi"}}`
	req := httptest.NewRequest(http.MethodPost, "/tg/test-tenant", bytes.NewBufferString(update))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "test-secret")
	w := httptest.NewRecorder()

	start := time.Now()
	r.ServeHTTP(w, req)

	// Pod is ready → must return 200 immediately
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Less(t, time.Since(start), 500*time.Millisecond, "must ack Telegram immediately")

	// Wait for async forward
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pod server never received the forwarded update")
	}

	assert.Equal(t, "/telegram-webhook", receivedPath)
	assert.Contains(t, string(receivedBody), "update_id")
	assert.Equal(t, "test-secret", receivedSecret)
}

// TestWebhookHandler_Returns200WhenPodNotReady — Telegram treats 4xx as errors
// and enters exponential backoff. Router must always return 200, even when pod
// is not ready. Wake is triggered async.
func TestWebhookHandler_Returns200WhenPodNotReady(t *testing.T) {
	orchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/wake/") {
			json.NewEncoder(w).Encode(map[string]string{"pod_ip": "10.0.0.1"})
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer orchServer.Close()

	rt := newTestRouter(orchServer.URL)
	// No cache entry → pod not ready
	r := chi.NewRouter()
	r.Post("/tg/{tenantID}", rt.webhookHandler)

	req := httptest.NewRequest(http.MethodPost, "/tg/cold-tenant",
		bytes.NewBufferString(`{"update_id":3}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Must return 200 — never 429 to Telegram
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestWebhookHandler_ReturnsOKWhenWakeFails — always returns 200 to Telegram,
// even when forward to pod fails (error is handled async)
func TestWebhookHandler_ReturnsOKWhenWakeFails(t *testing.T) {
	orchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer orchServer.Close()

	rt := newTestRouter(orchServer.URL)

	r := chi.NewRouter()
	r.Post("/tg/{tenantID}", rt.webhookHandler)

	req := httptest.NewRequest(http.MethodPost, "/tg/bad-tenant",
		bytes.NewBufferString(`{"update_id":2}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestWebhookHandler_MissingTenantID — chi returns 404
func TestWebhookHandler_MissingTenantID(t *testing.T) {
	rt := newTestRouter("http://localhost:9999")
	r := chi.NewRouter()
	r.Post("/tg/{tenantID}", rt.webhookHandler)

	req := httptest.NewRequest(http.MethodPost, "/tg/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestForwardUpdate_RetriesOnStaleCacheIP — if cached pod IP is stale (pod deleted),
// forwardUpdate clears the bad cache entry, wakes a fresh pod, waits for gateway healthz,
// and delivers the update.
func TestForwardUpdate_RetriesOnStaleCacheIP(t *testing.T) {
	done := make(chan struct{}, 1)
	firstAttempt := make(chan struct{})

	// The "real" new pod server: serves both /healthz (gateway) and /telegram-webhook
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/telegram-webhook", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		done <- struct{}{}
	})
	newPodServer := httptest.NewServer(mux)
	defer newPodServer.Close()
	newPort := strings.Split(newPodServer.Listener.Addr().String(), ":")[1]

	// Orchestrator: signals firstAttempt on /wake/ so test can switch port before retry proceeds
	orchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/wake/") {
			// Signal that first postToPod already failed and we're in retry wake
			select {
			case firstAttempt <- struct{}{}:
			default:
			}
			json.NewEncoder(w).Encode(map[string]string{"pod_ip": "127.0.0.1"})
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer orchServer.Close()

	// Closed server: simulates a deleted pod (connection refused)
	closedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedPort := strings.Split(closedSrv.Listener.Addr().String(), ":")[1]
	closedSrv.Close()

	rt := newTestRouter(orchServer.URL)
	rt.testWebhookPort = closedPort
	rt.testGatewayPort = closedPort // gateway also unreachable initially

	// Cache a stale IP (127.0.0.1:closedPort will refuse)
	rt.rdb.Set(context.Background(), cacheKeyPrefix+"retry-tenant", "127.0.0.1", endpointCacheTTL)

	// Switch to working port as soon as orchestrator /wake/ is called
	go func() {
		<-firstAttempt
		rt.testWebhookPort = newPort
		rt.testGatewayPort = newPort
	}()

	rt.forwardUpdate("retry-tenant", []byte(`{"update_id":10}`), "secret")

	select {
	case <-done:
		// retry delivered to new pod ✓
	case <-time.After(15 * time.Second):
		t.Fatal("retry never reached new pod server")
	}
}

// ── waitForGateway / healthz polling ──────────────────────────────────────────

// TestWaitForGateway_ImmediateReady — gateway is already healthy, returns immediately
func TestWaitForGateway_ImmediateReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()
	port := strings.Split(srv.Listener.Addr().String(), ":")[1]

	rt := newTestRouter("http://localhost:9999")
	rt.testGatewayPort = port

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	resultIP, err := rt.waitForGateway(ctx, "127.0.0.1", "test-tenant")
	assert.NoError(t, err)
	assert.Equal(t, "127.0.0.1", resultIP)
	assert.Less(t, time.Since(start), 1*time.Second, "should return immediately when gateway is ready")
}

// TestWaitForGateway_BecomesReady — gateway is initially unavailable, becomes ready after a delay
func TestWaitForGateway_BecomesReady(t *testing.T) {
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			select {
			case <-ready:
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("ok"))
			default:
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}
	}))
	defer srv.Close()
	port := strings.Split(srv.Listener.Addr().String(), ":")[1]

	rt := newTestRouter("http://localhost:9999")
	rt.testGatewayPort = port

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Make gateway ready after 2s (within the 3s ticker)
	go func() {
		time.Sleep(2 * time.Second)
		close(ready)
	}()

	start := time.Now()
	resultIP, err := rt.waitForGateway(ctx, "127.0.0.1", "test-tenant")
	elapsed := time.Since(start)
	assert.NoError(t, err)
	assert.Equal(t, "127.0.0.1", resultIP)
	assert.Greater(t, elapsed, 1*time.Second, "should have polled at least once")
	assert.Less(t, elapsed, 10*time.Second, "should find gateway within a few poll cycles")
}

// TestWaitForGateway_Timeout — context expires before gateway becomes ready
func TestWaitForGateway_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	port := strings.Split(srv.Listener.Addr().String(), ":")[1]

	rt := newTestRouter("http://localhost:9999")
	rt.testGatewayPort = port

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	_, err := rt.waitForGateway(ctx, "127.0.0.1", "test-tenant")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "gateway not ready within timeout")
}


// TestWaitForGateway_IPChange — pod IP changes mid-poll (simulates Kata SandboxChanged).
// Starts polling 127.0.0.1 which has nothing listening (connection refused).
// After 5 ticks (15s), refreshPodIP returns 127.0.0.2 which serves healthy /healthz.
// Verifies the function detects the change and returns the new IP.
func TestWaitForGateway_IPChange(t *testing.T) {
	// Bind healthz server to 127.0.0.2 (Linux loopback alias)
	listener, err := net.Listen("tcp", "127.0.0.2:0")
	require.NoError(t, err)
	healthzSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})}
	go healthzSrv.Serve(listener)
	defer healthzSrv.Close()
	newPort := strings.Split(listener.Addr().String(), ":")[1]

	// Orchestrator returns 127.0.0.2 as the new pod IP
	orchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tenants/") {
			json.NewEncoder(w).Encode(map[string]string{"pod_ip": "127.0.0.2"})
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer orchServer.Close()

	rt, mr := newTestRouterWithMR(orchServer.URL)
	defer mr.Close()
	rt.testGatewayPort = newPort

	// Give enough time for 5+ ticks (5 * 3s = 15s) + buffer
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	start := time.Now()
	// Start with 127.0.0.1 — nothing listening on newPort there, so healthz will fail
	resultIP, err := rt.waitForGateway(ctx, "127.0.0.1", "ip-change-tenant")
	elapsed := time.Since(start)

	assert.NoError(t, err)
	assert.Equal(t, "127.0.0.2", resultIP, "should return the new pod IP")
	assert.Greater(t, elapsed, 14*time.Second, "should take at least 5 ticks (15s) before IP refresh")
	assert.Less(t, elapsed, 22*time.Second, "should complete within reasonable time after IP refresh")

	// Verify Redis cache was updated with new IP
	cachedIP, cacheErr := rt.rdb.Get(ctx, cacheKeyPrefix+"ip-change-tenant").Result()
	assert.NoError(t, cacheErr)
	assert.Equal(t, "127.0.0.2", cachedIP, "Redis cache should have the new IP")
}

// TestRefreshPodIP_Success — orchestrator returns a valid pod IP
func TestRefreshPodIP_Success(t *testing.T) {
	orchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tenants/test-tenant" && r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]string{"pod_ip": "10.0.0.5"})
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer orchServer.Close()

	rt := newTestRouter(orchServer.URL)
	ip, err := rt.refreshPodIP(context.Background(), "test-tenant")
	assert.NoError(t, err)
	assert.Equal(t, "10.0.0.5", ip)
}

// TestRefreshPodIP_OrchestratorError — orchestrator returns error, refreshPodIP returns error
func TestRefreshPodIP_OrchestratorError(t *testing.T) {
	orchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer orchServer.Close()

	rt := newTestRouter(orchServer.URL)
	ip, err := rt.refreshPodIP(context.Background(), "test-tenant")
	assert.Error(t, err)
	assert.Empty(t, ip)
}

// TestRefreshPodIP_Unreachable — orchestrator is unreachable
func TestRefreshPodIP_Unreachable(t *testing.T) {
	rt := newTestRouter("http://127.0.0.1:1") // nothing on port 1
	ip, err := rt.refreshPodIP(context.Background(), "test-tenant")
	assert.Error(t, err)
	assert.Empty(t, ip)
}

// TestForwardUpdate_ColdStartWithHealthzPoll — full cold start: wake pod, poll healthz, forward
func TestForwardUpdate_ColdStartWithHealthzPoll(t *testing.T) {
	done := make(chan struct{}, 1)

	// Pod server: serves both /healthz and /telegram-webhook
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/telegram-webhook", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		done <- struct{}{}
	})
	podServer := httptest.NewServer(mux)
	defer podServer.Close()
	podPort := strings.Split(podServer.Listener.Addr().String(), ":")[1]

	orchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/wake/") {
			json.NewEncoder(w).Encode(map[string]string{"pod_ip": "127.0.0.1"})
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer orchServer.Close()

	rt := newTestRouter(orchServer.URL)
	rt.testWebhookPort = podPort
	rt.testGatewayPort = podPort
	// No cache entry → full cold start path

	rt.forwardUpdate("cold-tenant", []byte(`{"update_id":20,"message":{"chat":{"id":99}}}`), "")

	select {
	case <-done:
		// message delivered after healthz poll ✓
	case <-time.After(10 * time.Second):
		t.Fatal("cold start forward never reached pod server")
	}
}

// TestForwardUpdate_CachedPodRetry_WaitsForGateway — cached pod returns 5xx (postRetry),
// router waits for gateway healthz then delivers.
func TestForwardUpdate_CachedPodRetry_WaitsForGateway(t *testing.T) {
	done := make(chan struct{}, 1)
	var webhookAttempts int

	// Pod server: first webhook attempt returns 503, then after healthz is ready, returns 200
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/telegram-webhook", func(w http.ResponseWriter, r *http.Request) {
		webhookAttempts++
		if webhookAttempts <= 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		select {
		case done <- struct{}{}:
		default:
		}
	})
	podServer := httptest.NewServer(mux)
	defer podServer.Close()

	podAddr := podServer.Listener.Addr().String()
	podIP := strings.Split(podAddr, ":")[0]
	podPort := strings.Split(podAddr, ":")[1]

	orchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer orchServer.Close()

	rt := newTestRouter(orchServer.URL)
	rt.testWebhookPort = podPort
	rt.testGatewayPort = podPort

	// Pre-warm cache
	rt.rdb.Set(context.Background(), cacheKeyPrefix+"retry-gw-tenant", podIP, endpointCacheTTL)

	rt.forwardUpdate("retry-gw-tenant", []byte(`{"update_id":30}`), "")

	select {
	case <-done:
		assert.Equal(t, 2, webhookAttempts, "should have retried webhook after gateway ready")
	case <-time.After(10 * time.Second):
		t.Fatal("forward never succeeded after gateway healthz poll")
	}
}

// ── getCachedPodIP / wakePod ──────────────────────────────────────────────────

// TestGetCachedPodIP_CacheHit — returns cached pod IP without calling orchestrator
func TestGetCachedPodIP_CacheHit(t *testing.T) {
	wakeCalled := false
	orchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/wake/") {
			wakeCalled = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer orchServer.Close()

	rt, mr := newTestRouterWithMR(orchServer.URL)
	defer mr.Close()

	// Pre-warm cache via miniredis
	mr.Set(cacheKeyPrefix+"cached-tenant", "10.0.0.1")

	podIP, err := rt.getCachedPodIP(t.Context(), "cached-tenant")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1", podIP)
	assert.False(t, wakeCalled, "should not call /wake when pod IP is cached")
}

// TestWakePod_ColdStart — calls orchestrator /wake when no cached pod IP
func TestWakePod_ColdStart(t *testing.T) {
	orchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/wake/") {
			json.NewEncoder(w).Encode(map[string]string{"pod_ip": "10.0.0.2"})
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer orchServer.Close()

	rt := newTestRouter(orchServer.URL)
	podIP, err := rt.wakePod(t.Context(), "cold-tenant")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.2", podIP)
}

// ── RegisterWebhook ───────────────────────────────────────────────────────────

// TestRegisterWebhook_SetsCorrectURL — drop_pending_updates=false so queued
// messages are forwarded to OpenClaw after pod wakes.
func TestRegisterWebhook_SetsCorrectURL(t *testing.T) {
	var received map[string]interface{}

	tgServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer tgServer.Close()

	rt := newTestRouter("http://localhost:9999")
	rt.httpClient = &http.Client{
		Timeout:   5 * time.Second,
		Transport: &rewriteTransport{target: tgServer.URL},
	}

	err := rt.RegisterWebhook("bot123:TOKEN", "tenant-abc")
	assert.NoError(t, err)
	assert.Equal(t, "https://test.example.com/tg/tenant-abc", received["url"])
	assert.Equal(t, false, received["drop_pending_updates"])
}
