package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamotypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/redis/go-redis/v9"
	"github.com/shawn/openclaw-tenancy/internal/api"
	k8sclient "github.com/shawn/openclaw-tenancy/internal/k8s"
	"github.com/shawn/openclaw-tenancy/internal/lifecycle"
	"github.com/shawn/openclaw-tenancy/internal/lock"
	"github.com/shawn/openclaw-tenancy/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const tableName = "tenant-registry-test"

// ── Test infrastructure ───────────────────────────────────────────────────────

func setupDynamoDB(ctx context.Context, t *testing.T) (*dynamodb.Client, func()) {
	t.Helper()
	req := testcontainers.ContainerRequest{
		Image:        "amazon/dynamodb-local:latest",
		ExposedPorts: []string{"8000/tcp"},
		Cmd:          []string{"-jar", "DynamoDBLocal.jar", "-inMemory"},
		WaitingFor:   wait.ForListeningPort("8000/tcp"),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)

	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "8000/tcp")
	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	cfg, _ := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	db := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})

	_, err = db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(tableName),
		KeySchema: []dynamotypes.KeySchemaElement{
			{AttributeName: aws.String("tenant_id"), KeyType: dynamotypes.KeyTypeHash},
		},
		AttributeDefinitions: []dynamotypes.AttributeDefinition{
			{AttributeName: aws.String("tenant_id"), AttributeType: dynamotypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("status"), AttributeType: dynamotypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("last_active_at"), AttributeType: dynamotypes.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexes: []dynamotypes.GlobalSecondaryIndex{
			{
				IndexName: aws.String("status-index"),
				KeySchema: []dynamotypes.KeySchemaElement{
					{AttributeName: aws.String("status"), KeyType: dynamotypes.KeyTypeHash},
					{AttributeName: aws.String("last_active_at"), KeyType: dynamotypes.KeyTypeRange},
				},
				Projection: &dynamotypes.Projection{
					ProjectionType: dynamotypes.ProjectionTypeAll,
				},
			},
		},
		BillingMode: dynamotypes.BillingModePayPerRequest,
	})
	require.NoError(t, err)

	return db, func() { c.Terminate(ctx) }
}

func setupRedis(ctx context.Context, t *testing.T) (*redis.Client, func()) {
	t.Helper()
	req := testcontainers.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForListeningPort("6379/tcp"),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)

	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "6379/tcp")

	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%s", host, port.Port()),
	})
	return rdb, func() { c.Terminate(ctx) }
}

// newTestHandler builds a test handler backed by real DynamoDB + Redis containers
func newIntegrationHandler(t *testing.T, db *dynamodb.Client, rdb *redis.Client) (*api.Handler, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	reg := registry.New(db, tableName)
	locker := lock.New(rdb)
	k8s := k8sclient.New(cs, k8sclient.Config{
		KataRuntimeClass: "kata-qemu",
		OpenClawImage:    "openclaw:test",
		S3Bucket:         "test-bucket",
		RouterPublicURL:  "https://router.test.example.com",
	})
	h := api.New(reg, k8s, locker, nil, nil, api.Config{
		Namespace:    "tenants",
		PodReadyWait: 10 * time.Second,
	})
	return h, cs
}

