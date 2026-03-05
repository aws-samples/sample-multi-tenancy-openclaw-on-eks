#!/bin/bash
# openclaw-start.sh — Config injection + gateway startup for tenant pods
# Called as CMD inside the openclaw tenant container.
#
# Config strategy:
#   ConfigMap mounts openclaw.json.tpl at /etc/openclaw/openclaw.json.tpl
#   envsubst replaces ${VAR} placeholders with pod env vars → /root/.openclaw/openclaw.json
#   Simple, no jq, no patch logic — always fresh config from template on each start.
#
# State lifecycle:
#   startup : initContainer s3-restore  → S3 → /root/.openclaw/ + /openclaw-workspace/
#   running : s3-sync sidecar           → local → S3 every 60s
#   SIGTERM : s3-sync sidecar           → final sync → S3
set -euo pipefail

OPENCLAW_DIR="${HOME}/.openclaw"
export OPENCLAW_MODEL="${OPENCLAW_MODEL:-amazon-bedrock/global.anthropic.claude-sonnet-4-6}"
export OPENCLAW_WORKSPACE="${OPENCLAW_WORKSPACE:-/openclaw-workspace}"
export OPENCLAW_PORT="${OPENCLAW_PORT:-18789}"
export OPENCLAW_WEBHOOK_PORT="${OPENCLAW_WEBHOOK_PORT:-8787}"
CONFIG_TPL="${OPENCLAW_CONFIG_TPL:-/etc/openclaw/openclaw.json.tpl}"
CONFIG="${OPENCLAW_DIR}/openclaw.json"

log() { echo "[openclaw-start] $*"; }

# ── 1. Validate required env vars ──────────────────────────────────────────────
: "${TELEGRAM_BOT_TOKEN:?TELEGRAM_BOT_TOKEN is required}"
: "${OPENCLAW_HOOKS_TOKEN:?OPENCLAW_HOOKS_TOKEN is required}"
: "${TENANT_ID:?TENANT_ID is required}"
: "${ROUTER_PUBLIC_URL:?ROUTER_PUBLIC_URL is required}"
: "${TELEGRAM_WEBHOOK_SECRET:?TELEGRAM_WEBHOOK_SECRET is required}"

mkdir -p "${OPENCLAW_DIR}" "${OPENCLAW_WORKSPACE}"

# ── 2. Render config from template ────────────────────────────────────────────
log "Rendering config from template ${CONFIG_TPL} ..."
envsubst < "${CONFIG_TPL}" > "${CONFIG}"
log "Config written to ${CONFIG}."

# ── 3. Start gateway ───────────────────────────────────────────────────────────
log "Starting OpenClaw Gateway on port ${OPENCLAW_PORT} (webhook → :${OPENCLAW_WEBHOOK_PORT})..."
openclaw gateway --allow-unconfigured &
GATEWAY_PID=$!

# ── 4. Wait for gateway ready, then start node ────────────────────────────────
log "Waiting for gateway /healthz..."
for i in $(seq 1 30); do
    if curl -sf "http://localhost:${OPENCLAW_PORT}/healthz" >/dev/null 2>&1; then
        log "Gateway ready (~$((i * 2))s). Starting node host..."
        break
    fi
    sleep 2
done

openclaw node run --host localhost --port "${OPENCLAW_PORT}" &
NODE_PID=$!
log "Node host started (PID ${NODE_PID}). Telegram webhook active on :${OPENCLAW_WEBHOOK_PORT}."

# ── 5. Wait for either process to exit ────────────────────────────────────────
wait -n ${GATEWAY_PID} ${NODE_PID} 2>/dev/null || true
log "A process exited — terminating pod."
kill ${GATEWAY_PID} ${NODE_PID} 2>/dev/null || true
exit 1
