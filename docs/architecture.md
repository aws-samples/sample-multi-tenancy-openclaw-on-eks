# Architecture

Deep dive into the openclaw-tenancy system design, component interactions, and infrastructure decisions.

---

## Architecture Overview

```mermaid
flowchart TB
    TG(["☁ Telegram"])

    subgraph cp ["Control Plane"]
        ALB["ALB · HTTPS"]
        R["Router ×2 · :9090"]
        O["Orchestrator ×2"]
    end

    subgraph stores ["Data Stores"]
        Redis[("Redis<br/><sub>cache · lock · config</sub>")]
        DDB[("DynamoDB<br/><sub>tenant registry</sub>")]
    end

    subgraph dp ["Data Plane · Kata VMs · bare metal"]
        direction LR
        TP1["🧠 Tenant Pod<br/><sub>{tenantID}</sub>"]
        TP2["🧠 Tenant Pod<br/><sub>{tenantID}</sub>"]
        WP["💤 Warm Pool ×N"]
    end

    subgraph aws ["AWS Managed"]
        direction LR
        S3[("S3<br/><sub>state sync · ABAC</sub>")]
        BR["Bedrock<br/><sub>LLM inference</sub>"]
    end

    TG -- "POST /tg/{tenantID}" --> ALB
    ALB --> R
    R -. "cache check/set" .-> Redis
    R -- "POST /wake" --> O
    R -- "forward :8787" --> TP1
    O -. "tenant CRUD" .-> DDB
    O -. "distributed lock" .-> Redis
    O -- "create pod" --> TP1
    O -- "consume" --> WP
    TP1 -. "s3 sync" .-> S3
    TP1 -. "InvokeModel" .-> BR

    style cp fill:none,stroke:#3b82f6,stroke-width:2px
    style stores fill:none,stroke:#818cf8,stroke-width:1px,stroke-dasharray:5 5
    style dp fill:none,stroke:#f59e0b,stroke-width:2px
    style aws fill:none,stroke:#a78bfa,stroke-width:1px,stroke-dasharray:5 5
```


---

## AWS Infrastructure

```mermaid
flowchart TB
    subgraph internet [" "]
        direction LR
        User(["👤 Telegram User"])
        TG(["☁ Telegram API"])
    end

    subgraph aws ["AWS · us-west-2"]
        subgraph vpc ["VPC · 10.0.0.0/16"]
            ALB["⚡ Application Load Balancer<br/><sub>HTTPS · ACM cert</sub>"]

            subgraph eks ["EKS Cluster"]
                subgraph ns_tenants ["namespace: tenants"]
                    direction TB
                    subgraph mgmt ["Management Pods"]
                        direction LR
                        R["Router ×2<br/><sub>Deployment · arm64/amd64</sub>"]
                        O["Orchestrator ×2<br/><sub>Deployment · arm64/amd64<br/>Leader Election via Lease</sub>"]
                        RD[("Redis<br/><sub>Deployment ×1</sub>")]
                    end

                    subgraph metal ["Bare Metal Nodes · c6i/c7i.metal · amd64"]
                        direction LR
                        TP1["🧠 Tenant Pod<br/><sub>Kata VM (QEMU)<br/>:8787 webhook<br/>:18789 healthz</sub>"]
                        TP2["🧠 Tenant Pod<br/><sub>Kata VM (QEMU)</sub>"]
                        WP["💤 Warm Pool<br/><sub>Deployment ×N<br/>sleep ∞ · image prefetch</sub>"]
                    end

                    NP{{"NetworkPolicy<br/><sub>Calico iptables<br/>policy-only</sub>"}}
                end

                KP["Karpenter<br/><sub>NodePool: kata-metal<br/>on-demand · amd64 only</sub>"]

                PI["Pod Identity Agent<br/><sub>session tags enabled</sub>"]
            end
        end

        subgraph aws_svc ["AWS Managed Services"]
            direction LR
            DDB[("DynamoDB<br/><sub>tenant-registry<br/>PAY_PER_REQUEST</sub>")]
            S3[("S3<br/><sub>state bucket<br/>tenants/{id}/ prefix<br/>ABAC via session tags</sub>")]
            ECR[("ECR<br/><sub>orchestrator<br/>router · openclaw</sub>")]
            BR["Bedrock<br/><sub>Claude · on-demand</sub>"]
        end
    end

    User -- message --> TG
    TG -- "webhook POST" --> ALB
    ALB --> R
    R --> RD
    R -- "POST /wake" --> O
    R -- "forward" --> TP1

    O --> DDB
    O --> RD
    O -. "create/delete" .-> TP1
    O -. "consume" .-> WP
    KP -. "provisions" .-> metal

    PI -. "inject credentials
+ session tags" .-> TP1

    TP1 -. "aws s3 sync" .-> S3
    TP1 -. "InvokeModel" .-> BR

    NP -. "allow Router→Pod
ports 8787+18789
block cross-tenant" .-> metal

    style internet fill:none,stroke:none
    style aws fill:none,stroke:#f59e0b,stroke-width:2px
    style vpc fill:none,stroke:#64748b,stroke-width:1px,stroke-dasharray:5 5
    style eks fill:none,stroke:#3b82f6,stroke-width:2px
    style ns_tenants fill:none,stroke:#3b82f6,stroke-width:1px,stroke-dasharray:5 5
    style mgmt fill:none,stroke:#64748b,stroke-width:1px,stroke-dasharray:3 3
    style metal fill:none,stroke:#f59e0b,stroke-width:1px,stroke-dasharray:3 3
    style aws_svc fill:none,stroke:#a78bfa,stroke-width:1px,stroke-dasharray:5 5
```

