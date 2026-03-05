#!/usr/bin/env bash
# setup-pod-identity.sh — Bootstrap EKS Pod Identity for openclaw tenant pods
#
# Run once per cluster. Idempotent — safe to re-run.
#
# Usage:
#   CLUSTER=my-eks-cluster ACCOUNT=123456789012 ./scripts/setup-pod-identity.sh
#
# What this creates:
#   IAM Policy  : openclaw-tenant-bedrock  (Bedrock InvokeModel)
#   IAM Role    : openclaw-tenant-pod      (trusted by pods.eks.amazonaws.com)
#   Association : tenants/openclaw-tenant → openclaw-tenant-pod

set -euo pipefail

CLUSTER="${CLUSTER:-my-eks-cluster}"
ACCOUNT="${ACCOUNT:-$(aws sts get-caller-identity --query Account --output text)}"
REGION="${REGION:-$(aws configure get region || echo us-east-1)}"
NAMESPACE="tenants"
SA_NAME="openclaw-tenant"
ROLE_NAME="openclaw-tenant-pod"
POLICY_NAME="openclaw-tenant-bedrock"

log()  { echo -e "\033[1;36m[pod-identity]\033[0m $*"; }
ok()   { echo -e "\033[1;32m[ OK]\033[0m $*"; }

# ── 1. IAM Policy ─────────────────────────────────────────────────────────────
log "Creating IAM policy: ${POLICY_NAME} ..."
POLICY_ARN="arn:aws:iam::${ACCOUNT}:policy/${POLICY_NAME}"

aws iam create-policy \
  --policy-name "${POLICY_NAME}" \
  --description "Allow openclaw tenant pods to invoke Bedrock models" \
  --policy-document "{
    \"Version\": \"2012-10-17\",
    \"Statement\": [
      {
        \"Sid\": \"BedrockInvoke\",
        \"Effect\": \"Allow\",
        \"Action\": [
          \"bedrock:InvokeModel\",
          \"bedrock:InvokeModelWithResponseStream\"
        ],
        \"Resource\": [
          \"arn:aws:bedrock:*::foundation-model/*\",
          \"arn:aws:bedrock:*:${ACCOUNT}:provisioned-model/*\",
          \"arn:aws:bedrock:*:${ACCOUNT}:inference-profile/*\"
        ]
      }
    ]
  }" 2>/dev/null || log "Policy already exists, skipping."
ok "Policy: ${POLICY_ARN}"

# ── 2. IAM Role ───────────────────────────────────────────────────────────────
log "Creating IAM role: ${ROLE_NAME} ..."
aws iam create-role \
  --role-name "${ROLE_NAME}" \
  --description "EKS Pod Identity role for openclaw tenant pods" \
  --assume-role-policy-document '{
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Principal": { "Service": "pods.eks.amazonaws.com" },
      "Action": ["sts:AssumeRole", "sts:TagSession"]
    }]
  }' 2>/dev/null || log "Role already exists, skipping."

aws iam attach-role-policy \
  --role-name "${ROLE_NAME}" \
  --policy-arn "${POLICY_ARN}" 2>/dev/null || true

ROLE_ARN="arn:aws:iam::${ACCOUNT}:role/${ROLE_NAME}"
ok "Role: ${ROLE_ARN}"

# ── 3. EKS Pod Identity Association ───────────────────────────────────────────
log "Creating Pod Identity association: ${NAMESPACE}/${SA_NAME} → ${ROLE_NAME} ..."

# Check if association already exists
EXISTING=$(aws eks list-pod-identity-associations \
  --cluster-name "${CLUSTER}" \
  --namespace "${NAMESPACE}" \
  --service-account "${SA_NAME}" \
  --query 'associations[0].associationId' \
  --output text 2>/dev/null || echo "None")

if [ "${EXISTING}" = "None" ] || [ -z "${EXISTING}" ]; then
  aws eks create-pod-identity-association \
    --cluster-name "${CLUSTER}" \
    --namespace "${NAMESPACE}" \
    --service-account "${SA_NAME}" \
    --role-arn "${ROLE_ARN}"
  ok "Association created"
else
  ok "Association already exists (${EXISTING}), skipping."
fi

# ── 4. Verify ─────────────────────────────────────────────────────────────────
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
ok "Pod Identity setup complete"
echo ""
echo "  Cluster    : ${CLUSTER}"
echo "  Namespace  : ${NAMESPACE}"
echo "  SA         : ${SA_NAME}"
echo "  Role       : ${ROLE_ARN}"
echo ""
echo "  To verify on a running pod:"
echo "  kubectl exec -n ${NAMESPACE} <openclaw-pod> -- env | grep AWS_CONTAINER"
echo ""
echo "  Expected env vars injected by Pod Identity webhook:"
echo "  AWS_CONTAINER_CREDENTIALS_FULL_URI=http://169.254.170.23/..."
echo "  AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE=/var/run/secrets/pods.eks.amazonaws.com/serviceaccount/token"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
