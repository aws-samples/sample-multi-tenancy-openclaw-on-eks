# End-to-End Setup Guide

Deploy the OpenClaw multi-tenant platform on a fresh EKS cluster. This guide covers everything from AWS resource creation to a working Telegram webhook pipeline.

## Prerequisites

- EKS cluster with Karpenter installed and configured
- `kubectl` configured with cluster access
- `aws` CLI with appropriate IAM permissions
- `docker buildx` for multi-arch image builds
- ECR login configured

### Cluster Requirements

| Component | Required | Notes |
|-----------|----------|-------|
| Karpenter | ✅ | With at least one EC2NodeClass + NodePool |
| Pod Identity Agent | ✅ | EKS add-on |
| AWS Load Balancer Controller | ✅ | For ALB Ingress |
| VPC CNI | ✅ | With `NETWORK_POLICY_ENFORCING_MODE=standard` for NetworkPolicy |
| CoreDNS | ✅ | Standard |

---

## Step 1: ECR Repositories

Create three repositories: `orchestrator`, `router`, and `openclaw`.

```bash
REGION=us-east-1
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

for repo in orchestrator router openclaw; do
  aws ecr create-repository \
    --repository-name $repo \
    --region $REGION \
    --image-scanning-configuration scanOnPush=false
done
```

If the OpenClaw image exists in another ECR registry, copy it using `docker buildx imagetools`:

```bash
# Login to both source and target ECR
aws ecr get-login-password --region us-west-2 --profile source | \
  docker login --username AWS --password-stdin <SOURCE_ACCOUNT>.dkr.ecr.us-west-2.amazonaws.com

aws ecr get-login-password --region $REGION | \
  docker login --username AWS --password-stdin ${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com

# Copy multi-arch manifest
docker buildx imagetools create \
  --tag ${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com/openclaw:latest \
  <SOURCE_ACCOUNT>.dkr.ecr.us-west-2.amazonaws.com/openclaw:latest
```

---

## Step 2: DynamoDB Table

Create the tenant registry table with a GSI for status-based queries.

```bash
aws dynamodb create-table \
  --table-name tenant-registry \
  --attribute-definitions \
    AttributeName=tenant_id,AttributeType=S \
    AttributeName=status,AttributeType=S \
    AttributeName=last_active_at,AttributeType=S \
  --key-schema \
    AttributeName=tenant_id,KeyType=HASH \
  --global-secondary-indexes '[
    {
      "IndexName": "status-index",
      "KeySchema": [
        {"AttributeName": "status", "KeyType": "HASH"},
        {"AttributeName": "last_active_at", "KeyType": "RANGE"}
      ],
      "Projection": {"ProjectionType": "ALL"}
    }
  ]' \
  --billing-mode PAY_PER_REQUEST \
  --region $REGION

# Wait for table
aws dynamodb wait table-exists --table-name tenant-registry --region $REGION
```

---

## Step 3: S3 Bucket

Create the bucket for tenant persistent state (brain.db, workspace files).

```bash
BUCKET="openclaw-tenant-state-${REGION_SHORT}"  # e.g. openclaw-tenant-state-ue1

aws s3 mb s3://$BUCKET --region $REGION
aws s3api put-bucket-versioning \
  --bucket $BUCKET \
  --versioning-configuration Status=Enabled \
  --region $REGION
```

> **Why S3 not EBS?** A single EC2 instance supports ~25-28 EBS volumes max. With 210+ tenants per metal node, EBS is not viable. S3 provides unlimited scale.

---

## Step 4: IAM Roles

Two roles are needed, both using EKS Pod Identity (not IRSA).

### 4a. Orchestrator Role

Needs DynamoDB access for tenant CRUD.