### Key Infrastructure Decisions

| Decision | Choice | Reason |
|----------|--------|--------|
| Instance type | `c6i/c7i.metal` (amd64) | Kata needs `/dev/kvm`; Graviton metal lacks KVM |
| Node provisioning | Karpenter | Auto-scale bare metal on demand, avoid idle cost |
| Container runtime | `kata-qemu` | VM-level tenant isolation (guest kernel per pod) |
| NetworkPolicy engine | Calico iptables policy-only | VPC CNI eBPF conflicts with Kata TC redirect |
| State storage | S3 + `aws s3 sync` | S3 CSI (mountpoint-s3) is write-once FUSE, can't overwrite |
| Data isolation | S3 ABAC via Pod Identity session tags | Zero extra IAM Roles; `${aws:PrincipalTag/kubernetes-pod-name}` restricts prefix |
| Tenant registry | DynamoDB PAY_PER_REQUEST | Multi-replica concurrent R/W, cross-pod persistence |
| Image build | `docker buildx` multi-arch | Cluster has both amd64 + arm64 nodes |
| Image registry | ECR (private) | Same region, no cross-region pull latency |


---

## Message Flow

```mermaid
sequenceDiagram
    autonumber
    actor User
    participant TG as Telegram
    participant Router
    participant Redis
    participant Orch as Orchestrator
    participant Pod as Tenant Pod

    User->>TG: Send message
    TG->>Router: POST /tg/{tenantID}
    Router-->>TG: 200 OK (immediate ack)

    Router->>Redis: GET router:endpoint:{tenantID}

    alt Cache HIT — fast path
        Redis-->>Router: pod_ip
        Router->>Pod: POST pod_ip:8787/telegram-webhook
        Pod-->>User: LLM reply via Bot API
    else Cache MISS or stale IP
        Redis-->>Router: (nil)
        Router->>Orch: POST /wake/{tenantID}

        Note over Orch: Acquire Redis NX lock,<br/>check DynamoDB, warm pool...

        Orch-->>Router: pod_ip (+ bot_token)

        opt True cold start (no prior cached IP)
            Router->>User: 🏗 Starting up, please wait...
        end

        loop healthz poll — every 3s, up to 5m30s
            Router->>Pod: GET pod_ip:18789/healthz
            Pod-->>Router: 200 OK or connection refused
        end

        Router->>Pod: POST pod_ip:8787/telegram-webhook
        Router->>Redis: SET router:endpoint:{tenantID} pod_ip (re-cache)
        Pod-->>User: LLM reply via Bot API
    end
```

### Timing

| Scenario | Total latency |
|---|---|
| Pod already running (cache hit) | ~2–5 s (LLM response time) |
| Warm pool hit (node pre-provisioned) | ~40–60 s (s3-restore ~3 s + OpenClaw init ~37 s) |
| Cold start (Karpenter provisions node) | ~3–5 min (metal node provision + above) |

