# 09 — Wire protocol

## Scope

This doc fixes the bytes-on-the-wire contract between any client (the Python SDK in doc 08, the MCP shim, future Go clients, curl-driven shell scripts) and the fabric daemon. Anything not specified here is left to the implementation.

The protocol is HTTP/1.1 and HTTP/2 over TLS, with JSON bodies, plus Server-Sent Events for streaming reads. There is no custom binary framing in v1. The goal is "boring HTTP that any language can speak in twenty lines".

## Transport

- Default port: `8200` (plain HTTP, loopback or trusted network).
- TLS port: `8243`, with PEM certs from `--tls-cert` and `--tls-key`.
- HTTP/2 enabled when TLS is on. HTTP/1.1 when plain.
- Keep-alive timeout: 60s.
- Max request body: 4 MiB. Larger payloads are rejected with `413`.

mTLS is optional and off by default. When on, the daemon trusts a CA bundle at `--client-ca-bundle` and rejects connections without a client cert chain that validates.

## Request shape

Every JSON request has the same envelope:

```
POST /v1/<resource>/<verb> HTTP/1.1
Host: dl580:8200
Authorization: Bearer <api_key>
Content-Type: application/json
X-Client-Version: 0.1.0
X-Request-Id: 01J5W3Q1V8KX9JZ0Y7N1K9M4QF       # ULID, client-generated
Content-Length: <n>

{ "field_a": ..., "field_b": ... }
```

`X-Request-Id` is required. The daemon echoes it on the response. Clients use it to correlate logs across the SDK, the daemon, and any downstream service.

## Response shape

Successful response:

```
HTTP/1.1 200 OK
Content-Type: application/json
X-Server-Version: 0.1.3
X-Request-Id: 01J5W3Q1V8KX9JZ0Y7N1K9M4QF
X-Latency-Ms: 7

{ "result_field_a": ..., "result_field_b": ... }
```

Error response:

```
HTTP/1.1 4xx-or-5xx
Content-Type: application/json
X-Request-Id: 01J5W3Q1V8KX9JZ0Y7N1K9M4QF

{
  "error": {
    "code": "fabric.memo.not_found",
    "message": "no memo with that id in this tenant",
    "detail": { "id": "..." },
    "request_id": "01J5W3Q1V8KX9JZ0Y7N1K9M4QF"
  }
}
```

The `error.code` field is the machine-friendly identifier. The exception hierarchy in doc 11 maps these codes one-to-one onto Python exception classes.

## Field conventions

- All field names are `snake_case`.
- All timestamps are RFC 3339 UTC with millisecond precision (`2026-05-25T13:42:11.034Z`).
- All ids are UUIDv7 strings unless explicitly noted as ULID.
- All durations are integer milliseconds in a field suffixed `_ms`.
- All sizes are integer bytes in a field suffixed `_bytes`.
- All money values are integer pence in a field suffixed `_pence`.
- `null` means "absent or not applicable". Omitting a field carries the same meaning.

These rules are mechanically enforced by the daemon's response validator in CI. A response that violates them fails the build.

## Pagination

List endpoints accept:

```
{
  "limit":  100,                 # max 1000
  "cursor": "<opaque>",          # absent on first page
  "filter": { ... }              # per-endpoint
}
```

Response:

```
{
  "items": [...],
  "next_cursor": "<opaque>",     # null when no more pages
  "total_estimate": 12345        # may be approximate
}
```

The cursor is opaque to clients. It encodes a (sort key, id) tuple plus a tenant binding so it cannot be replayed across tenants.

## Streaming reads (SSE)

`GET /v1/events/subscribe?channel=<name>&since=<rfc3339|"now">` opens an SSE stream.

```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-store

id: 01J5W3Q1V8KX9JZ0Y7N1K9M4QF
event: message
data: {"channel":"health","ts":"2026-05-25T13:42:11.034Z","payload":{...}}

id: 01J5W3Q1V8KX9JZ0Y7N1K9M4QH
event: message
data: {"channel":"health","ts":"2026-05-25T13:42:12.501Z","payload":{...}}

: keep-alive
```

- `id:` is the event's ULID. Clients pass the last seen id as `Last-Event-ID` header on reconnect; the daemon replays from there if still in the retention window.
- Keep-alive comments (`: keep-alive`) every 15s so intermediate proxies do not idle-close the stream.
- Event payloads always have `channel`, `ts`, and `payload` keys. Channel-specific schema lives under `payload`.

## Idempotency

Mutating endpoints accept an `Idempotency-Key` header. The daemon stores the response body for 24h keyed by `(api_key, idempotency_key)`. A retry with the same key returns the original response unchanged, including status code.

This is how the SDK's auto-retry stays safe across `POST` requests: it sets `Idempotency-Key` to a per-request ULID.