// makePodReady simulates OpenClaw gateway becoming ready inside the fake k8s
func makePodReady(t *testing.T, cs *fake.Clientset, tenantID, namespace, ip string) {
	t.Helper()
	go func() {
		time.Sleep(200 * time.Millisecond)
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

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestIntegration_WakeIdleWakeCycle: full lifecycle — wake → idle timeout → wake again
func TestIntegration_WakeIdleWakeCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()

	db, cleanDB := setupDynamoDB(ctx, t)
	defer cleanDB()
	rdb, cleanRedis := setupRedis(ctx, t)
	defer cleanRedis()

	h, cs := newIntegrationHandler(t, db, rdb)
	srv := httptest.NewServer(h.Router())
	defer srv.Close()

	reg := registry.New(db, tableName)
	tenantID := "integration-tenant"

	// ── Step 1: Create tenant via API ───────────────────────────────────────
	body, _ := json.Marshal(map[string]interface{}{
		"tenant_id":      tenantID,
		"bot_token":      "test:fake_bot_token",
		"idle_timeout_s": 300,
	})
	resp, err := http.Post(srv.URL+"/tenants", "application/json",
		jsonReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	// Verify HooksToken was auto-generated
	rec, err := reg.GetTenant(ctx, tenantID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.NotEmpty(t, rec.HooksToken, "HooksToken must be auto-generated on tenant creation")
	assert.Len(t, rec.HooksToken, 64, "HooksToken should be 32 bytes hex = 64 chars")

	// Verify secrets are NOT in the API response
	var createResp map[string]interface{}
	// (body already closed — we just check the registry)

	// ── Step 2: Wake the tenant ─────────────────────────────────────────────
	makePodReady(t, cs, tenantID, "tenants", "10.1.0.1")
	wakeResp, err := http.Post(srv.URL+"/wake/"+tenantID, "application/json", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, wakeResp.StatusCode)

	var wakeResult map[string]string
	json.NewDecoder(wakeResp.Body).Decode(&wakeResult)
	wakeResp.Body.Close()
	assert.Equal(t, "10.1.0.1", wakeResult["pod_ip"])

	rec, err = reg.GetTenant(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, registry.StatusRunning, rec.Status)
	assert.Equal(t, "10.1.0.1", rec.PodIP)
	assert.NotEmpty(t, rec.HooksToken, "HooksToken must survive wake cycle")

	// ── Step 3: GET /tenants/:id/hooks_token returns correct token ──────────
	hooksResp, err := http.Get(srv.URL + "/tenants/" + tenantID + "/hooks_token")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, hooksResp.StatusCode)
	var hooksResult map[string]string
	json.NewDecoder(hooksResp.Body).Decode(&hooksResult)
	hooksResp.Body.Close()
	assert.Equal(t, rec.HooksToken, hooksResult["HooksToken"])

	// ── Step 4: GET /tenants/:id must NOT expose secrets ────────────────────
	tenantResp, err := http.Get(srv.URL + "/tenants/" + tenantID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, tenantResp.StatusCode)
	json.NewDecoder(tenantResp.Body).Decode(&createResp)
	tenantResp.Body.Close()
	assert.Empty(t, createResp["bot_token"], "bot_token must be redacted in GET /tenants/:id")
	assert.Empty(t, createResp["hooks_token"], "hooks_token must be redacted in GET /tenants/:id")

	// ── Step 5: Simulate idle timeout ───────────────────────────────────────
	require.NoError(t, reg.UpdateStatus(ctx, tenantID, registry.StatusRunning, tenantID, "10.1.0.1"))
	_, err = db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]dynamotypes.AttributeValue{
			"tenant_id": &dynamotypes.AttributeValueMemberS{Value: tenantID},
		},
		UpdateExpression: aws.String("SET last_active_at = :old"),
		ExpressionAttributeValues: map[string]dynamotypes.AttributeValue{
			":old": &dynamotypes.AttributeValueMemberS{Value: time.Now().Add(-10 * time.Minute).Format(time.RFC3339)},
		},
	})
	require.NoError(t, err)

	k8s := k8sclient.New(cs, k8sclient.Config{OpenClawImage: "openclaw:test"})
	ctrl := lifecycle.NewForTest(reg, k8s)
	ctrl.CheckIdleTenants(ctx)

	rec, err = reg.GetTenant(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, registry.StatusIdle, rec.Status)
	assert.Empty(t, rec.PodIP)
	assert.NotEmpty(t, rec.HooksToken, "HooksToken must survive idle transition")

	_, err = cs.CoreV1().Pods("tenants").Get(ctx, tenantID, metav1.GetOptions{})
	assert.Error(t, err, "pod should have been deleted after idle timeout")

	// ── Step 6: Wake again from idle ────────────────────────────────────────
	makePodReady(t, cs, tenantID, "tenants", "10.1.0.2")
	wake2Resp, err := http.Post(srv.URL+"/wake/"+tenantID, "application/json", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, wake2Resp.StatusCode)

	var wake2Result map[string]string
	json.NewDecoder(wake2Resp.Body).Decode(&wake2Result)
	wake2Resp.Body.Close()
	assert.Equal(t, "10.1.0.2", wake2Result["pod_ip"])

	rec2, err := reg.GetTenant(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, registry.StatusRunning, rec2.Status)
	assert.Equal(t, "10.1.0.2", rec2.PodIP)
	// HooksToken must be stable across the full lifecycle
	assert.Equal(t, rec.HooksToken, rec2.HooksToken, "HooksToken must be stable across wake cycles")
}

