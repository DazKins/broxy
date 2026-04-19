# Broxy

`broxy` is a standalone local server that exposes an OpenAI-compatible chat API and forwards requests to Amazon Bedrock. It records usage, estimated costs, request metadata, and optional prompt/output logs without requiring external infrastructure.

## What it ships

- OpenAI-style endpoints:
  - `POST /v1/chat/completions`
  - `GET /v1/responses` for websocket Responses clients
  - `POST /v1/responses`
  - `GET /v1/responses/{response_id}`
  - `GET /v1/models`
  - `GET /healthz`
- Embedded admin UI for:
  - request logs
  - token usage and cost summaries
  - client API key management
  - model alias management
- CLI for:
  - initialization
  - background service management
  - admin password reset
  - API key creation and revocation
  - model route management
  - usage and log inspection
- SQLite persistence with no external database

## Install

Broxy is packaged as a single binary plus a native user service:

- macOS: LaunchAgent under `~/Library/LaunchAgents/com.broxy.agent.plist`
- Linux: systemd user unit under `${XDG_CONFIG_HOME:-~/.config}/systemd/user/broxy.service`

Default install:

```bash
curl -fsSL https://raw.githubusercontent.com/DazKins/broxy/main/scripts/install.sh | sh
```

The installer:

1. downloads the latest GitHub Release archive for your OS and CPU
2. installs `broxy` to `~/.local/bin`
3. initializes config and state on first install
4. installs the background service
5. starts or restarts the service

Version override:

```bash
BROXY_VERSION=v0.1.0 curl -fsSL https://raw.githubusercontent.com/DazKins/broxy/main/scripts/install.sh | sh
```

## Default paths

Broxy stores all app-owned files under `~/.broxy/` on every platform:

- root: `~/.broxy/`
- config: `~/.broxy/config.json`
- pricing: `~/.broxy/pricing.json`
- database: `~/.broxy/broxy.db`
- logs: `~/.broxy/logs/`

Inspect the effective paths on any machine:

```bash
broxy config path
```

## Background service

Install and manage the background server:

```bash
broxy service install
broxy service start
broxy service status
broxy service restart
broxy service reset
broxy service logs --lines 100
broxy service stop
broxy service uninstall
```

On Linux, the service runs as a user service and starts automatically after that user logs in. On macOS, it runs as a LaunchAgent for the logged-in user.

Add environment variables that the service should always receive to the config file's `env` block, then restart the service:

```json
{
  "env": {
    "BROXY_LOG_LEVEL": "debug",
    "HTTPS_PROXY": "http://127.0.0.1:7890",
    "NO_PROXY": "127.0.0.1,localhost"
  }
}
```

`BROXY_LOG_LEVEL=debug` enables debug logging, including raw upstream Responses API payloads in the service logs. Re-run `broxy service install` after changing `env` if you want the native systemd or launchd service definition to show the updated values too.

## First-time setup without the installer

```bash
go build -o ./broxy ./cmd/broxy
./broxy init
./broxy service install
./broxy service start
```

Reset the local admin password if needed:

```bash
broxy admin reset-password
```

## CLI quick start

Create a client key:

```bash
broxy apikey create --name local-client
```

Add a model alias:

```bash
broxy models add \
  --alias claude-haiku-4-5 \
  --model-id us.anthropic.claude-haiku-4-5-20251001-v1:0 \
  --region us-east-1
```

Send a request through the proxy:

```bash
curl http://127.0.0.1:27699/v1/chat/completions \
  -H "Authorization: Bearer YOUR_PROXY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-haiku-4-5",
    "messages": [{"role":"user","content":"Say hello in one sentence."}]
  }'
```

Responses API example:

```bash
curl http://127.0.0.1:27699/v1/responses \
  -H "Authorization: Bearer YOUR_PROXY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-haiku-4-5",
    "instructions": "You are a helpful assistant.",
    "input": "Say hello in one sentence."
  }'
```

The Responses API support is currently text-oriented:

