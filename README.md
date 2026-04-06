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

Override examples:

```bash
BROXY_VERSION=v0.1.0 curl -fsSL https://raw.githubusercontent.com/DazKins/broxy/main/scripts/install.sh | sh
BROXY_CONFIG_PATH="$HOME/.broxy-dev/config.json" curl -fsSL https://raw.githubusercontent.com/DazKins/broxy/main/scripts/install.sh | sh
```

## Default paths

Linux:

- config: `${XDG_CONFIG_HOME:-~/.config}/broxy/config.json`
- pricing: `${XDG_CONFIG_HOME:-~/.config}/broxy/pricing.json`
- state: `${XDG_STATE_HOME:-~/.local/state}/broxy/`
- database: `${XDG_STATE_HOME:-~/.local/state}/broxy/broxy.db`
- logs: `${XDG_STATE_HOME:-~/.local/state}/broxy/logs/`

macOS:

- root: `~/Library/Application Support/broxy/`
- config: `~/Library/Application Support/broxy/config.json`
- pricing: `~/Library/Application Support/broxy/pricing.json`
- database: `~/Library/Application Support/broxy/broxy.db`
- logs: `~/Library/Application Support/broxy/logs/`

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
broxy service logs --lines 100
broxy service stop
broxy service uninstall
```

On Linux, the service runs as a user service and starts automatically after that user logs in. On macOS, it runs as a LaunchAgent for the logged-in user.

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
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer YOUR_PROXY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-haiku-4-5",
    "messages": [{"role":"user","content":"Say hello in one sentence."}]
  }'
```

Responses API example:

```bash
curl http://127.0.0.1:8080/v1/responses \
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
- `previous_response_id` chaining against in-memory server state
- SSE streaming for text output
- websocket streaming for text output and function-call argument deltas
- passthrough tolerance for built-in `web_search` tool declarations from agent clients

Currently unsupported:

- execution of built-in non-function tools such as `web_search`
- multimodal tool outputs
- persisted response storage beyond the running server process

Log into the admin UI at `http://127.0.0.1:8080/` with the generated `admin` password.

## Bedrock authentication

### AWS credential chain

Default mode uses the standard AWS SDK chain:

- `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`
- shared credentials and config files
- `AWS_PROFILE`
- SSO / instance or task roles

Set `AWS_REGION` or `AWS_DEFAULT_REGION`, or edit the generated config file.

### Bearer token mode

You can set bearer mode in config or through environment variables:

```bash
export AWS_BEARER_TOKEN_BEDROCK=...
export BEDROCK_PROXY_UPSTREAM_MODE=bearer
export BEDROCK_PROXY_BEDROCK_REGION=us-east-1
broxy service restart
```

## Pricing and costs

The generated pricing catalog starts with zero-valued placeholder entries. Edit the pricing file shown by `broxy config path`, then restart the service. Estimated costs are derived from token usage and that local pricing table.

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