> **Starting notification** is sent only on true cold starts — when no cached IP existed before the wake call. Cache-miss retries (stale IP → re-wake) do **not** re-notify the user.

> **Cache preservation**: after the healthz poll succeeds, the Router re-sets `router:endpoint:{tenantID}` to keep the cache warm for subsequent messages.

---

## Pod Lifecycle

```mermaid
stateDiagram-v2
    [*] --> idle : create tenant

    idle --> running : message arrives / POST /wake
    running --> idle : idle timeout (30s tick)
    running --> auto_restart : pod dies within idle window
    auto_restart --> running : informer detects & re-wakes (~1s)
    running --> idle : pod missing in k8s (informer or safety-net)
    idle --> [*] : DELETE /tenants/{id}
```

### State Transitions

| Component | Trigger | Transition |
|---|---|---|
| **API handler** `/wake` | `POST /wake/{id}` | idle → running |
| **Lifecycle controller** | 30 s tick (leader only) | running → idle if `now - last_active_at > idle_timeout_s` |
| **Reconciler** | K8s Informer (event-driven) + 5 min safety-net | running → idle if pod not found in k8s |
| **Reconciler** | pod dies within idle window (~1s detection) | auto-restart → running |
| **API handler** `/delete` | `DELETE /tenants/{id}` | any → deleted |

---

## Pod Spec

Each tenant pod (`{tenantID}`) has three containers:

```mermaid
flowchart LR
    subgraph Pod ["Pod · {tenantID}"]
        direction TB

        subgraph Init ["initContainer"]
            S3R[s3-restore<br/><i>aws-cli:2.15.30</i>]:::init
        end

        subgraph Main ["Containers"]
            direction LR
            GW["openclaw gateway<br/>:8787 webhook<br/>:18789 healthz"]:::main
            SYNC["s3-sync sidecar<br/><i>aws-cli:2.15.30</i><br/>every 60s + SIGTERM flush"]:::sidecar
        end

        Init --> Main
    end

    subgraph Volumes
        ED1["emptyDir · openclaw-state<br/>/root/.openclaw/"]:::vol
        ED2["emptyDir · workspace<br/>/openclaw-workspace/"]:::vol
        CM["ConfigMap · config-template<br/>/etc/openclaw/ (readOnly)"]:::vol
    end

    ED1 -.- GW
    ED1 -.- SYNC
    ED1 -.- S3R
    ED2 -.- GW
    ED2 -.- S3R
    CM -.- GW

    classDef init fill:#457b9d,stroke:#1d3557,color:#fff
    classDef main fill:#2d6a4f,stroke:#1b4332,color:#fff
    classDef sidecar fill:#e76f51,stroke:#9c4225,color:#fff
    classDef vol fill:#6c757d,stroke:#495057,color:#fff
```

### Container Details

| Container | Image | Purpose |
|---|---|---|
| **s3-restore** (init) | `public.ecr.aws/aws-cli/aws-cli:2.15.30` | Restore state & workspace from S3 before OpenClaw starts. Excludes `openclaw.json`, `*.lock` |
| **openclaw** (main) | `{ECR}/openclaw@sha256:{digest}` (pinned) | OpenClaw Gateway — Telegram webhook mode. Config rendered from ConfigMap template via `envsubst` on start |
| **s3-sync** (sidecar) | `public.ecr.aws/aws-cli/aws-cli:2.15.30` | `aws s3 sync` every 60 s + final sync on SIGTERM. Excludes `openclaw.json*` |

### Volumes

| Name | Type | Mount | Notes |
|---|---|---|---|
| `openclaw-state` | emptyDir | `/root/.openclaw/` | OpenClaw state (memory, sessions) |
| `workspace` | emptyDir | `/openclaw-workspace/` | Agent workspace files |
| `config-template` | ConfigMap | `/etc/openclaw/` (readOnly) | `openclaw.json.tpl` — rendered on start |

> No PVCs. S3 CSI (mountpoint-s3) was evaluated and rejected — write-once FUSE, cannot overwrite or delete existing files.

---

## State Persistence

### S3 Layout