- string or message-style `input`
- `instructions`
- websocket `response.create` requests on `/v1/responses`
- function tool definitions via `tools`
- model-emitted function calls
- text or JSON `function_call_output` follow-up inputs
- `item_reference` follow-ups used by Responses clients that store prior output items
- `previous_response_id` chaining against in-memory server state
- SSE streaming for text output
- websocket streaming for text output and function-call argument deltas
- passthrough tolerance for built-in `web_search` tool declarations from agent clients

Currently unsupported:

- execution of built-in non-function tools such as `web_search`
- multimodal tool outputs
- persisted response storage beyond the running server process

Log into the admin UI at `http://127.0.0.1:27699/` with the generated `admin` password.

## Using Broxy with Codex

Codex can use Broxy as a custom OpenAI-compatible model provider. First create a Broxy client key:

```bash
broxy apikey create --name codex
```

Export that key in the shell where you start Codex:

```bash
export BROXY_API_KEY="YOUR_PROXY_KEY"
```

Then add a provider and profile to `~/.codex/config.toml`:

```toml
[profiles.broxy]
model_provider = "broxy"
model = "claude-opus-4.6"

[model_providers.broxy]
name = "Broxy"
base_url = "http://127.0.0.1:27699/v1" // replace with remote url if not using local broxy
env_key = "BROXY_API_KEY"
requires_openai_auth = false
supports_websockets = false
```

Make sure the configured model is available through Broxy. For Bedrock inference profiles, you can sync them into Broxy routes:

```bash
broxy models sync
```

Or add the route manually:

```bash
broxy models add \
  --alias claude-opus-4.6 \
  --model-id global.anthropic.claude-opus-4-6-v1 \
  --region us-east-1
```

Start Codex with the profile:

```bash
codex --profile broxy
```

## Using Broxy with opencode

opencode can use Broxy as a custom OpenAI provider through Broxy's Responses-compatible endpoint. First create a Broxy client key:

```bash
broxy apikey create --name opencode
```

Export that key in the shell where you start opencode:

```bash
export BROXY_API_KEY="YOUR_PROXY_KEY"
```

Then create or update `~/.config/opencode/opencode.json` or a project-level `opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "model": "broxy/claude-opus-4.6",
  "small_model": "broxy/claude-haiku-4.5",
  "provider": {
    "broxy": {
      "npm": "@ai-sdk/openai",
      "name": "Broxy",
      "options": {
        "baseURL": "http://127.0.0.1:27699/v1",  // replace with remote url if not using local broxy
        "apiKey": "{env:BROXY_API_KEY}"
      },
      "models": {
        "claude-opus-4.6": {
          "name": "Claude Opus 4.6 via Broxy",
          "limit": {
            "context": 200000,
            "output": 32000
          }
        },
        "claude-haiku-4.5": {
          "name": "Claude Haiku 4.5 via Broxy",
          "limit": {
            "context": 200000,
            "output": 32000
          }
        }
      }
    }
  }
}
```

Make sure each configured opencode model ID is also available as a Broxy model alias. For example:

```bash
broxy models add \
  --alias claude-opus-4.6 \
  --model-id global.anthropic.claude-opus-4-6-v1 \
  --region us-east-1
```

Use `@ai-sdk/openai` for this provider so opencode talks to Broxy through `/v1/responses`. Broxy's `/v1/chat/completions` endpoint is suitable for plain chat clients, but the Responses endpoint is the right path for opencode because it supports function tool calls and tool result follow-ups.

## Using Broxy with Claude Code

Claude Code can use Broxy through the Anthropic Messages-compatible endpoint. First create a Broxy client key:

```bash
broxy apikey create --name claude-code
```

Export that key in the shell where you start Claude Code:

```bash
export BROXY_API_KEY="YOUR_PROXY_KEY"
```

Then create or update your user-level `~/.claude/settings.json`:

```json
{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "model": "claude-opus-4.6",
  "apiKeyHelper": "test -n \"$BROXY_API_KEY\" && printf '%s' \"$BROXY_API_KEY\"",
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:27699", // replace with remote url if not using local broxy
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-4.6",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-4.6",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "claude-haiku-4.5",
    "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"
  }
}
```

For one project only, put the same JSON in `.claude/settings.local.json` instead.

Make sure each configured model is available as a Broxy model alias:

