#!/usr/bin/env bash
# build-and-deploy.sh — Build and push container images to ECR
#   ./scripts/build-and-deploy.sh [orchestrator] [router] [openclaw] [all]
set -euo pipefail

AWS_REGION="${AWS_REGION:-us-east-1}"
AWS_ACCOUNT_ID="${AWS_ACCOUNT_ID:?AWS_ACCOUNT_ID required}"
ECR_REGISTRY="${ECR_REGISTRY:-${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com}"

info()  { echo -e "\033[1;34m[info]\033[0m  $*"; }
error() { echo -e "\033[1;31m[error]\033[0m $*" >&2; exit 1; }

ecr_login() {
  aws ecr get-login-password --region "${AWS_REGION}" | \
    docker login --username AWS --password-stdin "${ECR_REGISTRY}"
}

build_orchestrator() {
  local image="${ECR_REGISTRY}/orchestrator:latest"
  info "Building orchestrator (multi-arch)..."
  docker buildx build --platform linux/amd64,linux/arm64 \
    -f Dockerfile.orchestrator \
    -t "${image}" --push .
}

build_router() {
  local image="${ECR_REGISTRY}/router:latest"
  info "Building router (multi-arch)..."
  docker buildx build --platform linux/amd64,linux/arm64 \
    -f Dockerfile.router \
    -t "${image}" --push .
}

build_openclaw() {
  # openclaw tenant pod is amd64-only (kata-qemu nodes are amd64 metal)
  local image="${ECR_REGISTRY}/openclaw:latest"
  info "Building openclaw (linux/amd64 only)..."
  docker buildx build --platform linux/amd64 \
    -f Dockerfile.openclaw \
    -t "${image}" --push .
  info "openclaw pods are ephemeral — new image takes effect on next pod wake"
}

[[ $# -eq 0 ]] && { echo "Usage: $0 [orchestrator|router|openclaw|all]"; exit 1; }

ecr_login

for target in "$@"; do
  case "${target}" in
    orchestrator) build_orchestrator ;;
    router)       build_router ;;
    openclaw)     build_openclaw ;;
    all)          build_orchestrator; build_router; build_openclaw ;;
    *) error "Unknown target: ${target}. Use orchestrator|router|openclaw|all" ;;
  esac
done
