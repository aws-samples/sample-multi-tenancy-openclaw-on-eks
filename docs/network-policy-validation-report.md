# NetworkPolicy Validation Report: VPC CNI + Kata Containers on EKS

**Date**: 2026-03-04
**Cluster**: `my-eks-cluster` (EKS 1.35)
**Namespace**: `tenants`

---

## Summary

**VPC CNI NetworkPolicy (`standard` mode) works with both runc and Kata Containers.** No Calico needed.

- runc pods: 7/7 tests passed
- Kata pods: 8/8 tests passed
- Real tenant E2E: Telegram bot on Kata VM verified

---

## Environment

| Component | Version |
|-----------|---------|
| EKS | 1.35 |
| VPC CNI | v1.21.1-eksbuild.3 |
| `NETWORK_POLICY_ENFORCING_MODE` | `standard` |
| Kata Containers | kata-qemu RuntimeClass |
| Kata nodes | c6i.metal (amd64) |
| runc nodes | m6g.8xlarge (arm64) |

---

## NetworkPolicy Configuration

### Tenant Pod Isolation

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: tenant-pod-isolation
  namespace: tenants
spec:
  podSelector:
    matchLabels:
      app: openclaw
  policyTypes:
  - Ingress
  - Egress
  ingress:
  - from:
    - podSelector:
        matchLabels:
          app: router
    ports:
    - port: 8787
      protocol: TCP
    - port: 18789
      protocol: TCP
  egress:
  # DNS
  - to:
    - namespaceSelector: {}
      podSelector:
        matchLabels:
          k8s-app: kube-dns
    ports:
    - port: 53
      protocol: UDP
    - port: 53
      protocol: TCP
  # Pod Identity Agent
  - to:
    - ipBlock:
        cidr: 169.254.170.23/32
    ports:
    - port: 80
      protocol: TCP
    - port: 443
      protocol: TCP
  # IMDS
  - to:
    - ipBlock:
        cidr: 169.254.169.254/32
    ports:
    - port: 80
      protocol: TCP
  # All external (except VPC + service CIDRs)
  - to:
    - ipBlock:
        cidr: 0.0.0.0/0
        except:
        - 10.8.0.0/16       # VPC CIDR (adjust per cluster)
        - 172.20.0.0/16      # Service CIDR
```

### Egress Rules — Don't Forget These

| Destination | Why |
|-------------|-----|
| `169.254.170.23/32:80,443` | **EKS Pod Identity Agent** — without this, credential retrieval fails |
| `169.254.169.254/32:80` | **IMDS** — instance metadata token |
| `kube-dns:53` | DNS resolution |
| `0.0.0.0/0` (except VPC/Service) | Tenant outbound — AI agents need unrestricted external access |

### Other Policies

| Policy | Selector | Purpose |
|--------|----------|---------|
| `orchestrator-policy` | `app=orchestrator` | → DynamoDB, Redis, K8s API (`172.20.0.1/32:443`), Pod Identity Agent |
| `router-policy` | `app=router` | → tenant pods, orchestrator, external |
| `redis-policy` | `app=redis` | ← orchestrator, router, warm-pool only |
| `warm-pool-policy` | `app=warm-pool` | → orchestrator, Redis |

---

## Test Results — runc Pods

| # | Test | Expected | Result |
|---|------|----------|--------|
| 1 | `app=router` → tenant:8787 | Allow | ✅ HTTP 200 |
| 2 | `app=attacker` → tenant:8787 | Deny | ✅ Timeout |
| 3 | `app=router` → tenant:80 (wrong port) | Deny | ✅ Timeout |
| 4 | default namespace → tenant:8787 | Deny | ✅ Timeout |
| 5 | tenant → external HTTPS | Allow | ✅ HTTP 200 |
| 6 | tenant → redis:6379 | Deny | ✅ Timeout |
| 7 | tenant → orchestrator:8080 | Deny | ✅ Timeout |

**7/7 PASS**

## Test Results — Kata Pods

| # | Test | Expected | Result |
|---|------|----------|--------|
| 1 | `app=router` → kata:8787 | Allow | ✅ HTTP 200 |
| 2 | `app=attacker` → kata:8787 | Deny | ✅ Timeout |
| 3 | `app=router` → kata:80 (wrong port) | Deny | ✅ Timeout |
| 4 | default namespace → kata:8787 | Deny | ✅ Timeout |
| 5 | kata → external HTTPS | Allow | ✅ HTTP 200 |
| 6 | kata → redis:6379 | Deny | ✅ Timeout |
| 7 | kata → orchestrator:8080 | Deny | ✅ Timeout |
| 8 | kata → other tenant:8787 | Deny | ✅ Timeout |

**8/8 PASS**

## Real Tenant E2E

Created tenant `np-verify` with Telegram bot token on Kata pod (c6i.metal):
- Full path: Telegram → CloudFront → Internal ALB → Router → Kata pod
- Bot responded to messages ✅
- Pod Identity credentials working ✅
- Pod stable: 2/2 Running, 0 restarts ✅

---

## How It Works

VPC CNI eBPF programs attach to the `clsact` qdisc on the host veth interface. Kata's TC redirect operates as a separate TC action on the same interface. They coexist without conflict.

```
Ingress:  network → host veth (eBPF filter) → TC redirect → Kata VM
Egress:   Kata VM → TC redirect → host veth (eBPF filter) → network
```

---

## When You Might Need Calico

VPC CNI NetworkPolicy covers standard K8s NetworkPolicy. Consider Calico only if you need:
- GlobalNetworkPolicy (cluster-wide rules)
- HostEndpoint policies (node-level protection)

If using Calico with Kata, use **iptables mode** (`linuxDataplane: Iptables`), not eBPF.