```bash
# Trust policy for Pod Identity
cat > /tmp/pod-identity-trust.json << 'EOF'
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Service": "pods.eks.amazonaws.com"},
    "Action": ["sts:AssumeRole", "sts:TagSession"]
  }]
}
EOF

aws iam create-role \
  --role-name orchestrator-pod-identity \
  --assume-role-policy-document file:///tmp/pod-identity-trust.json

# DynamoDB policy (table + GSI indexes)
cat > /tmp/orchestrator-dynamo.json << EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:UpdateItem",
      "dynamodb:DeleteItem", "dynamodb:Scan", "dynamodb:Query"
    ],
    "Resource": [
      "arn:aws:dynamodb:${REGION}:${ACCOUNT_ID}:table/tenant-registry",
      "arn:aws:dynamodb:${REGION}:${ACCOUNT_ID}:table/tenant-registry/index/*"
    ]
  }]
}
EOF

aws iam put-role-policy \
  --role-name orchestrator-pod-identity \
  --policy-name dynamodb-access \
  --policy-document file:///tmp/orchestrator-dynamo.json
```

> ⚠️ **Don't forget `index/*`** — without it, GSI Query operations get `AccessDeniedException`.

### 4b. Tenant Pod Role

Needs S3 (with ABAC isolation) + Bedrock access.

```bash
aws iam create-role \
  --role-name openclaw-tenant-pod \
  --assume-role-policy-document file:///tmp/pod-identity-trust.json

cat > /tmp/tenant-policy.json << 'EOF'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::BUCKET_NAME",
      "Condition": {
        "StringLike": {
          "s3:prefix": ["tenants/${aws:PrincipalTag/kubernetes-pod-name}/*"]
        }
      }
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
      "Resource": "arn:aws:s3:::BUCKET_NAME/tenants/${aws:PrincipalTag/kubernetes-pod-name}/*"
    },
    {
      "Effect": "Allow",
      "Action": ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"],
      "Resource": "*"
    }
  ]
}
EOF
# Replace BUCKET_NAME with your actual bucket name before applying

aws iam put-role-policy \
  --role-name openclaw-tenant-pod \
  --policy-name s3-bedrock-access \
  --policy-document file:///tmp/tenant-policy.json
```

> **S3 ABAC**: The `${aws:PrincipalTag/kubernetes-pod-name}` session tag is automatically set by Pod Identity. Pod name = tenant ID, so each tenant can only access its own S3 prefix.

---

## Step 5: Kubernetes Resources

### 5a. Namespace + ServiceAccounts

```bash
kubectl create namespace tenants
kubectl create serviceaccount orchestrator -n tenants
kubectl create serviceaccount openclaw-tenant -n tenants
```

### 5b. Pod Identity Associations

```bash
aws eks create-pod-identity-association \
  --cluster-name $CLUSTER_NAME \
  --namespace tenants \
  --service-account orchestrator \
  --role-arn arn:aws:iam::${ACCOUNT_ID}:role/orchestrator-pod-identity \
  --region $REGION

aws eks create-pod-identity-association \
  --cluster-name $CLUSTER_NAME \
  --namespace tenants \
  --service-account openclaw-tenant \
  --role-arn arn:aws:iam::${ACCOUNT_ID}:role/openclaw-tenant-pod \
  --region $REGION
```

Verify:

```bash
aws eks list-pod-identity-associations --cluster-name $CLUSTER_NAME --region $REGION
```

### 5c. RBAC

```bash
kubectl apply -f deploy/00-prerequisites.yaml
```

This creates ClusterRole and ClusterRoleBinding for the orchestrator to manage pods, leases, and events.

### 5d. Redis

```bash
# Simple single-replica Redis (production: use ElastiCache)
kubectl apply -f - << 'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: redis
  namespace: tenants
spec:
  replicas: 1
  selector:
    matchLabels:
      app: redis
  template:
    metadata:
      labels:
        app: redis
    spec:
      containers:
      - name: redis
        image: redis:7-alpine
        ports:
        - containerPort: 6379
        resources:
          requests: {cpu: 50m, memory: 64Mi}
          limits: {cpu: 200m, memory: 128Mi}
---
apiVersion: v1
kind: Service
metadata:
  name: redis
  namespace: tenants
spec:
  selector:
    app: redis
  ports:
  - port: 6379
    targetPort: 6379
EOF
```

### 5e. Secret + ConfigMap