```mermaid
flowchart TB
    B(["📦 s3://{S3_BUCKET}/"])
    T["tenants/"]
    TID["{tenant_id}/"]
    ST["state/ → /root/.openclaw/<br/><sub>brain.db · sessions · memory</sub>"]
    WS["workspace/ → /openclaw-workspace/<br/><sub>agent work files</sub>"]

    B --> T --> TID
    TID --> ST
    TID --> WS
```

### S3 ABAC (Attribute-Based Access Control)

Tenant pods use **EKS Pod Identity** with session tags to scope S3 access per tenant:

| Mechanism | Detail |
|---|---|
| **Pod Identity Association** | Service account `openclaw-tenant` → IAM role `openclaw-tenant-pod-identity` |
| **Session Tag** | `kubernetes-pod-name = {tenantID}` (injected by Pod Identity webhook) |
| **IAM Condition** | `ListBucket` scoped by `s3:prefix`; `Get/Put/Delete` scoped by Resource ARN with `${aws:PrincipalTag/kubernetes-pod-name}` |
| **Effect** | Each pod can only read/write its own `tenants/{tenantID}/` prefix — enforced at IAM level |

### Consistency Properties

- **Last-write-wins**: `aws s3 sync` with no `--delete`. S3 accumulates extra files; OpenClaw tolerates extras.
- **Loss window**: If pod is killed without SIGTERM (OOM, node failure), up to 60 s of state lost. Acceptable for agent memory (append-only).
- **`openclaw.json` excluded**: Always regenerated from template via `envsubst`. Restored config would have wrong auth tokens.
- **Lock files excluded**: `.lock` files from previous lifetime cause "session file locked" errors.

---

## Warm Pool

Pre-provisioned nodes to eliminate Karpenter provisioning latency (~3–4 min → ~40 s).

```mermaid
sequenceDiagram
    participant WPD as Warm Pool Deployment
    participant WP as Warm Pod (sleep ∞)
    participant Orch as Orchestrator
    participant TP as Tenant Pod

    Note over WPD: replicas = WARM_POOL_TARGET<br/>(Redis: otm config set warm-pool-target N)

    WPD->>WP: Create warm pod<br/>(node=ip-10-6-x, label warm=true)

    Note over Orch: Tenant wake arrives...

    Orch->>WP: Find Running pod with label warm=true
    Orch->>WP: Patch label: warm=true → warm=consuming
    Note over WPD: Selector loses pod → schedules replacement

    Orch->>WP: Delete warm pod (free node resources)
    Orch->>TP: Create {tenantID} pod<br/>(nodeName = same node)

    WPD->>WP: Create replacement warm pod (background)
```

Warm pods run `sleep infinity` — they pre-pull the openclaw image and hold the node but do **not** start OpenClaw. OpenClaw still needs ~37 s to initialize after the tenant pod starts.

**Configuration**: warm pool target is stored in Redis and adjustable at runtime:

```bash
otm config set warm-pool-target <N>
```

---

## High Availability

Orchestrator runs **2 replicas**. Coordination mechanisms:

### Redis Wake Lock

Prevents duplicate pod creation for the same tenant.

```mermaid
sequenceDiagram
    participant A as Replica A
    participant Redis
    participant B as Replica B
    participant DDB as DynamoDB

    A->>Redis: SET tenant:waking:{tenantID} "1" NX EX 240s
    Redis-->>A: OK (lock acquired)

    B->>Redis: SET tenant:waking:{tenantID} "1" NX EX 240s
    Redis-->>B: nil (lock held)

    Note over A: Creates pod, updates DynamoDB...
    A->>DDB: status=running, pod_ip=...
    A->>Redis: DEL tenant:waking:{tenantID}

    loop Poll until running
        B->>DDB: GET tenant status
        DDB-->>B: status=running, pod_ip
    end
    B-->>B: Return pod_ip to caller
```

### Kubernetes Lease Leader Election

Only one replica runs the idle timeout loop.

| Parameter | Value |
|---|---|
| Lease name | `orchestrator-leader` (tenants namespace) |
| Duration | 15 s |
| Renew | 10 s |
| Retry | 2 s |
| Leader runs | `checkIdleTenants()` every 30 s |

### Reconciler (All Replicas)

