# Cold Start Time Analysis

## Summary Comparison (2026-03-03)

| Stage | Kata cold | Kata warm | runc cold | runc warm |
|-------|-----------|-----------|-----------|-----------|
| **Node** | c6i.metal | c6i.metal | r6g.12xlarge | r6g.12xlarge |
| **Arch** | x86_64 | x86_64 | arm64 | arm64 |
| EC2 → Node Ready | 55s | 0s | 41s | 0s |
| kata-deploy | 60s | 0s | — | — |
| S3 restore | 3s | 3s | 1s | 1s |
| Image pull | 41s | 0s | 29s | 0s |
| OpenClaw Gateway | **33s** | **32s** | **50s** | **50s** |
| **TOTAL** | **~5m** | **43s** | **2m 14s** | **54s** |

> **Note**: OpenClaw Gateway starts ~18s slower on arm64 (Graviton) than x86_64 consistently across all tests. On an x86 node, runc warm pool would be ~35s.

---

## Kata Cold Start (New Metal Node)

### Test Environment
- **Date**: 2026-03-03
- **Node**: c6i.metal / us-west-2b / on-demand
- **S3 Data**: state 37 files / 1.9 MB, workspace 20 files / 51 KB

### Timeline

```
07:04:57  Karpenter: NodeClaim created
07:05:00  EC2 launched (c6i.metal)
07:05:35  Node registered                              +35s
07:05:54  Pod scheduled → FailedCreatePodSandBox
07:05:55  Node Ready                                   +55s
07:06:53  kata-deploy: start install
07:06:55  kata-deploy: containerd restarted, done      +60s from Node Ready
07:07:00  Sandbox retry → aws-cli pull (3s)
07:07:03  S3 restore done                               3s
07:09:15  OpenClaw image pull start              (132s gap)
07:09:53  OpenClaw image pulled                        38s
07:09:53  Containers started
```

> **Note**: First pod was killed by idle timeout before reaching Ready (wake timed out at 4m, idle check terminated pod). Reconciler recreated on the same node with cached images — see Kata warm pool below.

### Stage Breakdown (serial path)

| # | Stage | Start → End | Duration | Notes |
|---|-------|-------------|----------|-------|
| 1 | EC2 Launch → Node Ready | 04:57 → 05:55 | 58s | Karpenter + kubelet bootstrap (metal) |
| 2 | kata-deploy install | 06:53 → 06:55 | 2s | Artifacts copy + containerd restart |
| 3 | kata-deploy image pull | 05:55 → 06:53 | 58s | DaemonSet container pull |
| 4 | Sandbox retries | 05:56 → 07:00 | 64s | K8s backoff retry until kata ready |
| 5 | Init image pull + S3 restore | 07:00 → 07:03 | 3s | aws-cli cached from DaemonSet pull |
| 6 | OpenClaw image pull | 09:15 → 09:53 | 38s | 1.13 GB, first pull on metal |
| 7 | Unexplained gap | 07:03 → 09:15 | 132s | Between init done and main image pull |

> **132s gap**: Occurs between S3 restore completion and OpenClaw image pull start. Observed in both kata cold start tests. May be containerd settling after restart, or K8s image pull queue scheduling. Needs further investigation with kubelet/containerd debug logs.

---

## Kata Warm Pool (Existing Metal Node)

### Test Environment
- **Date**: 2026-03-03
- **Node**: c6i.metal (same node as cold start, images cached)

### Timeline

```
07:18:15  Pod created + scheduled (instant)
07:18:18  S3 restore start
07:18:21  S3 restore done                               3s
07:18:22  OpenClaw started (pull 135ms, cached)
07:18:54  OpenClaw Gateway listening                    32s
07:18:58  Pod Ready ✅
```

**TOTAL: 43s**

| # | Stage | Duration |
|---|-------|----------|
| 1 | Pod scheduling | 0s |
| 2 | Kata VM boot + S3 restore | 6s |
| 3 | OpenClaw Gateway startup | 32s |
| 4 | Webhook + readiness probe | 4s |

---

## runc Cold Start (New Node)

### Test Environment
- **Date**: 2026-03-03
- **Node**: r6g.12xlarge / us-west-2c / spot

### Timeline

```
07:43:01  Pod created
07:43:03  Karpenter: NodeClaim created
07:43:05  EC2 launched (r6g.12xlarge)
07:43:23  Node registered                              +20s
07:43:41  Pod scheduled
07:43:42  Node Ready                                   +39s
07:43:42  Init image pull start (aws-cli)
07:43:54  Init image pulled                            12s
07:43:54  S3 restore start
07:43:55  S3 restore done                               1s
07:44:06  OpenClaw image pull start
07:44:23  OpenClaw image pulled                        17s
07:44:23  Containers started
07:45:13  OpenClaw Gateway listening                   50s
07:45:15  Pod Ready ✅
```

**TOTAL: 2m 14s**

| # | Stage | Start → End | Duration | Notes |
|---|-------|-------------|----------|-------|
| 1 | EC2 Launch → Node Ready | 43:01 → 43:42 | 41s | r6g.12xlarge spot |
| 2 | Init image pull (aws-cli) | 43:42 → 43:54 | 12s | 126 MB, first pull |
| 3 | S3 Restore | 43:54 → 43:55 | 1s | 37 files, 1.9 MB |
| 4 | OpenClaw image pull | 44:06 → 44:23 | 17s | 432 MB (arm64, smaller than x86) |
| 5 | OpenClaw Gateway startup | 44:23 → 45:13 | 50s | arm64 Graviton |
| 6 | Webhook + Readiness probe | 45:13 → 45:15 | 2s | |

