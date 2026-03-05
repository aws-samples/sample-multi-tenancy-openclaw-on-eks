# Operations Guide

Day-to-day operations for managing the openclaw-tenancy platform.

---

## Tenant Management

### List tenants

```bash
# Via port-forward
kubectl port-forward svc/orchestrator 18800:8080 -n tenants
curl http://localhost:18800/tenants

# Via otm CLI
otm tenant list
```

### Create tenant

```bash
curl -X POST http://localhost:18800/tenants \
  -H "Content-Type: application/json" \
  -d '{
    "tenant_id": "alice",
    "bot_token": "123456:AABBcc...",
    "idle_timeout_s": 600
  }'
```

Orchestrator automatically registers the Telegram webhook on creation.

### Wake / sleep tenant

```bash
# Wake (creates pod if not running)
curl -X POST http://localhost:18800/wake/alice

# Check status
curl http://localhost:18800/tenants/alice

# Update idle timeout
curl -X PATCH http://localhost:18800/tenants/alice \
  -H "Content-Type: application/json" \
  -d '{"idle_timeout_s": 1800}'
```

### Delete tenant

```bash
curl -X DELETE http://localhost:18800/tenants/alice
# Deletes pod + DynamoDB record (S3 state is retained)
```

---

## Cluster State

```bash
# All pods in tenants namespace
kubectl get pods -n tenants

# Tenant registry (DynamoDB)
aws dynamodb scan --table-name tenant-registry --region $AWS_REGION \
  --query 'Items[*].{id:tenant_id.S,status:status.S,ip:pod_ip.S}'

# Router cache (Redis)
kubectl exec -n tenants deployment/redis -- redis-cli KEYS 'router:endpoint:*'
kubectl exec -n tenants deployment/redis -- redis-cli GET router:endpoint:<tenant>
```

---

## Logs

```bash
# Router (webhook + wake triggers)
kubectl logs -n tenants -l app=router -f | grep -v healthz

# Orchestrator (wake/sleep/reconciler)
kubectl logs -n tenants -l app=orchestrator -f | grep -v healthz

# Tenant pod (OpenClaw)
kubectl logs -n tenants <tenant> -c openclaw -f

# s3-restore init container
kubectl logs -n tenants <tenant> -c s3-restore

# s3-sync sidecar
kubectl logs -n tenants <tenant> -c s3-sync -f
```

---

## Troubleshooting

### Tenant pod not waking

1. Check Telegram webhook status:
```bash
BOT_TOKEN="..."
curl "https://api.telegram.org/bot${BOT_TOKEN}/getWebhookInfo"
```
If `last_error_message` is present, Telegram is in backoff. Reset:
```bash
curl "https://api.telegram.org/bot${BOT_TOKEN}/deleteWebhook?drop_pending_updates=true"
curl "https://api.telegram.org/bot${BOT_TOKEN}/setWebhook" \
  -d "url=https://<DOMAIN>/tg/{tenantID}" \
  -d "drop_pending_updates=true"
```

2. Check Router logs for wake calls:
```bash
kubectl logs -n tenants -l app=router --since=5m | grep -v healthz
```

3. Check Orchestrator logs:
```bash
kubectl logs -n tenants -l app=orchestrator --since=5m | grep -v healthz
```

4. Check DynamoDB status:
```bash
aws dynamodb get-item --table-name tenant-registry \
  --key '{"tenant_id": {"S": "<tenant>"}}' --region $AWS_REGION
```

### Pod keeps getting killed after start

Symptom: `reconciler: orphan pod found, deleting` in orchestrator logs right after pod creation.

Cause: Two orchestrator replicas racing — one creates the pod, the other sees it before DynamoDB is updated to `running`.

Fix: Already mitigated with 90s grace period. If still occurring, check reconciler logs for timing.

### Messages not delivered after pod is ready

Symptom: Pod is 2/2 Running but user receives no reply.

Cause: OpenClaw takes ~37s to initialize after pod ready. Router retries for ~50s with 3-5s intervals.

Check Router logs for `forward retry` messages:
```bash
kubectl logs -n tenants -l app=router --since=5m | grep "retry\|forward"
```

### Memory not persisting across restarts

1. Check s3-restore completed successfully:
```bash
kubectl logs -n tenants <tenant> -c s3-restore
```

2. Check S3 bucket has state:
```bash
aws s3 ls s3://<S3_BUCKET>/tenants/<tenant>/state/ --region $AWS_REGION
```

3. Check IAM pod identity has S3 access:
```bash
kubectl exec -n tenants <tenant> -c openclaw -- \
  aws s3 ls s3://<S3_BUCKET>/tenants/<tenant>/ --region $AWS_REGION
```

---

## Warm Pool

```bash
# View warm pool config
otm config get

# Set warm pool target
otm config set warm-pool-target 3

# Check warm pool pods
kubectl get pods -n tenants -l app=warm-pool
```

---

## Upgrading Components

### OpenClaw image

```bash
NEW_DIGEST=$(aws ecr describe-images --repository-name openclaw \
  --region $AWS_REGION \
  --query 'sort_by(imageDetails,&imagePushedAt)[-1].imageDigest' --output text)

kubectl set env deployment/orchestrator -n tenants \
  OPENCLAW_IMAGE=<AWS_ACCOUNT_ID>.dkr.ecr.${AWS_REGION}.amazonaws.com/openclaw@${NEW_DIGEST}

# warm-pool rolls out automatically, existing tenant pods use new image on next wake
```

### Router / Orchestrator

```bash
cd /home/coder/openclaw-tenancy

# Build + push + rollout
make deploy-router
make deploy-orch
```

---

## otm CLI Reference

```bash
# Build and install
make otm && sudo make install-otm

# Usage
otm tenant list
otm tenant create --id alice --bot-token "123456:AABBcc..."
otm tenant get alice
otm tenant wake alice
otm tenant delete alice
otm tenant patch alice --idle-timeout 1800

otm config get
otm config set warm-pool-target 3
```