Event-driven via **K8s SharedInformer** watching `app=openclaw` pods. Runs on every replica (idempotent).

**Event-driven path** (~1s response):
- Pod DELETE → check DynamoDB, auto-restart if within idle window
- Pod UPDATE (phase → Failed/Succeeded, or IP change) → reconcile single tenant

**Safety-net full reconcile** (every 5 min, configurable via `RECONCILER_INTERVAL`):
- Stale running tenant: pod missing in k8s → reset DynamoDB to idle
- Orphan pod: `{tenantID}` pod with no running tenant in DynamoDB → delete (90s grace)
- IP drift: pod IP changed → update Redis cache + DynamoDB

---

## Infrastructure

### EKS Cluster

| Item | Value |
|---|---|
| Cluster | `<EKS_CLUSTER>`, `us-west-2`, account `<AWS_ACCOUNT_ID>` |
| Namespace | `tenants` |
| Ingress | `<DOMAIN>` → ALB → Router:9090 |
| Runtime | Kata Containers (`kata-qemu`) — VM-level isolation for all tenant pods |

### Kata Containers / Bare Metal

Kata requires hardware virtualization (`/dev/kvm`). Only `.metal` EC2 instances expose this.

Karpenter NodePool `kata-metal` provisions `c/m/r` gen-6+ metal nodes with:
- **Devmapper** thin-pool snapshotter (required by kata-qemu)
- **Taint** `kata-runtime=true:NoSchedule` — only kata-tolerating pods schedule here

### NetworkPolicy

Network isolation uses **VPC CNI NetworkPolicy** (`NETWORK_POLICY_ENFORCING_MODE=standard`). Validated compatible with both runc and Kata Containers on EKS 1.35 / VPC CNI v1.21.1 (8/8 tests passed for Kata pods). No Calico needed.

| Policy | Effect |
|---|---|
| **tenant-pod-isolation** | Ingress: only `app=router` on `:8787` and `:18789`. Egress: DNS, Pod Identity Agent, IMDS, all external (except VPC/Service CIDRs) |
| **orchestrator-policy** | Egress to Redis, K8s API server (`172.20.0.1:443`), Pod Identity Agent, external HTTPS |
| **router-policy** | Ingress from VPC (ALB). Egress to orchestrator, Redis, tenant pods, external HTTPS |
| **redis-policy** | Ingress from orchestrator + router only. No egress |
| **warm-pool-policy** | No ingress, no egress (`sleep infinity`) |

> **Key egress rules**: Must explicitly allow Pod Identity Agent (`169.254.170.23:80,443`) and IMDS (`169.254.169.254:80`) — without these, AWS credential retrieval fails inside tenant pods.
| **Allow Pod → Telegram** | Egress to `api.telegram.org` (Bot API replies) |
| **Deny Pod → Pod** | No lateral movement between tenant pods |

### IAM

| Role | Service Account | Permissions |
|---|---|---|
| `openclaw-tenant-pod-identity` | `openclaw-tenant` | Bedrock: `InvokeModel`, `InvokeModelWithResponseStream`; S3: read/write on `<S3_BUCKET>` (scoped by ABAC session tags) |

---

## Security

### BotToken Handling

| Aspect | Detail |
|---|---|
| Stored in | DynamoDB `bot_token` field (encrypted at rest) |
| Redacted from | All public API responses |
| Internal access | `GET /tenants/{id}/bot_token` — used by Router for Telegram notifications |
| Pod access | `TELEGRAM_BOT_TOKEN` env var (used by OpenClaw) |

### Tenant Isolation

| Layer | Mechanism |
|---|---|
| **Compute** | Each tenant in dedicated Kata VM (QEMU) — hardware-enforced memory/CPU isolation |
| **Storage** | Dedicated S3 prefix per tenant (`tenants/{tenantID}/`), enforced by ABAC session tags |
| **Network** | Calico NetworkPolicy — deny lateral movement, allow only required egress |
| **IAM** | Pod Identity session tags + IAM condition on `${aws:PrincipalTag/kubernetes-pod-name}` |

> **Shared IAM role caveat**: All tenant pods share one IAM role (single service account). CloudTrail cannot attribute Bedrock usage per tenant — application-level tracking is needed for billing.