> **Note**: arm64 OpenClaw image is 432 MB vs 1.13 GB for x86_64 — pulls 2x faster.

---

## runc Warm Pool (Existing Node)

### Test Environment
- **Date**: 2026-03-03
- **Node**: r6g.12xlarge (same node as cold start, images cached)

### Timeline

```
07:46:14  Pod created + scheduled (instant)
07:46:15  S3 restore start
07:46:16  S3 restore done                               1s
07:46:17  OpenClaw started (pull 123ms, cached)
07:47:07  OpenClaw Gateway listening                   50s
07:47:08  Pod Ready ✅
```

**TOTAL: 54s**

| # | Stage | Duration |
|---|-------|----------|
| 1 | Pod scheduling | 0s |
| 2 | S3 restore | 1s |
| 3 | OpenClaw Gateway startup | 50s |
| 4 | Webhook + readiness probe | 1s |

---

## Key Findings

### OpenClaw Gateway Startup: arm64 vs x86_64

| Arch | Gateway Startup | Samples |
|------|----------------|---------|
| x86_64 (c6i.metal) | 32-34s | 4 runs |
| arm64 (r6g.12xlarge) | 50-54s | 4 runs |
| **Delta** | **~18s** | |

This is the single largest performance difference between runc and Kata warm pool scenarios. The gateway startup is a black box — zero log output between container start and "listening" message.

### Image Size: arm64 vs x86_64

| Image | x86_64 | arm64 |
|-------|--------|-------|
| OpenClaw | 1.13 GB | 432 MB |
| aws-cli | 125 MB | 126 MB |

arm64 OpenClaw image is 2.6x smaller, resulting in faster pulls on cold start.

### 132s Gap in Kata Cold Start

Observed in both kata cold start tests: after S3 restore completes and before OpenClaw image pull begins, there is an unexplained ~130s gap. This does not occur in runc cold starts (only 11s gap). Likely related to containerd state after kata-deploy restarts it.

---

## Bug Fixes Applied

The cold start tests exposed several issues:

1. **Handler**: Set tenant status to `provisioning` at wake start (not just for new tenants)
2. **Reconciler**: Skip orphan cleanup for pods belonging to `provisioning` tenants
3. **Reconciler**: `syncProvisioningTenants` promotes to `running` when pod becomes ready
4. **Karpenter**: `consolidateAfter` increased from 60s to 300s for `kata-metal` NodePool
5. **IAM**: Added `table/tenant-registry/index/*` to orchestrator role for GSI Query access

---

## Optimization Opportunities

| Optimization | Estimated Saving | Effort | Applies To |
|-------------|-----------------|--------|------------|
| x86 nodes for runc | ~18s (warm) | Low | runc |
| `ImagePullPolicy=IfNotPresent` | ~30-60s (cold) | Low | Both |
| kata-deploy image pre-pull | ~58s (kata cold) | Medium | Kata |
| Parallel S3 restore | ~1.5s | Low | Both |
| Investigate OpenClaw Gateway startup | ~20s? | Unknown | Both |
| Investigate 132s gap | ~130s (kata cold) | Medium | Kata |
| runc mode (skip Kata entirely) | ~3m 46s (cold) | Medium | New option |
| Warm pool (current) | eliminates EC2+pull | Done ✅ | Both |

## Measurement Commands

```bash
# Kata cold start (no warm pool)
kubectl cordon <existing-kata-node>
kubectl run wake --rm -i --restart=Never --image=curlimages/curl -- \
  curl -s -X POST http://orchestrator.tenants.svc.cluster.local:8080/wake/<tenant>

# runc cold start (specific instance type)
# Create pod with nodeSelector: {node.kubernetes.io/instance-type: r6g.12xlarge}
# Karpenter provisions the node automatically

# Timestamps
kubectl get pod <pod> -n tenants -o json | python3 -c "
import json,sys; d=json.load(sys.stdin)
print('Created:', d['metadata']['creationTimestamp'])
[print(f'{c[\"type\"]:15s}', c['lastTransitionTime']) for c in d['status'].get('conditions',[])]
[print(f'Init: {s[\"startedAt\"]} → {s[\"finishedAt\"]}') for ic in d['status'].get('initContainerStatuses',[]) for s in [ic.get('state',{}).get('terminated',{})] if s]
[print(f'{cs[\"name\"]}: {s[\"startedAt\"]}') for cs in d['status'].get('containerStatuses',[]) for s in [cs.get('state',{}).get('running',{})] if s]
"
```

## History

| Date | Scenario | Node | Total |
|------|----------|------|-------|
| 2026-03-02 | Kata warm (before cleanup) | c8i.2xlarge | ~45s |
| 2026-03-02 | Kata warm (after cleanup) | c8i.2xlarge | ~38s |
| 2026-03-03 | Kata cold (new metal) | c6i.metal | ~6m |
| 2026-03-03 | Kata warm | c6i.metal | 43s |
| 2026-03-03 | runc cold (new node) | r6g.12xlarge | 2m 14s |
| 2026-03-03 | runc warm | r6g.12xlarge | 54s |
