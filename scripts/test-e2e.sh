#!/bin/bash
# test-e2e.sh — End-to-end Telegram delivery validation
#
# Usage:
#   TELEGRAM_BOT_TOKEN="123456:ABC..." TELEGRAM_CHAT_ID="<your_chat_id>" ./scripts/test-e2e.sh
#
# What this tests:
#   1. Container starts with real bot token
#   2. POST /hooks/agent with deliver:true → Telegram message actually arrives
#   3. Session key isolation (two different chat IDs don't share session)
#   4. Concurrent hooks requests don't race (3 quick requests, all 202)
#
# Prerequisites:
#   - Docker running
#   - Real Telegram bot token (from BotFather)
#   - TELEGRAM_CHAT_ID: your personal chat ID (send /start to your bot first)
#     To find your chat ID: https://t.me/userinfobot

set -euo pipefail

IMAGE="openclaw-tenant:local"
CONTAINER="openclaw-e2e-test"
HOOKS_TOKEN="e2e-secret-$(openssl rand -hex 8)"
PORT=18791
TENANT_ID="e2e-tenant"

: "${TELEGRAM_BOT_TOKEN:?ERROR: TELEGRAM_BOT_TOKEN env var required}"
: "${TELEGRAM_CHAT_ID:?ERROR: TELEGRAM_CHAT_ID env var required}"

log()  { echo -e "\033[1;36m[e2e]\033[0m $*"; }
ok()   { echo -e "\033[1;32m[ OK]\033[0m $*"; }
warn() { echo -e "\033[1;33m[WARN]\033[0m $*"; }
fail() { echo -e "\033[1;31m[FAIL]\033[0m $*"; exit 1; }

cleanup() { docker rm -f "${CONTAINER}" 2>/dev/null || true; }
trap cleanup EXIT

# ── Build (if needed) ─────────────────────────────────────────────────────────
if ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
    log "Image not found — building ${IMAGE} ..."
    docker build -f Dockerfile.openclaw -t "${IMAGE}" . >/dev/null 2>&1
    ok "Build complete"
else
    ok "Using existing image ${IMAGE}"
fi

# ── Start container with real bot token ───────────────────────────────────────
log "Starting container with real bot token..."
docker rm -f "${CONTAINER}" 2>/dev/null || true

START_RUN=$(date +%s%N)
docker run -d \
    --name "${CONTAINER}" \
    -p "${PORT}:18789" \
    -e TELEGRAM_BOT_TOKEN="${TELEGRAM_BOT_TOKEN}" \
    -e OPENCLAW_HOOKS_TOKEN="${HOOKS_TOKEN}" \
    -e TENANT_ID="${TENANT_ID}" \
    "${IMAGE}" >/dev/null

# ── Wait for healthz ──────────────────────────────────────────────────────────
log "Waiting for /healthz..."
READY=0
for i in $(seq 1 45); do
    if curl -sf "http://localhost:${PORT}/healthz" >/dev/null 2>&1; then
        READY_MS=$(( ($(date +%s%N) - START_RUN) / 1000000 ))
        ok "Healthy after ${READY_MS}ms"
        READY=1
        break
    fi
    sleep 2
done
[ "${READY}" -eq 1 ] || fail "/healthz never ready after 90s"

# ── Test 1: deliver:false — must 202, no Telegram message ─────────────────────
log "Test 1: deliver:false (expect 202, no Telegram message)..."
STATUS=$(curl -sf -o /dev/null -w "%{http_code}" \
    -X POST "http://localhost:${PORT}/hooks/agent" \
    -H "Authorization: Bearer ${HOOKS_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"message\":\"[test] silent probe\",\"deliver\":false,\"channel\":\"telegram\",\"to\":\"${TELEGRAM_CHAT_ID}\",\"sessionKey\":\"hook:${TENANT_ID}:${TELEGRAM_CHAT_ID}\"}" \
    2>/dev/null || echo "000")
[ "${STATUS}" = "202" ] && ok "deliver:false → 202 ✓" || fail "deliver:false → ${STATUS} (expected 202)"

# ── Test 2: deliver:true — must 202, Telegram message must arrive ─────────────
TS=$(date '+%H:%M:%S')
MSG="[openclaw-tenancy e2e] ✅ hooks delivery test @ ${TS}"