## Authentication

Bearer tokens by default. Tokens are opaque strings issued by `fabric admin token create`. The daemon stores only a sha256 of each token plus a label and scope set.

Scopes:

| Scope | What it allows |
|---|---|
| `memo:read` | `memo_search`, `memo_get`, `memo_iter` |
| `memo:write` | adds `memo_remember`, `memo_update`, `memo_delete` |
| `events:read` | `events_subscribe` |
| `events:write` | `events_publish` |
| `graph:read` | `graph_query`, `graph_subgraph` |
| `admin` | every admin endpoint |

A token missing the required scope receives `403` with `error.code=fabric.auth.scope_missing`.

## Tenancy

Every request is bound to a tenant. The default deployment runs single-tenant with `tenant_id="default"`. Multi-tenant deployments derive the tenant from the token. Cross-tenant reads are impossible because every row in the canonical store has `tenant_id` enforced by row-level security.

Requests never carry `tenant_id` in the body. Attempting to set it returns `400 fabric.request.tenant_in_body`.

## Versioning

- The URL path carries the major version: `/v1/...`.
- The daemon advertises its full version via `X-Server-Version` on every response.
- A new endpoint added to v1 is additive. Removing or changing an endpoint requires a `/v2/` path and a parallel rollout window of at least one minor server release.
- Clients that send `X-Client-Version` older than the daemon's `min_supported_client_version` receive `400 fabric.client.unsupported_version` with the minimum in `detail.min_version`.

## Time and clock

The daemon's clock is authoritative for any `ts` field it stamps. Clients send their own clock only in `Idempotency-Key` (where it does not matter for correctness). The daemon rejects requests whose `Date:` header skews by more than five minutes when mTLS is on, because that skew usually indicates a misconfigured CI worker.

## Compression

- Request bodies may be `Content-Encoding: gzip`. Daemon decompresses transparently.
- Response bodies are gzipped only when the client sets `Accept-Encoding: gzip` and the response exceeds 1 KiB.
- SSE streams are never compressed; per-event payloads stay below 16 KiB by convention.

## Limits

| Limit | Default | Notes |
|---|---|---|
| Max request body | 4 MiB | Larger memos must be chunked client-side |
| Max search `top_k` | 100 | Higher values truncated with `X-Truncated: 1` header |
| Max events SSE replay window | 24h | Older events require a search by time range |
| Per-token requests per minute | 600 | Rate-limit headers per RFC 6585 |
| Per-token concurrent SSE streams | 8 | 9th returns `429` |

Limits live in the daemon config so operators can adjust per deployment.

## Health, ready, and metrics

| Endpoint | Purpose | Auth |
|---|---|---|
| `GET /healthz` | Liveness probe. Returns `200 {"ok":true}` if process is up. | None |
| `GET /readyz` | Readiness. Returns `200 {"ok":true}` only when DB + embed model reachable. | None |
| `GET /metrics` | Prometheus exposition. | Optional bearer; defaults to localhost-only |

Health endpoints are intentionally separate from the versioned `/v1/...` namespace so probes never need a token and never break across daemon major versions.

## A complete request, end to end

```
$ curl -sS https://dl580:8243/v1/memo/search \
    -H 'Authorization: Bearer demo-key' \
    -H 'Content-Type: application/json' \
    -H 'X-Request-Id: 01J5W3Q1V8KX9JZ0Y7N1K9M4QF' \
    -d '{"query":"lucy port","top_k":3}'

HTTP/2 200
content-type: application/json
x-server-version: 0.1.3
x-request-id: 01J5W3Q1V8KX9JZ0Y7N1K9M4QF
x-latency-ms: 11

{
  "items": [
    {"id":"0193…","title":"F5-TTS Lucy port","body_excerpt":"…port 8872…","score":0.94,"type":"reference","ts":"2026-05-25T13:42:11.034Z"},
    {"id":"0193…","title":"Voice infra inventory","body_excerpt":"…Lucy on port 8872…","score":0.81,"type":"project","ts":"2026-05-24T08:01:33.512Z"},
    {"id":"0193…","title":"Voxpad runtime notes","body_excerpt":"…shares port 8872…","score":0.74,"type":"reference","ts":"2026-05-22T17:14:02.119Z"}
  ],
  "next_cursor": null,
  "total_estimate": 3
}
```

Twenty lines, one round trip, one type-checked response. That is the entire client experience.

## See also

- [`03-ipc-design.md`](03-ipc-design.md) for the rationale behind HTTP over a custom protocol.
- [`08-python-client.md`](08-python-client.md) for the SDK that wraps this protocol.
- [`11-error-model.md`](11-error-model.md) for every `error.code` value and its meaning.
