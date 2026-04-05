# Broxy

`broxy` is a standalone local server that exposes an OpenAI-compatible chat API and forwards requests to Amazon Bedrock. It records usage, estimated costs, request metadata, and optional prompt/output logs without requiring external infrastructure.

## What it ships

- OpenAI-style endpoints:
  - `POST /v1/chat/completions`
  - `GET /v1/models`
  - `GET /healthz`
- Embedded admin UI for:
  - request logs
  - token usage and cost summaries
  - client API key management
  - model alias management
- CLI for:
  - initialization
  - admin password reset
  - API key creation and revocation
  - model route management
  - usage and log inspection
- SQLite persistence with no external database

## Current scope

- Supported today:
  - chat completions
  - SSE-compatible streaming responses
  - AWS credential-chain auth to Bedrock
  - bearer-token auth to Bedrock
  - metadata-only logging by default, per-key full content logging as opt-in
- Not implemented yet:
  - embeddings
  - multi-tenant org/workspace auth
  - external database backends
  - true upstream token streaming from Bedrock

The proxy supports client-side streaming semantics by emitting SSE chunks after the upstream Bedrock response completes. That keeps the OpenAI-compatible API surface stable while avoiding Bedrock event-stream complexity in this first implementation.

## Quick start

1. Initialize the app:

```bash
go run ./cmd/broxy init
```

2. Start the server:

```bash
go run ./cmd/broxy serve
```

3. Log into the admin UI at `http://127.0.0.1:8080/` using the generated `admin` password.

4. Create a client key:

```bash
go run ./cmd/broxy apikey create --name local-client
```

5. Add a model alias:

```bash
go run ./cmd/broxy models add \
  --alias claude-haiku-4-5 \
  --model-id us.anthropic.claude-haiku-4-5-20251001-v1:0 \
  --region us-east-1
```

6. Send a request through the proxy:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer YOUR_PROXY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-haiku-4-5",
    "messages": [{"role":"user","content":"Say hello in one sentence."}]
  }'
```

## Bedrock authentication

### AWS credential chain

Default mode uses the standard AWS SDK chain, which covers the common boto3-style paths:

- `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`
- shared credentials and config files
- `AWS_PROFILE`
- SSO / instance or task roles

Set `AWS_REGION` or `AWS_DEFAULT_REGION`, or edit the generated config file.

### Bearer token mode

Set:

```bash
export AWS_BEARER_TOKEN_BEDROCK=...
export BEDROCK_PROXY_UPSTREAM_MODE=bearer
export BEDROCK_PROXY_BEDROCK_REGION=us-east-1
```

Then restart the proxy.

## Pricing and costs

The generated pricing catalog starts with zero-valued placeholder entries. Edit the pricing file shown by `init` to insert your preferred Bedrock pricing values, then restart the proxy. Estimated costs are derived from token usage and that local pricing table.

## Build

```bash
go build ./...
go test ./...
```