// TestIntegration_MultiTenantIsolation: two tenants do not share tokens or pod IPs
func TestIntegration_MultiTenantIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()

	db, cleanDB := setupDynamoDB(ctx, t)
	defer cleanDB()
	rdb, cleanRedis := setupRedis(ctx, t)
	defer cleanRedis()

	h, cs := newIntegrationHandler(t, db, rdb)
	srv := httptest.NewServer(h.Router())
	defer srv.Close()

	reg := registry.New(db, tableName)

	tenants := []struct {
		id string
		ip string
	}{
		{"tenant-alpha", "10.2.0.1"},
		{"tenant-beta", "10.2.0.2"},
	}

	// Create both tenants
	for _, tc := range tenants {
		body, _ := json.Marshal(map[string]interface{}{
			"tenant_id": tc.id,
			"bot_token": "token:" + tc.id,
		})
		resp, err := http.Post(srv.URL+"/tenants", "application/json", jsonReader(body))
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		resp.Body.Close()
	}

	// Wake both concurrently
	var wg sync.WaitGroup
	for _, tc := range tenants {
		wg.Add(1)
		go func(id, ip string) {
			defer wg.Done()
			makePodReady(t, cs, id, "tenants", ip)
			resp, err := http.Post(srv.URL+"/wake/"+id, "application/json", nil)
			if err != nil || resp.StatusCode != http.StatusOK {
				return
			}
			resp.Body.Close()
		}(tc.id, tc.ip)
	}
	wg.Wait()

	// Verify isolation: different pod IPs, different HooksTokens
	recs := make([]*registry.TenantRecord, len(tenants))
	for i, tc := range tenants {
		rec, err := reg.GetTenant(ctx, tc.id)
		require.NoError(t, err)
		require.NotNil(t, rec)
		recs[i] = rec
	}

	assert.NotEqual(t, recs[0].PodIP, recs[1].PodIP, "tenants must have distinct pod IPs")
	assert.NotEqual(t, recs[0].HooksToken, recs[1].HooksToken, "tenants must have distinct HooksTokens")
	assert.NotEqual(t, recs[0].BotToken, recs[1].BotToken, "tenants must have distinct bot tokens")
}

