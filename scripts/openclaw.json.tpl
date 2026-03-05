{
  "channels": {
    "telegram": {
      "botToken": "${TELEGRAM_BOT_TOKEN}",
      "dmPolicy": "open",
      "allowFrom": ["*"],
      "groupPolicy": "open",
      "groups": { "*": {} },
      "webhookUrl": "${ROUTER_PUBLIC_URL}/tg/${TENANT_ID}",
      "webhookSecret": "${TELEGRAM_WEBHOOK_SECRET}",
      "webhookHost": "0.0.0.0",
      "webhookPort": ${OPENCLAW_WEBHOOK_PORT}
    }
  },
  "plugins": {
    "entries": {
      "telegram": { "enabled": true }
    }
  },
  "hooks": {
    "enabled": true,
    "token": "${OPENCLAW_HOOKS_TOKEN}",
    "path": "/hooks",
    "allowRequestSessionKey": true,
    "allowedSessionKeyPrefixes": ["hook:"]
  },
  "agents": {
    "defaults": {
      "workspace": "${OPENCLAW_WORKSPACE}",
      "model": { "primary": "${OPENCLAW_MODEL}" }
    }
  },
  "gateway": {
    "bind": "lan",
    "port": ${OPENCLAW_PORT},
    "mode": "local"
  }
}
