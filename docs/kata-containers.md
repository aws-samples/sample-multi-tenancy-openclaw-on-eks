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
| `maxmem` | 16,726 MiB | QEMU command line (`-m 2048M,slots=10,maxmem=16726M`). Derived from `default_memory` (2048 MiB) + 10 memory slots × ~1,468 MiB per slot |
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

| Instance | Arch | Type | KVM | Kata Status |
|----------|------|------|-----|-------------|
| c6i.metal | x86_64 | Bare metal | ✅ Native `/dev/kvm` | ✅ Tested — 210 pods |
| c8i.2xlarge | x86_64 | Nitro (non-metal) | ✅ Nested virtualization | ✅ Tested — RSS measurements |
| c7g.metal | arm64 | Bare metal (Graviton 3) | ✅ Native `/dev/kvm` | ✅ Tested — requires special config |
| m7g.metal | arm64 | Bare metal (Graviton 3) | ✅ Native `/dev/kvm` | ✅ Should work (same arch) |

## Kata Containers on Graviton (arm64) Bare Metal

### Overview

Kata Containers **can run on Graviton bare metal instances** (c7g.metal, m7g.metal, etc.), but requires a specific configuration change to work around an arm64 hardware limitation. Without this change, pod creation fails with:

```
failed to create shim task: failed to query hotpluggable CPUs:
QMP command failed: machine does not support hot-plugging CPUs
```

### Why arm64 Needs Special Handling

On x86_64, Kata uses **CPU hotplug** to dynamically add vCPUs to a running VM:

1. VM starts with 1 vCPU (`default_vcpus=1`) for fast boot
2. Container is created with a CPU limit (e.g., `cpu: 1`)
3. Kata sends QMP `device_add` to QEMU → hotplugs additional vCPUs
4. Guest kernel receives ACPI notification → brings new CPUs online

On arm64, this flow breaks because:

- **GIC (Generic Interrupt Controller)** requires all CPU redistributors to be declared at VM boot time — they cannot be added at runtime
- QEMU's `virt` machine type (the only option for arm64) **does not support ACPI-driven CPU hotplug**
- The arm64 Linux kernel supports CPU online/offline (toggling existing CPUs) but not adding entirely new CPU devices after boot

**In short**: x86 ACPI is a "discover and use" model; arm64 Device Tree is a "declare at boot" model.

### The Fix: `static_sandbox_resource_mgmt`

Kata provides a configuration flag that pre-allocates all vCPUs at VM startup, bypassing hotplug entirely:

```toml
# /opt/kata/share/defaults/kata-containers/configuration-qemu.toml
static_sandbox_resource_mgmt = true
```

With this enabled, Kata:

1. Reads the Pod's `resources.limits.cpu` **before** starting the VM
2. Calculates total vCPUs: `default_vcpus + ceil(container CPU limit)`
3. Starts QEMU with `-smp N` (all CPUs allocated upfront)
4. **Never performs CPU hotplug** during the VM lifecycle
5. Uses cgroups inside the guest for per-container CPU isolation