// TestIntegration_WarmPoolHit: tenant wake claims warm pod and pins to its node
func TestIntegration_WarmPoolHit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()

	db, cleanDB := setupDynamoDB(ctx, t)
	defer cleanDB()
	rdb, cleanRedis := setupRedis(ctx, t)
	defer cleanRedis()

	h, cs := newIntegrationHandler(t, db, rdb)
	srv := httptest.NewServer(h.Router())
	defer srv.Close()

	tenantID := "warm-tenant"

	// Pre-seed a warm pool pod (Running, with node assigned)
	warmPodName := "warm-pool-abc123"
	_, err := cs.CoreV1().Pods("tenants").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      warmPodName,
			Namespace: "tenants",
			Labels: map[string]string{
				"app":  "warm-pool",
				"warm": "true",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "kata-node-01",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.3.0.99",
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	// Create tenant
	body, _ := json.Marshal(map[string]interface{}{
		"tenant_id": tenantID,
		"bot_token": "token:warm-test",
	})
	resp, err := http.Post(srv.URL+"/tenants", "application/json", jsonReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	// Wake — should claim warm pod node, not wait for Karpenter
	makePodReady(t, cs, tenantID, "tenants", "10.3.0.50")
	wakeResp, err := http.Post(srv.URL+"/wake/"+tenantID, "application/json", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, wakeResp.StatusCode)
	wakeResp.Body.Close()

	// Verify warm pod was detached (label changed to warm=consuming or deleted)
	pods, err := cs.CoreV1().Pods("tenants").List(ctx, metav1.ListOptions{
		LabelSelector: "app=warm-pool,warm=true",
	})
	require.NoError(t, err)
	assert.Empty(t, pods.Items, "warm pod should have been claimed (no longer warm=true)")

	// Verify tenant pod was created and pinned to the warm node
	tenantPod, err := cs.CoreV1().Pods("tenants").Get(ctx, tenantID, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "kata-node-01", tenantPod.Spec.NodeName, "tenant pod should be pinned to warm pool node")
}

// TestIntegration_PodSpec_OpenClaw: CreateTenantPod sets correct labels, envs, and mounts
func TestIntegration_PodSpec_OpenClaw(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()

	db, cleanDB := setupDynamoDB(ctx, t)
	defer cleanDB()
	rdb, cleanRedis := setupRedis(ctx, t)
	defer cleanRedis()

	h, cs := newIntegrationHandler(t, db, rdb)
	srv := httptest.NewServer(h.Router())
	defer srv.Close()

	tenantID := "spec-tenant"

	// Create + wake
	body, _ := json.Marshal(map[string]interface{}{
		"tenant_id": tenantID,
		"bot_token": "token:spec-test",
	})
	resp, _ := http.Post(srv.URL+"/tenants", "application/json", jsonReader(body))
	resp.Body.Close()

	makePodReady(t, cs, tenantID, "tenants", "10.4.0.1")
	wakeResp, _ := http.Post(srv.URL+"/wake/"+tenantID, "application/json", nil)
	wakeResp.Body.Close()

	// Inspect created pod
	pod, err := cs.CoreV1().Pods("tenants").Get(ctx, tenantID, metav1.GetOptions{})
	require.NoError(t, err)

	// Labels
	assert.Equal(t, "openclaw", pod.Labels["app"], "app label must be 'openclaw'")
	assert.Equal(t, tenantID, pod.Labels["tenant"])

	// Containers: openclaw (main) + s3-sync (sidecar)
	require.Len(t, pod.Spec.Containers, 2)
	c := pod.Spec.Containers[0]
	assert.Equal(t, "openclaw", c.Name)
	assert.Equal(t, "openclaw:test", c.Image)

	// Sidecar
	sidecar := pod.Spec.Containers[1]
	assert.Equal(t, "s3-sync", sidecar.Name)

	// InitContainers: single s3-restore handles both state + workspace
	require.Len(t, pod.Spec.InitContainers, 1)
	assert.Equal(t, "s3-restore", pod.Spec.InitContainers[0].Name)

	// Env vars
	envMap := make(map[string]string)
	for _, e := range c.Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, tenantID, envMap["TENANT_ID"])
	assert.Equal(t, "token:spec-test", envMap["TELEGRAM_BOT_TOKEN"])
	assert.NotEmpty(t, envMap["OPENCLAW_HOOKS_TOKEN"], "OPENCLAW_HOOKS_TOKEN must be injected")
	assert.NotEmpty(t, envMap["TELEGRAM_WEBHOOK_SECRET"], "TELEGRAM_WEBHOOK_SECRET must be injected")
	assert.NotEmpty(t, envMap["ROUTER_PUBLIC_URL"], "ROUTER_PUBLIC_URL must be injected")

	// Volume mounts (openclaw main container: state + workspace only, no direct S3 access)
	mountMap := make(map[string]string)
	for _, m := range c.VolumeMounts {
		mountMap[m.Name] = m.MountPath
	}
	assert.Equal(t, "/root/.openclaw", mountMap["openclaw-state"])
	assert.Equal(t, "/openclaw-workspace", mountMap["workspace"])
	assert.Empty(t, mountMap["s3-state"], "openclaw container must not mount s3-state directly")

	// Readiness probe
	require.NotNil(t, c.ReadinessProbe)
	require.NotNil(t, c.ReadinessProbe.HTTPGet)
	assert.Equal(t, "/healthz", c.ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, int32(18789), c.ReadinessProbe.HTTPGet.Port.IntVal)

	// Resource requests (must be >= 1Gi memory)
	memReq := c.Resources.Requests[corev1.ResourceMemory]
	assert.True(t, memReq.Value() >= 1<<30, "memory request should be >= 1Gi, got %s", memReq.String())
}

// ── Helpers ────────────────────────────────────────────────────────────────

func jsonReader(b []byte) *bytes.Reader {
	return bytes.NewReader(b)
}
