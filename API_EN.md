# API Reference

Base URL:

```text
http://127.0.0.1:8080/v1
```

All endpoints under `/v1` require authentication. Use either:

```http
Authorization: Bearer <claude.ai sessionKey>
```

or:

```http
X-Claude-Cookie: <full claude.ai browser Cookie header>
```

If `CLAUDE_SESSION_KEY` or `CLAUDE_COOKIE` is configured on the server, callers may omit the matching request header.

## Errors

Errors use an OpenAI-like JSON shape:

```json
{
  "error": {
    "message": "error message",
    "type": "invalid_request_error"
  }
}
```

Common status codes:

| Status | Meaning |
| --- | --- |
| `400` | Invalid request body or missing model. |
| `401` | Missing session key or browser Cookie. |
| `502` | Upstream claude.ai request failed. |

## Models

### `GET /v1/models`

Fetches `/edge-api/bootstrap/{org_id}/app_start?statsig_hashing_algorithm=djb2&growthbook_format=sdk&cache_bust=1&include_system_prompts=false` via GET with the selected account's browser credentials on every call and deduplicates `claude_ai_available_models.models[].model_id`. Upstream failures, schema mismatches, or empty lists return `502`, without a static fallback. `created` is `0` because the web response provides no release timestamp.

The new HAR confirms a 200 response for this GET. The result is a model catalog including models with different subscription requirements, not an account permission list. The implementation neither reads model selection history nor sends PATCH.

```bash
curl http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer <sessionKey>'
```

Response:

```json
{
  "object": "list",
  "data": [
    {
      "id": "claude-sonnet-5",
      "object": "model",
      "created": 0,
      "owned_by": "anthropic"
    }
  ]
}
```

Use the IDs returned by the endpoint. Completion requests no longer use a local fixed allowlist; the web upstream validates model availability.

## Chat Completions

### `POST /v1/chat/completions`

OpenAI-compatible chat completions endpoint.

### Request

```json
{
  "model": "claude-sonnet-5",
  "messages": [
    {"role": "system", "content": "You are concise."},
    {"role": "user", "content": "Say hello"}
  ],
  "stream": false,
  "max_tokens": 1024,
  "temperature": 0.7,
  "top_p": 1
}
```

Fields currently used by the proxy:

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `model` | string | No | Defaults to `DEFAULT_MODEL`; availability is validated by the web upstream. |
| `messages` | array | Yes | At least one message. Roles: `system`, `user`, `assistant`. |
| `stream` | boolean | No | When `true`, returns SSE chunks. |
| `conversation_id` | string | No | Enables persistent conversation mode; the same ID reuses one upstream claude.ai conversation. |
| `max_tokens` | integer | No | Accepted for compatibility. |
| `temperature` | number | No | Accepted for compatibility. |
| `top_p` | number | No | Accepted for compatibility. |
| `attachments` | array | No | Present in request model; upstream file behavior is limited. |

Messages are flattened to a claude.ai prompt using `[System]`, `[Human]`, and `[Assistant]` sections.

### Non-streaming example

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer <sessionKey>' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-sonnet-5",
    "messages": [{"role": "user", "content": "Reply with exactly: pong"}],
    "stream": false
  }'
```

Response:

```json
{
  "id": "chatcmpl-e0f87945",
  "object": "chat.completion",
  "created": 1783594773,
  "model": "claude-sonnet-5",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "pong"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 0,
    "completion_tokens": 1,
    "total_tokens": 0
  }
}
```

### Streaming example

```bash
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer <sessionKey>' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-sonnet-5",
    "messages": [{"role": "user", "content": "Reply with exactly: pong"}],
    "stream": true
  }'
```

Response stream:

```text
data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"pong"},"finish_reason":null}]}

data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
```

## Anthropic Messages

### `POST /v1/messages`

Anthropic Messages-compatible endpoint. The `dev` branch supports client-owned
`tools`, `tool_choice`, `tool_use` and `tool_result.tool_use_id` in both JSON and
named SSE responses. The proxy never executes client tools. See
[Client tools and magpie configuration](docs/CLIENT_TOOLS.md) for the lifecycle,
buffered tool streaming and limitations.

### Request

```json
{
  "model": "claude-sonnet-5",
  "max_tokens": 1024,
  "system": "You are concise.",
  "messages": [
    {"role": "user", "content": "Say hello"}
  ],
  "stream": false
}
```

## Responses API

### `POST /v1/responses`

OpenAI Responses-compatible endpoint.

### Request: string input

```json
{
  "model": "claude-sonnet-5",
  "instructions": "You are concise.",
  "input": "Say hello",
  "stream": false
}
```

## Conversation Continuity

By default, when `conversation_id` is omitted, each request creates a temporary claude.ai conversation and deletes it after completion.

When `conversation_id` is provided, the server stores an in-memory mapping and reuses the same upstream claude.ai conversation:

```json
{
  "conversation_id": "my-chat-001",
  "model": "claude-sonnet-5",
  "messages": [
    {"role": "user", "content": "Continue the previous topic"}
  ]
}
```

Delete a persistent conversation:

```bash
curl -X DELETE http://127.0.0.1:8080/v1/conversations/my-chat-001 \
  -H 'Authorization: Bearer <sessionKey>'
```

Response:

```json
{"id":"my-chat-001","deleted":true}
```

The conversation mapping is stored in memory and is lost after server restart.

## Full Browser Cookie Usage

When the simple Bearer session key gets rate-limited or behaves differently from the browser, pass the full claude.ai browser Cookie:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'X-Claude-Cookie: sessionKey=...; sessionKeyLC=...; anthropic-device-id=...; lastActiveOrg=...; ...' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-sonnet-5",
    "messages": [{"role": "user", "content": "Reply with exactly: pong"}],
    "stream": false
  }'
```

The proxy extracts `sessionKey` from the Cookie when no Bearer token is present.

## Generated Environment in sessionKey Mode

When only a `sessionKey` is provided, the server generates browser-like values and request structure that are normally frontend-generated, such as:

- `sessionKeyLC`
- `anthropic-device-id`
- `activitySessionId`
- `ajs_anonymous_id`
- `__ssid`
- `_dd_s`
- `traceparent` / Datadog RUM headers
- selected UI / analytics cookies
- the claude.ai web `tools` payload in completion requests
- dynamic Referer values: `/new`, `/chat/<conversation_id>`

Signed server-side or Cloudflare-issued cookies are not forged and are not sent in Bearer-only mode:

- `routingHint`
- `cf_clearance`
- `__cf_bm`
- `_cfuvid`

`routingHint` is issued by the claude.ai backend. It commonly appears as `sk-ant-rh-...` and its internal shape is similar to a signed JWT. It is usually created during login, session refresh, account routing initialization, or organization loading. Clients can only store and replay a real value; they cannot generate a valid one locally.

Use `X-Claude-Cookie` or `CLAUDE_COOKIE` with a real browser Cookie header if you need those values.

## Unsupported Endpoints

The following endpoints are intentionally not implemented:

- `/v1/images/generations`
- `/v1/images/edits`
- `/v1/images/variations`

Calls to these paths return Gin's default 404 response because no routes are registered for them.