**Tradeoffs:**
- ❌ Cannot dynamically adjust CPU after VM starts (`docker update --cpus` won't work)
- ❌ Pods **must** set `resources.limits.cpu`, otherwise they only get `default_vcpus` (1)
- ✅ No hotplug latency — all CPUs available immediately
- ✅ Simpler VM lifecycle — fewer failure modes

For our use case (fixed CPU allocation per tenant), this is a perfect fit.

### Karpenter Configuration

Two new resources are needed for arm64 Kata nodes:

**EC2NodeClass** (`kata-arm64`):

```yaml
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: kata-arm64
spec:
  role: my-eks-cluster                    # Same node role as x86 kata
  amiSelectorTerms:
  - alias: al2023@latest                  # AL2023 supports arm64 natively
  blockDeviceMappings:
  - deviceName: /dev/xvda
    ebs:
      encrypted: true
      volumeSize: 500Gi                   # Same as x86 — devmapper needs space
      volumeType: gp3
  subnetSelectorTerms:                    # Use explicit subnet IDs from your primary VPC CIDR
  - id: subnet-0123456789abcdef0          # Replace with your subnet IDs
  - id: subnet-0123456789abcdef1
  - id: subnet-0123456789abcdef2
  securityGroupSelectorTerms:
  - tags:
      karpenter.sh/discovery: my-eks-cluster
  tags:
    karpenter.sh/discovery: my-eks-cluster
  userData: |
    #!/bin/bash
    set -ex
    # ... (same devmapper thin-pool setup as x86 kata EC2NodeClass)

    # --- Kata arm64 static sandbox config ---
    # See "Automated Configuration via EC2NodeClass userData" section for details
    cat > /usr/local/bin/kata-arm64-config.sh <<'KATA_SCRIPT'
    #!/bin/bash
    set -e
    CONF=/opt/kata/share/defaults/kata-containers/configuration-qemu.toml
    for i in $(seq 1 120); do
      if [ -f "$CONF" ]; then
        sed -i 's/^static_sandbox_resource_mgmt = false/static_sandbox_resource_mgmt = true/' "$CONF"
        echo "kata arm64: static_sandbox_resource_mgmt=true patched"
        exit 0
      fi
      sleep 5
    done
    echo "kata arm64: timeout waiting for kata config file"
    exit 1
    KATA_SCRIPT
    chmod +x /usr/local/bin/kata-arm64-config.sh

    cat > /etc/systemd/system/kata-arm64-config.service <<'KATA_SVC'
    [Unit]
    Description=Patch Kata config for arm64 static sandbox resource management
    After=containerd.service
    [Service]
    Type=oneshot
    RemainAfterExit=true
    ExecStart=/usr/local/bin/kata-arm64-config.sh
    [Install]
    WantedBy=multi-user.target
    KATA_SVC

    systemctl daemon-reload
    systemctl enable kata-arm64-config.service
    systemctl start kata-arm64-config.service &
```

**NodePool** (`kata-metal-arm64`):

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: kata-metal-arm64
spec:
  weight: 10
  template:
    metadata:
      labels:
        katacontainers.io/kata-runtime: "true"
    spec:
      taints:
      - key: kata-runtime
        value: "true"
        effect: NoSchedule
      requirements:
      - key: karpenter.k8s.aws/instance-category
        operator: In
        values: ["c", "m", "r"]           # r = memory-optimized, fits more pods
      - key: karpenter.k8s.aws/instance-size
        operator: In
        values: ["metal"]
      - key: karpenter.k8s.aws/instance-generation
        operator: Gt
        values: ["6"]                     # Graviton 3+ (c7g, m7g, r7g, etc.)
      - key: karpenter.sh/capacity-type
        operator: In
        values: ["on-demand"]
      - key: kubernetes.io/arch
        operator: In
        values: ["arm64"]
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: kata-arm64
  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 300s
```

### kata-deploy DaemonSet

Use the `katacontainers.io/kata-runtime: "true"` label as the DaemonSet's `nodeAffinity` selector. Since the Karpenter NodePool already sets this label in `template.metadata.labels`, every kata node gets the label at registration time, and kata-deploy automatically schedules to it — no instance type list maintenance needed.

```yaml
# kata-deploy DaemonSet nodeAffinity
spec:
  template:
    spec:
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: katacontainers.io/kata-runtime
                operator: In
                values:
                - "true"
```

This works for both x86 and arm64 nodes — any NodePool that labels nodes with `katacontainers.io/kata-runtime: "true"` will automatically get kata-deploy.

> **Note**: kata-deploy automatically detects the host architecture and sets `machine_type = "virt"` for arm64 (instead of `q35` for x86). No manual intervention is needed for this setting.

### Automated Configuration via EC2NodeClass userData

After kata-deploy installs on a new arm64 node, `static_sandbox_resource_mgmt` must be patched to `true`. This is achieved with a systemd oneshot service embedded in the EC2NodeClass `userData`, which runs on every new node and waits for kata-deploy to write the config file before patching it.

**How it works:**

1. Cloud-init runs the userData script during node bootstrap
2. The script creates a shell script (`/usr/local/bin/kata-arm64-config.sh`) that polls for the kata config file
3. A systemd oneshot service (`kata-arm64-config.service`) is registered with `After=containerd.service`
4. The service is started in background (`systemctl start ... &`) — it loops checking for `/opt/kata/share/defaults/kata-containers/configuration-qemu.toml`
5. Once kata-deploy writes the file (~45s after node registration), the script patches `static_sandbox_resource_mgmt = true` via `sed`
6. Next kata-qemu pod can start without CPU hotplug errors

**Add this to the end of your `kata-arm64` EC2NodeClass `userData`:**

```bash
# --- Kata arm64 static sandbox config ---
# kata-deploy writes the config file after it schedules on this node.
# This service waits for the file and patches static_sandbox_resource_mgmt=true
# to work around arm64 CPU hotplug limitation.
cat > /usr/local/bin/kata-arm64-config.sh <<'KATA_SCRIPT'
#!/bin/bash
set -e
CONF=/opt/kata/share/defaults/kata-containers/configuration-qemu.toml
for i in $(seq 1 120); do
  if [ -f "$CONF" ]; then
    sed -i 's/^static_sandbox_resource_mgmt = false/static_sandbox_resource_mgmt = true/' "$CONF"
    echo "kata arm64: static_sandbox_resource_mgmt=true patched"
    exit 0
  fi
  sleep 5
done
echo "kata arm64: timeout waiting for kata config file"
exit 1
KATA_SCRIPT
chmod +x /usr/local/bin/kata-arm64-config.sh

cat > /etc/systemd/system/kata-arm64-config.service <<'KATA_SVC'
[Unit]
Description=Patch Kata config for arm64 static sandbox resource management
After=containerd.service

[Service]
Type=oneshot
RemainAfterExit=true
ExecStart=/usr/local/bin/kata-arm64-config.sh

[Install]
WantedBy=multi-user.target
KATA_SVC

systemctl daemon-reload
systemctl enable kata-arm64-config.service
# Start in background — it loops waiting for kata-deploy to install
systemctl start kata-arm64-config.service &
```

The systemd service typically polls for ~45 seconds before kata-deploy writes the config file. Total time from node ready to first successful kata-qemu pod is approximately 60–70 seconds. No manual intervention is required — new Graviton metal nodes receive the patch automatically.

**Alternative approaches:**

- **Manual kubectl debug**: Useful for one-off testing, but the patch is lost when the node is replaced
  ```bash
  kubectl debug node/<node-name> -it --image=busybox -- sh -c '
    sed -i "s/^static_sandbox_resource_mgmt = false/static_sandbox_resource_mgmt = true/" \
      /host/opt/kata/share/defaults/kata-containers/configuration-qemu.toml
  '
  ```
- **Custom kata-deploy image**: Avoids post-install patching entirely, but requires maintaining a fork
- **Post-install DaemonSet**: Adds an extra moving part; the systemd approach is simpler since it's self-contained in the EC2NodeClass

### Important Notes

- **Metal instance boot time**: Bare metal instances (c6i.metal, c7g.metal) take **8-12 minutes** to fully boot (hardware initialization). Once UserData completes, kubelet registers in ~30 seconds. Plan for this in cold start scenarios.

- **Pod must specify CPU limits on arm64**: With `static_sandbox_resource_mgmt = true`, the VM size is determined at creation time from `resources.limits.cpu`. If no CPU limit is set, the VM starts with only `default_vcpus` (1 vCPU).

- **EC2NodeClass subnet selection**: Use explicit subnet IDs in `subnetSelectorTerms`, not tag-based selectors. VPCs with secondary CIDRs (e.g., `100.64.0.0/16` for EKS Pod networking) may cause nodes to launch in the wrong subnet and fail to register with the API server.

## References

- [Kata Containers QEMU configuration (upstream default)](https://github.com/kata-containers/kata-containers/blob/main/src/runtime/config/configuration-qemu.toml.in) — our node config at `/opt/kata/share/defaults/kata-containers/configuration-qemu.toml`
- [RuntimeClass overhead documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-overhead/) — Kubernetes Pod Overhead feature
- [kata-containers/kata-containers#6533](https://github.com/kata-containers/kata-containers/issues/6533) — Discussion on overhead tuning and memory accounting
- Our RuntimeClass definition: `kata-qemu` with `overhead.podFixed: {cpu: 250m, memory: 160Mi}`
- Container resource config: `internal/k8s/client.go` (200m/1GiB request, 1000m/2GiB limit)
