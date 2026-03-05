# Kata Containers Resource Mechanism

This document explains how Kata Containers allocates and consumes resources in our multi-tenant EKS deployment. All numbers are based on verified measurements from our actual cluster — not documentation defaults or assumptions.

## Overview

[Kata Containers](https://katacontainers.io/) runs each pod inside a lightweight QEMU/KVM virtual machine, providing **VM-level isolation** between tenants. In our multi-tenant architecture, this means a compromised container cannot escape to the host kernel or access another tenant's memory — the hypervisor boundary enforces isolation that Linux namespaces alone cannot guarantee.

The tradeoff: each pod carries the overhead of a VM (QEMU process, guest kernel, virtiofsd). Understanding how these resources are allocated and consumed is critical for capacity planning.

## How Kata Allocates Resources

### CPU

| Parameter | Value | Source |
|-----------|-------|--------|
| `default_vcpus` | 1 | `configuration-qemu.toml` |
| `maxcpus` | 8 | QEMU command line (`-smp 1,cores=1,threads=1,sockets=8,maxcpus=8`) |
| RuntimeClass overhead | 250m | `kata-qemu` RuntimeClass `overhead.podFixed.cpu` |

The guest VM starts with **1 vCPU**. Additional vCPUs are hot-plugged on demand up to `maxcpus=8` based on the container's CPU limit. The 250m overhead accounts for the QEMU process itself running on the host — this is added to the container's CPU request by the Kubernetes scheduler.

### Memory

| Parameter | Value | Source |
|-----------|-------|--------|
| `default_memory` | 2048 MiB | `configuration-qemu.toml` |
| `memory_slots` | 10 | `configuration-qemu.toml` |
| `maxmem` | 16,726 MiB | QEMU command line (`-m 2048M,slots=10,maxmem=16726M`) |
| `enable_mem_prealloc` | false | `configuration-qemu.toml` |
| Memory backend | `/dev/shm` (shared, file-backed) | QEMU command line |
| RuntimeClass overhead | 160 MiB | `kata-qemu` RuntimeClass `overhead.podFixed.memory` |

Key behaviors:

1. **Lazy allocation via mmap**: `enable_mem_prealloc = false` means QEMU maps 2048 MiB of virtual address space but **only consumes physical RAM for pages the guest actually touches**. An idle pod uses far less than 2 GiB.

2. **Memory hotplug**: With 10 memory slots and `maxmem=16726M`, the VM can grow beyond `default_memory` if the container's memory limit exceeds 2048 MiB. In our case, the container limit is 2 GiB — matching `default_memory` exactly, so **no hotplug occurs**.

3. **RuntimeClass overhead (160 MiB)**: Covers host-side processes (QEMU, virtiofsd, containerd-shim-kata) that run outside the VM. The scheduler adds this to the container's memory request.

### Key Insight: Scheduling vs Actual Usage

The Kubernetes scheduler and the host's actual memory consumption see very different numbers:

| Perspective | CPU per pod | Memory per pod |
|-------------|-------------|----------------|
| **K8s scheduler** (request + overhead) | 450m (200m + 250m) | 1.16 GiB (1 GiB + 160 MiB) |
| **Host physical** (idle `sleep infinity`) | — | ~334 MiB |
| **Host physical** (worst case at limit) | — | ~2.09 GiB (2 GiB + ~90 MiB) |

The scheduler allocates **1.16 GiB per pod** for scheduling decisions, but an idle pod only consumes **~334 MiB** of physical host RAM. This gap is what enables overcommit — and also what requires careful monitoring.

## Host-Side Processes Per Pod

Each Kata pod spawns three host-side processes. Measured on c8i.2xlarge with a `sleep infinity` workload:

| Process | Threads | RSS per pod | Role |
|---------|---------|-------------|------|
| `qemu-system-x86_64` | — | ~282 MiB | Hypervisor + guest VM memory (mmap'd) |
| `virtiofsd` | 2 | ~8 MiB | Filesystem passthrough (host → guest) |
| `containerd-shim-kata` | — | ~44 MiB | Container lifecycle management |
| **Total** | — | **~334 MiB** | — |

The QEMU process RSS (~282 MiB) includes the guest kernel, guest userspace, and QEMU's own overhead. This is the mmap'd portion that has been faulted in — it will grow as the workload touches more memory pages, up to `default_memory` (2048 MiB) plus QEMU overhead.

## Kubernetes Scheduling Math

### How the Scheduler Calculates Pod Cost

When using a RuntimeClass with `overhead.podFixed`, the scheduler adds the overhead to each container's requests and limits:

```
Effective CPU request    = container request + overhead = 200m  + 250m   = 450m
Effective memory request = container request + overhead = 1 GiB + 160 MiB = 1.16 GiB
```

### Node Capacity (c6i.metal)

| Resource | Allocatable | Per pod (scheduled) | Theoretical max pods | Observed max |
|----------|-------------|---------------------|---------------------|--------------|
| CPU | 127,610m | 450m | 283 | — |
| Memory | 255,258 MiB | 1,188 MiB (1.16 GiB) | ~214 | **210** |

Memory is the bottleneck. At 210 pods:
- **Memory requests**: ~243 GiB / 249 GiB allocatable ≈ **99%**
- **CPU requests**: 94,500m / 127,610m ≈ **74%**

The ~4 pod gap between theoretical (214) and observed (210) is due to system pods (kube-proxy, aws-node, etc.) consuming some allocatable capacity.

## Pod Density Test Results

**Test environment**: c6i.metal (128 vCPU / 256 GiB RAM)

**Methodology**: Scaled warm pool pods (running `sleep infinity`) until the scheduler could no longer place new pods.

| Metric | Value |
|--------|-------|
| Max pods on single node | **210** |
| Memory request utilization | 99% |
| CPU request utilization | 74% |
| Bottleneck | Memory requests |

> **Note**: This is a baseline measurement with an idle workload. Real tenant workloads (running code execution, installing packages, etc.) will consume more physical memory per pod, so effective density will be lower even though scheduling math remains the same.

## Configuration Alignment

Our configuration is deliberately aligned to avoid waste and unnecessary complexity:

| Setting | Value | Why |
|---------|-------|-----|
| Container memory limit | 2 GiB | Maximum memory a tenant container can use |
| `default_memory` | 2048 MiB | Guest VM initial address space |
| Container CPU limit | 1000m | Maximum CPU a tenant container can use |
| `maxcpus` | 8 | Allows hotplug headroom (limit is 1 vCPU) |

Because the container memory limit (2 GiB) equals `default_memory` (2048 MiB):
- **No memory hotplug occurs** — the VM starts with enough virtual address space
- **No wasted address space** — we don't over-allocate VM memory beyond what the container can use
- **Physical memory** grows from ~282 MiB (idle) up to ~2 GiB as the workload touches pages

Maximum physical RAM per pod at limit: ~2 GiB (guest) + ~90 MiB (virtiofsd + shim) = **~2.09 GiB**.

## Considerations for Production

### Memory Overcommit

The scheduler allows 210 pods based on **requests** (1.16 GiB each), but each pod's **limit** is 2 GiB:

```
210 pods × 2 GiB limit = 420 GiB potential memory usage
Node physical memory   = 256 GiB
Overcommit ratio       = 1.64×
```

This is safe **only if** not all tenants hit peak memory simultaneously. In practice:
- Idle/warm pool pods use ~334 MiB (far below their 1.16 GiB request)
- Active tenants rarely sustain 2 GiB continuously
- The kernel OOM killer will terminate the guest QEMU process if host memory is exhausted

### Monitoring

- **`kubectl top pods`**: Shows container-level metrics (inside the VM)
- **Node-level**: Monitor host RSS of QEMU processes and overall node memory pressure
- **Key alert**: Node memory utilization approaching 90% physical (not scheduled)

### Tuning Options

| Change | Effect | Tradeoff |
|--------|--------|----------|
| Lower `default_memory` (e.g., 1024 MiB) | Smaller initial mmap; higher density possible | Memory hotplug latency when workloads exceed 1 GiB |
| Raise container memory request | Reduce overcommit ratio | Fewer pods per node |
| Lower container memory limit | Less peak memory per pod | Tenants may OOM more frequently |
| Increase RuntimeClass memory overhead | More accurate scheduling | Fewer pods per node |

### Memory Pre-allocation

Currently `enable_mem_prealloc = false`. If set to `true`:
- QEMU would fault in all 2048 MiB at VM startup
- Each pod would immediately consume ~2 GiB of host RAM
- Maximum density would drop to ~120 pods per c6i.metal
- Benefit: more predictable performance (no page fault latency)

This is **not recommended** for our warm pool model where most pods are idle.

## Instance Requirements

Kata Containers requires hardware virtualization (KVM):

| Instance | Type | KVM | Status |
|----------|------|-----|--------|
| c6i.metal | Bare metal | ✅ Native `/dev/kvm` | Tested — 210 pods |
| c8i.2xlarge | Nitro (non-metal) | ✅ Nested virtualization | Tested — RSS measurements |
| Graviton (arm64) bare metal | Bare metal | ❌ No KVM support | **Not supported** |

> **Important**: arm64/Graviton bare metal instances do not expose `/dev/kvm`. Kata Containers on EKS requires **amd64 instances** with KVM access — either bare metal or Nitro instances with nested virtualization enabled.

## References

- [Kata Containers QEMU configuration (upstream default)](https://github.com/kata-containers/kata-containers/blob/main/src/runtime/config/configuration-qemu.toml.in) — our node config at `/opt/kata/share/defaults/kata-containers/configuration-qemu.toml`
- [RuntimeClass overhead documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-overhead/) — Kubernetes Pod Overhead feature
- [kata-containers/kata-containers#6533](https://github.com/kata-containers/kata-containers/issues/6533) — Discussion on overhead tuning and memory accounting
- Our RuntimeClass definition: `kata-qemu` with `overhead.podFixed: {cpu: 250m, memory: 160Mi}`
- Container resource config: `internal/k8s/client.go` (200m/1GiB request, 1000m/2GiB limit)