```bash
kubectl create secret generic orchestrator-config \
  -n tenants \
  --from-literal=redis-addr="redis.tenants.svc.cluster.local:6379"

kubectl apply -f deploy/03-config-template.yaml
```

---

## Step 6: Kata Containers (Optional — for VM-isolated tenants)

> **Requirement**: x86_64 bare metal instances (`*.metal`). ARM64 bare metal does NOT have `/dev/kvm`.

### 6a. Karpenter NodePool + EC2NodeClass

```bash
kubectl apply -f deploy/02-karpenter.yaml
```

This creates:
- **`kata` EC2NodeClass**: devmapper UserData for containerd thin-pool snapshotter (x86_64)
- **`kata-metal` NodePool**: x86_64 metal instances, `kata-runtime=true:NoSchedule` taint
- **`kata-arm64` EC2NodeClass**: Same as `kata`, plus a systemd service that auto-patches `static_sandbox_resource_mgmt=true` for arm64 CPU hotplug workaround
- **`kata-metal-arm64` NodePool**: Graviton 3+ (gen >6) metal instances, arm64

Key settings:
- `consolidateAfter: 300s` — prevents Karpenter from terminating nodes before kata-deploy finishes (~65s)
- x86_64: c6i.metal, c7i.metal, m6i.metal, etc.
- arm64: c7g.metal, m7g.metal, etc. (Graviton 3+)

### 6b. kata-deploy DaemonSet + RuntimeClass

```bash
# RuntimeClass
kubectl apply -f - << 'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: kata-qemu
handler: kata-qemu
overhead:
  podFixed:
    memory: "160Mi"
    cpu: "250m"
scheduling:
  nodeSelector:
    katacontainers.io/kata-runtime: "true"
  tolerations:
  - key: kata-runtime
    value: "true"
    effect: NoSchedule
EOF
```

Deploy the kata-deploy DaemonSet (see full spec in `deploy/02-karpenter.yaml`). Use `nodeAffinity` with the `katacontainers.io/kata-runtime` label to target kata nodes:

```yaml
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

> The DaemonSet shows `DESIRED=0` until a metal node is provisioned by Karpenter.

---

## Step 7: Build & Push Images

Both orchestrator and router need multi-arch builds (amd64 + arm64):

```bash
cd /path/to/openclaw-tenancy

