# @grafana/sigil-opencode

OpenCode plugin that sends LLM generations to [Grafana AI Observability](https://grafana.com/docs/grafana-cloud/machine-learning/ai-observability/).

By default only metadata is sent (model, tokens, tool names, timing). Flip `contentCapture` to `true` to also send message content (with automatic secret redaction).

## Setup

1. Create `~/.config/opencode/opencode-sigil.json`:

   ```json
   {
     "enabled": true,
     "endpoint": "http://localhost:8080",
     "auth": { "mode": "none" },
     "agentName": "opencode",
     "contentCapture": true
   }
   ```

   For Grafana Cloud, set `endpoint` to your Sigil API URL (found at `https://<your-grafana>.grafana.net/plugins/grafana-sigil-app`) and use `basic` auth — see the modes below.

2. Register the plugin in your OpenCode configuration.

### Auth modes

- `none` — no authentication (local dev)
- `bearer` — `{ "mode": "bearer", "bearerToken": "..." }`
- `tenant` — `{ "mode": "tenant", "tenantId": "..." }`
- `basic` — `{ "mode": "basic", "tenantId": "<instance-id>", "token": "glc_..." }`

## Development

```bash
# From the repo root
pnpm install
pnpm --filter @grafana/sigil-opencode build
pnpm --filter @grafana/sigil-opencode test
```

The `@grafana/sigil-sdk-js` dependency resolves via pnpm workspace linking to `sdks/js`.
