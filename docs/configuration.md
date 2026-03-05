# Configuration Reference

All configurable parameters for the openclaw-tenancy platform.

---

## Orchestrator Environment Variables

| Name | Default | Description |
|------|---------|-------------|
| `DYNAMODB_TABLE` | `tenant-registry` | DynamoDB table name for tenant records |
| `DYNAMODB_ENDPOINT` | _(empty)_ | Custom DynamoDB endpoint (local dev: `http://localhost:8000`) |
| `REDIS_ADDR` | `localhost:6379` | Redis address (`host:port`) |
| `K8S_NAMESPACE` | `tenants` | Kubernetes namespace for tenant resources |
| `S3_BUCKET` | `<S3_BUCKET>` | S3 bucket for tenant state persistence |
| `WARM_POOL_TARGET` | `2` | Number of warm pool replicas to maintain |
| `OPENCLAW_IMAGE` | `openclaw:latest` | Full ECR image URI for OpenClaw (use digest for stability) |
| `KATA_RUNTIME_CLASS` | `kata-qemu` | Kubernetes RuntimeClass for tenant pods |
| `ROUTER_PUBLIC_URL` | _(empty)_ | Public router URL (e.g. `https://zeroclaw-router.example.com`). Enables auto-webhook registration. |
| `PORT` | `8080` | HTTP listen port |
| `POD_NAME` | _(downward API)_ | Pod name, used for leader election identity |
| `LOCAL_MODE` | `false` | Set `true` or set `DYNAMODB_ENDPOINT` for local dev (k8s ops skipped) |

### Internal Constants

| Constant | Value | Description |
|----------|-------|-------------|
| `WakeLockTTL` | 240s | Redis wake lock TTL (auto-expires on replica crash) |
| `PodReadyWait` | 210s | Max wait for pod to become Running with PodIP |
| Lifecycle tick | 30s | Leader: how often idle timeout is checked |
| Reconciler tick | 60s | All replicas: how often k8s vs DynamoDB is reconciled |
| Orphan grace period | 90s | New pods younger than this are skipped during orphan cleanup |

---

## Router Environment Variables

| Name | Default | Description |
|------|---------|-------------|
| `REDIS_ADDR` | `localhost:6379` | Redis address (`host:port`) |
| `ORCHESTRATOR_ADDR` | `http://localhost:8080` | Orchestrator service URL |
| `PUBLIC_BASE_URL` | _(empty)_ | Public URL for Telegram webhook paths |
| `PORT` | `9090` | HTTP listen port |

### Internal Constants

| Constant | Value | Description |
|----------|-------|-------------|
| `endpointCacheTTL` | 5 min | Redis TTL for cached pod IPs |
| `podReadyWait` | 5 min | Max wait for pod wake (forwardUpdate timeout) |
| `postToPod` timeout | 5s | Fail-fast timeout per forwarding attempt |
| Forward retry attempts | 10 | Max retries after wake (3s, then 5s × 9) |

---

## OpenClaw Pod Spec

Tenant pods (`openclaw-{tenantID}`) are created dynamically by the orchestrator.

### Pod-level Fields

| Field | Value | Notes |
|-------|-------|-------|
| `runtimeClassName` | `kata-qemu` | VM-level isolation |
| `priorityClassName` | `tenant-normal` | Higher priority than warm pool |
| `serviceAccountName` | `openclaw-tenant` | Shared SA with Bedrock + S3 IAM |
| `nodeName` | warm pod's node or empty | Pinned when warm pool hit |
| `nodeSelector` | `katacontainers.io/kata-runtime: "true"` | Only kata nodes |
| `tolerations` | `kata-runtime=true:NoSchedule` | Tolerates kata node taint |

### Container Resources (openclaw)

| Resource | Request | Limit |
|----------|---------|-------|
| CPU | 200m | 1000m |
| Memory | 1Gi | 2Gi |

### Container Environment Variables (openclaw)

| Name | Source | Description |
|------|--------|-------------|
| `TENANT_ID` | pod creation | Tenant identifier, used by s3 sync scripts |
| `TELEGRAM_BOT_TOKEN` | DynamoDB record | Passed at pod creation time |
| `OPENCLAW_HOOKS_TOKEN` | DynamoDB record | OpenClaw hooks auth token |
| `TELEGRAM_WEBHOOK_SECRET` | DynamoDB record | Telegram webhook verification secret |
| `ROUTER_PUBLIC_URL` | orchestrator config | Used by OpenClaw to register its own webhook |
| `S3_BUCKET` | orchestrator config | For s3-restore and s3-sync containers |
| `AWS_REGION` | orchestrator config | For aws s3 sync commands |

### Volumes

| Name | Mount Path | Type | Purpose |
|------|-----------|------|---------|
| `openclaw-state` | `/root/.openclaw` | emptyDir | OpenClaw config, memory, sessions |
| `workspace` | `/openclaw-workspace` | emptyDir | Agent workspace files |
| `config-template` | `/etc/openclaw` | ConfigMap (readOnly) | `openclaw.json.tpl` template |

---

## DynamoDB Schema

### Table: `tenant-registry`

| Field | Type | Key | Description |
|-------|------|-----|-------------|
| `tenant_id` | String | **PK** | Unique tenant identifier |
| `status` | String | — | `idle` or `running` |
| `pod_name` | String | — | `openclaw-{tenantID}`. Empty when idle. |
| `pod_ip` | String | — | Pod cluster IP. Empty when idle. |
| `namespace` | String | — | Always `tenants` |
| `s3_prefix` | String | — | `tenants/{tenantID}/` |
| `bot_token` | String | — | Telegram Bot API token (redacted from public API responses) |
| `hooks_token` | String | — | OpenClaw hooks auth token (redacted) |
| `webhook_secret` | String | — | Telegram webhook verification secret (redacted) |
| `created_at` | String (RFC3339) | — | Tenant creation timestamp |
| `last_active_at` | String (RFC3339) | — | Last message activity timestamp |
| `idle_timeout_s` | Number | — | Idle timeout in seconds (default: 600) |

### Billing Mode

PAY_PER_REQUEST (on-demand).

---

## Redis Key Schema

| Key Pattern | TTL | Set by | Description |
|-------------|-----|--------|-------------|
| `router:endpoint:{tenantID}` | 5 min | Router (after wake) | Cached pod IP |
| `tenant:waking:{tenantID}` | 240s | Orchestrator (wake path) | Distributed wake lock |

---

## S3 State Layout

```
<S3_BUCKET>/
└── tenants/
    └── {tenant_id}/
        ├── state/       → restored to /root/.openclaw/
        └── workspace/   → restored to /openclaw-workspace/
```

### Exclusions (s3-restore init container)

| Pattern | Reason |
|---------|--------|
| `openclaw.json` | Always regenerated from template (envsubst) on pod start |
| `openclaw.json.*` | Backup files generated by OpenClaw itself |
| `*.lock` | Session lock files cause "session file locked" errors if restored from previous pod |

---

## Karpenter NodePool: `kata-metal`

| Field | Value |
|-------|-------|
| Instance category | `c`, `m`, `r` |
| Instance size | `metal` only |
| Instance generation | `> 5` (gen 6+) |
| Capacity type | `on-demand` |
| Arch | `amd64` |
| Taint | `kata-runtime=true:NoSchedule` |
| Consolidation | `WhenEmpty`, after 60s |
| Expire after | `720h` (30 days) |
| CPU limit | `256` vCPUs |