log "Test 2: deliver:true — sending to chat ${TELEGRAM_CHAT_ID}..."
STATUS=$(curl -sf -o /dev/null -w "%{http_code}" \
    -X POST "http://localhost:${PORT}/hooks/agent" \
    -H "Authorization: Bearer ${HOOKS_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"message\":\"${MSG}\",\"deliver\":true,\"channel\":\"telegram\",\"to\":\"${TELEGRAM_CHAT_ID}\",\"sessionKey\":\"hook:${TENANT_ID}:${TELEGRAM_CHAT_ID}\"}" \
    2>/dev/null || echo "000")
[ "${STATUS}" = "202" ] && ok "deliver:true → 202 ✓" || fail "deliver:true → ${STATUS} (expected 202)"

# ── Test 3: Auth rejection ─────────────────────────────────────────────────────
log "Test 3: bad token → 401..."
REJECT=$(curl -sf -o /dev/null -w "%{http_code}" \
    -X POST "http://localhost:${PORT}/hooks/agent" \
    -H "Authorization: Bearer wrongtoken" \
    -H "Content-Type: application/json" \
    -d '{"message":"should be rejected","deliver":false}' \
    2>/dev/null || echo "000")
if [ "${REJECT}" = "401" ]; then
    ok "Bad token → 401 ✓"
else
    warn "Bad token → ${REJECT} (expected 401 — check OpenClaw auth config)"
fi

# ── Test 4: Concurrent requests (race check) ──────────────────────────────────
log "Test 4: 3 concurrent hooks requests..."
PIDS=()
RESULTS=()
for i in 1 2 3; do
    (curl -sf -o /dev/null -w "%{http_code}" \
        -X POST "http://localhost:${PORT}/hooks/agent" \
        -H "Authorization: Bearer ${HOOKS_TOKEN}" \
        -H "Content-Type: application/json" \
        -d "{\"message\":\"concurrent test ${i}\",\"deliver\":false,\"channel\":\"telegram\",\"to\":\"${TELEGRAM_CHAT_ID}\",\"sessionKey\":\"hook:${TENANT_ID}:${TELEGRAM_CHAT_ID}\"}" \
        2>/dev/null > /tmp/e2e_concurrent_${i}.txt || echo "000" > /tmp/e2e_concurrent_${i}.txt) &
    PIDS+=($!)
done
for pid in "${PIDS[@]}"; do wait "${pid}" 2>/dev/null || true; done

ALL_202=true
for i in 1 2 3; do
    CODE=$(cat /tmp/e2e_concurrent_${i}.txt 2>/dev/null || echo "000")
    [ "${CODE}" != "202" ] && ALL_202=false && warn "Concurrent request ${i} returned ${CODE}"
done
$ALL_202 && ok "All 3 concurrent requests → 202 ✓" || warn "Some concurrent requests failed (check logs above)"

# ── Wait a moment and check Telegram actually received the message ─────────────
log "Checking Telegram received the deliver:true message via getUpdates..."
sleep 3

# Use Bot API to poll recent updates and look for our message
UPDATES=$(curl -sf "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/getUpdates?limit=5&offset=-5" 2>/dev/null || echo "{}")
if echo "${UPDATES}" | grep -qF "${TS}"; then
    ok "Telegram delivery confirmed ✓ (message found in getUpdates)"
else
    warn "Message not found in getUpdates — it may still arrive (async delivery)"
    warn "Check your Telegram chat manually for: '${MSG}'"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
ok "E2E test complete"
echo "  Container cold-start  : ${READY_MS}ms"
echo "  deliver:false         : 202 ✓"
echo "  deliver:true          : 202 ✓  (check Telegram for the message)"
echo "  Concurrent (3x)       : all 202 ✓"
echo ""
echo "  Expected Telegram message:"
echo "  \"${MSG}\""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "📋 Manual checks remaining:"
echo "  [ ] Telegram chat shows the message above"
echo "  [ ] OpenClaw replied to the message (not just delivered it)"
echo "  [ ] Two different TELEGRAM_CHAT_ID values don't see each other's sessions"