```bash
broxy models add \
  --alias claude-opus-4.6 \
  --model-id global.anthropic.claude-opus-4-6-v1 \
  --region us-east-1

broxy models add \
  --alias claude-sonnet-4.6 \
  --model-id global.anthropic.claude-sonnet-4-6 \
  --region us-east-1

broxy models add \
  --alias claude-haiku-4.5 \
  --model-id global.anthropic.claude-haiku-4-5-20251001-v1:0 \
  --region us-east-1
```

Claude Code's `apiKeyHelper` reads the key from `BROXY_API_KEY`, while the `settings.json` `env` block applies the remaining gateway settings to Claude sessions without requiring extra shell exports.

Claude Code may issue Anthropic server-tool declarations such as `web_search_20250305` when its WebSearch tool is used. Broxy can execute those searches through a configured search provider, then pass the results back to Bedrock as tool output before returning the final assistant message.

To enable Brave Search, add a `search` block to the config file shown by `broxy config path`:

```json
{
  "search": {
    "provider": "brave",
    "brave_api_key": "YOUR_BRAVE_SEARCH_API_KEY",
    "max_results": 5,
    "timeout_seconds": 10,
    "country": "us",
    "search_lang": "en"
  }
}
```

If no search provider is configured, Broxy returns an assistant message explaining how to configure Brave instead of forwarding the unsupported server-side web search tool to Bedrock.

## Using Broxy with OpenClaw

OpenClaw can use Broxy as a custom OpenAI-compatible provider through the `models.providers` configuration. First create a Broxy client key:

```bash
broxy apikey create --name openclaw
```

Export that key in the shell where you start OpenClaw:

```bash
export BROXY_API_KEY="YOUR_PROXY_KEY"
```

Then add a Broxy provider block to your `~/.openclaw/openclaw.json`:

```json5
{
  models: {
    mode: "merge",
    providers: {
      broxy: {
        baseUrl: "http://127.0.0.1:27699/v1", // replace with remote url if not using local broxy
        apiKey: "${BROXY_API_KEY}",
        api: "openai-responses",
        models: [
          {
            id: "claude-opus-4.6",
            name: "Claude Opus 4.6 via Broxy",
            reasoning: false,
            input: ["text"],
            cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
            contextWindow: 200000,
            maxTokens: 32000,
          }
        ],
      },
    },
  },
  agents: {
    defaults: {
      model: {
        primary: "broxy/claude-opus-4.6",
      },
    },
  },
}
```

Make sure each configured model ID is available as a Broxy model alias:

```bash
broxy models add \
  --alias claude-opus-4.6 \
  --model-id global.anthropic.claude-opus-4-6-v1 \
  --region us-east-1
```

Use `api: "openai-responses"` so OpenClaw talks to Broxy through `/v1/responses`, which supports function tool calls and tool result follow-ups. Cost fields are set to zero because Broxy tracks usage and costs independently.

## Bedrock authentication

Configure Bedrock in the generated config file shown by `broxy config path`.
The relevant settings live under the `upstream` block.

### AWS credential chain

Default mode uses the standard AWS SDK chain. Put the Broxy-specific region and
profile selection in config:

```json
{
  "upstream": {
    "mode": "aws",
    "region": "us-east-1",
    "profile": "sso-prod"
  }
}
```

Use the normal AWS shared credentials/config files, SSO, credential process,
instance roles, or task roles for the selected profile. Do not store long-lived
AWS access keys in Broxy config.

### Bearer token mode

Set bearer mode directly in the same config file:

```json
{
  "upstream": {
    "mode": "bearer",
    "region": "us-east-1",
    "bearer_token": "..."
  }
}
```

## Pricing and costs

The generated pricing catalog starts empty. Adding a model route creates a zero-valued pricing entry for that Bedrock model and region; removing the final route for that pair removes the pricing entry. Edit the pricing file shown by `broxy config path`, then restart the service. Estimated costs are derived from token usage and that local pricing table.

## Development

Build and test locally:

```bash
go build ./...
go test ./...
```

Create release artifacts locally:

```bash
goreleaser check
goreleaser release --snapshot --clean
```

GitHub Actions:

- `.github/workflows/ci.yml` runs tests and cross-build smoke checks
- `.github/workflows/release.yml` publishes tagged releases via GoReleaser