aws ecr get-login-password --region $REGION | \
  docker login --username AWS --password-stdin ${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com

# Orchestrator
docker buildx build --platform linux/amd64,linux/arm64 \
  -f Dockerfile.orchestrator \
  -t ${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com/orchestrator:latest \
  --push .

# Router
docker buildx build --platform linux/amd64,linux/arm64 \
  -f Dockerfile.router \
  -t ${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com/router:latest \
  --push .
```

> ⚠️ **Always build multi-arch.** If pods run on arm64 nodes but the image is amd64-only, you get `exec format error`.

---

## Step 8: Deploy Orchestrator + Router

Get the OpenClaw image digest:

```bash
OPENCLAW_DIGEST=$(aws ecr describe-images \
  --repository-name openclaw --region $REGION \
  --query 'imageDetails[0].imageDigest' --output text)
OPENCLAW_IMAGE="${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com/openclaw@${OPENCLAW_DIGEST}"
```

Deploy (replace placeholders in `deploy/01-orchestrator.yaml`):

```bash
sed -e "s|<AWS_ACCOUNT_ID>|${ACCOUNT_ID}|g" \
    -e "s|<AWS_REGION>|${REGION}|g" \
    -e "s|<S3_BUCKET>|${BUCKET}|g" \
    -e "s|<YOUR_ROUTER_DOMAIN>|https://PLACEHOLDER.cloudfront.net|g" \
    deploy/01-orchestrator.yaml | kubectl apply -f -
```

Verify:

```bash
kubectl rollout status deployment/orchestrator -n tenants --timeout=120s
kubectl rollout status deployment/router -n tenants --timeout=120s
kubectl get pods -n tenants
# Expected: orchestrator ×2, router ×2, redis ×1, all Running
```

---

## Step 9: Ingress (Internal ALB)

```bash
# First, identify subnets. Exclude AZs not supported by CloudFront VPC Origins
# (e.g. us-east-1e is known to be unsupported)
SUBNETS="subnet-aaa,subnet-bbb,subnet-ccc"  # all private subnets EXCEPT unsupported AZs

kubectl apply -f - << EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: router-ingress
  namespace: tenants
  annotations:
    alb.ingress.kubernetes.io/scheme: internal
    alb.ingress.kubernetes.io/target-type: ip
    alb.ingress.kubernetes.io/healthcheck-path: /healthz
    alb.ingress.kubernetes.io/listen-ports: '[{"HTTP":80}]'
    alb.ingress.kubernetes.io/subnets: "${SUBNETS}"
spec:
  ingressClassName: alb
  rules:
  - http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: router
            port:
              number: 80
EOF
```

Wait for ALB:

```bash
aws elbv2 describe-load-balancers --region $REGION \
  --query 'LoadBalancers[].{Scheme:Scheme,DNS:DNSName,State:State.Code}'
# Wait until State = "active"
```

> ⚠️ **CloudFront VPC Origin AZ limitation**: Explicitly list subnets excluding unsupported AZs. If you don't, VPC Origin creation fails silently then reports the AZ error.

---

## Step 10: CloudFront with VPC Origin

### 10a. Create VPC Origin

```bash
ALB_ARN=$(aws elbv2 describe-load-balancers --region $REGION \
  --query 'LoadBalancers[0].LoadBalancerArn' --output text)

aws cloudfront create-vpc-origin \
  --vpc-origin-endpoint-config "{
    \"Name\": \"openclaw-router-alb\",
    \"Arn\": \"${ALB_ARN}\",
    \"HTTPPort\": 80,
    \"HTTPSPort\": 443,
    \"OriginProtocolPolicy\": \"http-only\"
  }"
```

Wait for deployment:

```bash
VPC_ORIGIN_ID=vo_xxxxx  # from create output

while true; do
  STATUS=$(aws cloudfront get-vpc-origin --id $VPC_ORIGIN_ID \
    --query 'VpcOrigin.Status' --output text)
  echo "VPC Origin: $STATUS"
  [ "$STATUS" = "Deployed" ] && break
  sleep 15
done
```

### 10b. Create CloudFront Distribution

```bash
ALB_DNS=$(aws elbv2 describe-load-balancers --region $REGION \
  --query 'LoadBalancers[0].DNSName' --output text)

cat > /tmp/cf-distribution.json << EOF
{
  "CallerReference": "openclaw-router-$(date +%s)",
  "Comment": "OpenClaw Router - Internal ALB via VPC Origin",
  "Enabled": true,
  "Origins": {
    "Quantity": 1,
    "Items": [{
      "Id": "openclaw-router-vpc-origin",
      "DomainName": "${ALB_DNS}",
      "VpcOriginConfig": {
        "VpcOriginId": "${VPC_ORIGIN_ID}",
        "OriginKeepaliveTimeout": 5,
        "OriginReadTimeout": 30
      }
    }]
  },
  "DefaultCacheBehavior": {
    "TargetOriginId": "openclaw-router-vpc-origin",
    "ViewerProtocolPolicy": "https-only",
    "AllowedMethods": {
      "Quantity": 7,
      "Items": ["GET","HEAD","OPTIONS","PUT","POST","PATCH","DELETE"],
      "CachedMethods": {"Quantity": 2, "Items": ["GET","HEAD"]}
    },
    "CachePolicyId": "4135ea2d-6df8-44a3-9df3-4b5a84be39ad",
    "OriginRequestPolicyId": "216adef6-5c7f-47e4-b989-5492eafa07d3",
    "Compress": true
  },
  "PriceClass": "PriceClass_100"
}
EOF

aws cloudfront create-distribution \
  --distribution-config file:///tmp/cf-distribution.json
```

Note the `DomainName` from output (e.g. `dXXXXXXXXXXXXX.cloudfront.net`).

### 10c. Update Orchestrator ROUTER_PUBLIC_URL

```bash
CF_DOMAIN=dXXXXXXXXXXX.cloudfront.net

kubectl set env deployment/orchestrator -n tenants \
  ROUTER_PUBLIC_URL=https://${CF_DOMAIN}
kubectl rollout status deployment/orchestrator -n tenants
```

**Final Architecture:**

```
Telegram → https://dXXX.cloudfront.net/tg/<tenant>
                    ↓
           CloudFront (HTTPS termination)
                    ↓ VPC Origin (private)
           Internal ALB (HTTP, private subnets)
                    ↓
           Router Service → Router Pods
                    ↓
           Orchestrator → Tenant Pods (Kata/runc)
```

---

## Step 11: NetworkPolicies

Apply network policies for tenant isolation:

```bash
VPC_CIDR=10.8.0.0/16        # Your VPC CIDR
SVC_CIDR=172.20.0.0/16      # K8s Service CIDR
API_SERVER_IP=172.20.0.1     # First IP of Service CIDR

cat deploy/network-policies.yaml | \
  sed "s|<VPC_CIDR>|${VPC_CIDR}|g" | \
  kubectl apply -f -
```

> ⚠️ **Critical**: The orchestrator NetworkPolicy egress must explicitly allow the K8s API server:
>
> ```yaml
> # Add to orchestrator-policy egress rules
> - to:
>     - ipBlock:
>         cidr: 172.20.0.1/32
>   ports:
>     - protocol: TCP
>       port: 443
> ```
>
> Without this, the orchestrator cannot manage pods or hold leader leases. Symptoms: `dial tcp 172.20.0.1:443: i/o timeout` in orchestrator logs.

Verify all 5 policies:

```bash
kubectl get networkpolicy -n tenants
# Expected: tenant-pod-isolation, router-policy, orchestrator-policy, redis-policy, warm-pool-policy
```

---

## Step 12: Verification

### Health checks

```bash
# CloudFront → Router (full chain)
curl -s -o /dev/null -w "HTTP %{http_code}\n" https://${CF_DOMAIN}/healthz
# → HTTP 200

# Webhook path
curl -s -o /dev/null -w "HTTP %{http_code}\n" -X POST https://${CF_DOMAIN}/tg/test
# → HTTP 200

# Orchestrator API (via port-forward)
kubectl port-forward -n tenants svc/orchestrator 18080:8080 &
curl -s http://localhost:18080/healthz    # → "ok"
curl -s http://localhost:18080/tenants    # → [] (empty)
```

### Create a test tenant

```bash
curl -s -X POST http://localhost:18080/tenants \
  -H 'Content-Type: application/json' \
  -d '{
    "tenant_id": "test-tenant",
    "bot_token": "<TELEGRAM_BOT_TOKEN>",
    "idle_timeout": 600
  }'
```

### Verify Telegram webhook

```bash
# The orchestrator auto-registers the webhook with Telegram
curl -s "https://api.telegram.org/bot<TOKEN>/getWebhookInfo" | python3 -m json.tool
# url should be: https://${CF_DOMAIN}/tg/test-tenant
```

### Verify S3 ABAC isolation

```bash
kubectl exec -n tenants test-tenant -- env | grep AWS_CONTAINER_CREDENTIALS
# Pod Identity credentials should be injected
```

---

## Troubleshooting

### Orchestrator: "dial tcp 172.20.0.1:443: i/o timeout"

**Cause**: NetworkPolicy egress blocks K8s API server.
**Fix**: Add `172.20.0.1/32:443` egress rule to orchestrator-policy.

### CloudFront VPC Origin: "Availability Zone not supported"

**Cause**: Some AZs (e.g. `us-east-1e`) don't support CloudFront VPC Origins.
**Fix**: Exclude the unsupported AZ subnet via `alb.ingress.kubernetes.io/subnets` annotation on Ingress.

### kata-deploy: "FailedCreatePodSandBox"

**Cause**: Normal during Kata cold start — sandbox creation fails until kata-deploy installs the runtime (~60s after node ready).
**Fix**: K8s retries automatically. The pod will start after kata-deploy completes.

### First message lost after cold start

**Cause**: Router forwarded before OpenClaw webhook handler was ready. Gateway healthz (`:18789`) returns 200 before the webhook (`:8787`) is listening.
**Fix**: Router probes `:8787` (webhook port) instead of `:18789` (gateway healthz). ReadinessProbe also uses `tcpSocket :8787` so pod Ready means webhook is actually listening.

### OpenClaw Gateway slow on arm64

**Known issue**: Gateway startup is ~50s on Graviton (arm64) vs ~32s on x86_64.

### "exec format error" on pods

**Cause**: Image architecture doesn't match node architecture.
**Fix**: Always build multi-arch with `docker buildx --platform linux/amd64,linux/arm64`.

---

## Resource Summary

| AWS Resource | Name | Region |
|-------------|------|--------|
| EKS Cluster | `$CLUSTER_NAME` | `$REGION` |
| DynamoDB Table | `tenant-registry` (+ `status-index` GSI) | `$REGION` |
| S3 Bucket | `openclaw-tenant-state-*` | `$REGION` |
| ECR | `orchestrator`, `router`, `openclaw` | `$REGION` |
| IAM Role | `orchestrator-pod-identity` | Global |
| IAM Role | `openclaw-tenant-pod` | Global |
| CloudFront | `dXXX.cloudfront.net` | Global |
| VPC Origin | `vo_XXX` | Global |
| Internal ALB | Auto-created by Ingress | `$REGION` |

| K8s Resource | Namespace | Count |
|-------------|-----------|-------|
| Orchestrator Deployment | tenants | 2 replicas |
| Router Deployment | tenants | 2 replicas |
| Redis Deployment | tenants | 1 replica |
| NetworkPolicies | tenants | 5 |
| Ingress (ALB) | tenants | 1 |
| kata-metal NodePool | cluster-scoped | 1 (x86_64) |
| kata-metal-arm64 NodePool | cluster-scoped | 1 (arm64, optional) |
| kata / kata-arm64 EC2NodeClass | cluster-scoped | 1 each |
| kata-deploy DaemonSet | kube-system | 0 (scales with metal nodes) |
| kata-qemu RuntimeClass | cluster-scoped | 1 |

---

## Appendix: Lessons from Real Deployment

These issues were discovered during an actual end-to-end deployment on a production cluster.

### A1. PriorityClass Required

The orchestrator creates tenant pods with `priorityClassName: tenant-normal`. Create it before deploying:

```bash
kubectl apply -f - << 'EOF'
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: tenant-normal
value: 0
globalDefault: false
description: "Normal priority for tenant pods"
EOF
```

### A2. kata-metal NodePool Must Declare Kata Label

Karpenter cannot provision nodes for pods with `nodeSelector: katacontainers.io/kata-runtime=true` unless the NodePool declares this label in `template.metadata.labels`:

```yaml
template:
  metadata:
    labels:
      katacontainers.io/kata-runtime: "true"  # Required!
```

Without this, Karpenter logs: `label "katacontainers.io/kata-runtime" does not have known values`.

### A3. EC2NodeClass Must Use Private Subnets Only

If the VPC has multiple subnet tiers (private/intra/public), ensure the kata EC2NodeClass only selects **private subnets** (with NAT Gateway). Nodes in intra subnets (no NAT) cannot reach AWS APIs, causing nodeadm to fail with `DescribeInstances` retries.

```yaml
subnetSelectorTerms:
- tags:
    karpenter.sh/discovery: my-cluster
    kubernetes.io/role/internal-elb: "1"  # Targets private subnets only
```

**Symptom**: EC2 runs but never registers as a K8s Node. Console output shows `nodeadm` retrying `DescribeInstances` endlessly.

### A4. Do NOT Add `startupTaints` for `uninitialized`

Karpenter automatically adds `node.cloudprovider.kubernetes.io/uninitialized:NoSchedule` as a startup taint. If you also specify it in `startupTaints`, kubelet registers with a **duplicate taint** and the API server rejects it:

```
Node "ip-xxx" is invalid: metadata.taints[3]: Duplicate value
```

**Fix**: Only specify custom taints in `startupTaints`, not ones Karpenter already manages.

### A5. kata-deploy Requires Dedicated ServiceAccount + RBAC

kata-deploy 3.26.0 needs permissions to:
- Read/patch Nodes
- Read/manage RuntimeClasses
- Read CustomResourceDefinitions (checks for NFD)

```bash
kubectl create serviceaccount kata-deploy -n kube-system

kubectl apply -f - << 'EOF'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kata-deploy
rules:
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch", "patch", "update"]
- apiGroups: ["node.k8s.io"]
  resources: ["runtimeclasses"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
- apiGroups: ["apiextensions.k8s.io"]
  resources: ["customresourcedefinitions"]
  verbs: ["get", "list"]
- apiGroups: ["nfd.k8s-sigs.io"]
  resources: ["nodefeaturerules"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kata-deploy
subjects:
- kind: ServiceAccount
  name: kata-deploy
  namespace: kube-system
roleRef:
  kind: ClusterRole
  name: kata-deploy
  apiGroup: rbac.authorization.k8s.io
EOF
```

Set `serviceAccountName: kata-deploy` in the DaemonSet spec.

### A6. kata-deploy `nsenter` Fails to Restart containerd

kata-deploy 3.26.0 on AL2023 may fail to restart containerd via `nsenter`. The install completes but containerd doesn't reload the new runtime config.

**Workaround**: Restart containerd manually via SSM:

```bash
aws ssm send-command \
  --instance-ids <METAL_INSTANCE_ID> \
  --document-name "AWS-RunShellScript" \
  --parameters 'commands=["systemctl restart containerd"]' \
  --region $REGION
```

**Fix**: Add `hostPID: true` to the kata-deploy DaemonSet spec. Without it, `nsenter` cannot restart containerd after installing Kata runtime classes.

### A7. NetworkPolicy Must Allow Pod Identity Agent

EKS Pod Identity Agent runs at `169.254.170.23:80` (link-local). Add egress rule for orchestrator (and any pod using Pod Identity):

```yaml
- to:
    - ipBlock:
        cidr: 169.254.170.23/32
  ports:
    - protocol: TCP
      port: 80
```

**Symptom**: `failed to refresh cached credentials, failed to load credentials` in orchestrator logs.

### A8. NetworkPolicy Must Allow K8s API Server

The API server IP is the first IP of the Service CIDR (e.g. `172.20.0.1`). Add egress rule:

```yaml
- to:
    - ipBlock:
        cidr: 172.20.0.1/32
  ports:
    - protocol: TCP
      port: 443
```

**Symptom**: `dial tcp 172.20.0.1:443: i/o timeout` in orchestrator logs.

### A9. CloudFront VPC Origin AZ Restriction

Some AZs (e.g. `us-east-1e`) are not supported by CloudFront VPC Origins. The VPC Origin creation succeeds but then fails with "Availability Zone not supported".

**Fix**: Explicitly list subnets in the Ingress annotation to exclude the unsupported AZ:

```yaml
alb.ingress.kubernetes.io/subnets: "subnet-aaa,subnet-bbb,subnet-ccc"
```

### Summary: Complete NetworkPolicy Egress for Orchestrator

```yaml
egress:
  - to: [{namespaceSelector: {}, podSelector: {matchLabels: {k8s-app: kube-dns}}}]
    ports: [{port: 53, protocol: UDP}, {port: 53, protocol: TCP}]
  - to: [{podSelector: {matchLabels: {app: redis}}}]
    ports: [{port: 6379, protocol: TCP}]
  - to: [{ipBlock: {cidr: 172.20.0.1/32}}]         # K8s API server
    ports: [{port: 443, protocol: TCP}]
  - to: [{ipBlock: {cidr: 169.254.170.23/32}}]      # Pod Identity Agent
    ports: [{port: 80, protocol: TCP}]
  - to: [{ipBlock: {cidr: 0.0.0.0/0, except: ["10.8.0.0/16","172.20.0.0/16"]}}]
    ports: [{port: 443, protocol: TCP}]              # External HTTPS
  - to: [{podSelector: {matchLabels: {app: openclaw}}}]
    ports: [{port: 8787, protocol: TCP}, {port: 18789, protocol: TCP}]
```
