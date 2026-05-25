# 11 — Error model

## Why this doc exists

Errors are an API surface. If a client cannot tell "the daemon is rejecting me" from "the network died" from "this memo simply does not exist", every caller invents its own ad-hoc handling and bugs accumulate. Fabric pins down one taxonomy and every layer respects it.

## The three-layer model

1. **Wire layer** — HTTP status code plus a JSON `error` object with a stable `error.code`.
2. **SDK layer** — Python exception classes that mirror the wire codes one-to-one.
3. **Caller layer** — try/except blocks against the exception classes the SDK exposes.

A new error never adds itself silently. Adding an error means: pick a code, document it here, add the exception class, add SDK mapping, add a test. CI rejects PRs that introduce an `error.code` value not present in this document.

## Wire format recap

```json
{
  "error": {
    "code": "fabric.memo.not_found",
    "message": "no memo with that id in this tenant",
    "detail": { "id": "01938e…" },
    "request_id": "01J5W3Q1V8KX9JZ0Y7N1K9M4QF"
  }
}
```

| Field | Always present | Meaning |
|---|---|---|
| `code` | yes | Stable machine identifier. Format: `fabric.<area>.<reason>`. |
| `message` | yes | Human-readable single sentence. Safe to surface in user-facing UI. |
| `detail` | sometimes | Structured context for programmatic handling. Shape is per-code, documented below. |
| `request_id` | yes | Correlation id, same as `X-Request-Id`. |

## Status code mapping

| HTTP | When it appears |
|---|---|
| `400` | Malformed request, missing field, invalid value, unsupported version. |
| `401` | Missing or unparseable `Authorization` header. |
| `403` | Token valid but lacks the required scope, or row-level security denied access. |
| `404` | Target id does not exist in the caller's tenant. (We never disclose existence in other tenants.) |
| `409` | Conflict on a uniqueness constraint or a stale `if_match` revision. |
| `413` | Request body exceeds the configured maximum. |
| `422` | Validation passed at the schema layer but business rules rejected the value. |
| `429` | Rate limit hit. `Retry-After` header set. |
| `499` | Client disconnected before the response was written. (Logged, never sent.) |
| `500` | Unexpected server failure. The `detail` is empty in production; the daemon log carries the stack. |
| `502` | Embed model or other upstream returned an error. |
| `503` | Daemon is starting, draining, or its readiness check fails. |
| `504` | Upstream timed out (embed model usually). |

## Code catalogue

Codes are stable. Once published in this catalogue, the only acceptable change is a clarification of the `message` field. A breaking change requires a new code and a deprecation note.

### Auth and tenancy

| Code | HTTP | Meaning | Detail keys |
|---|---|---|---|
| `fabric.auth.missing` | 401 | No `Authorization` header. | none |
| `fabric.auth.malformed` | 401 | Header present but not `Bearer <token>`. | none |
| `fabric.auth.invalid` | 401 | Token does not match any stored hash. | none |
| `fabric.auth.scope_missing` | 403 | Token is valid but lacks a required scope. | `required: [str]`, `have: [str]` |
| `fabric.auth.tenant_blocked` | 403 | Tenant exists but is disabled. | `tenant_id: str` |

### Request shape

| Code | HTTP | Meaning | Detail keys |
|---|---|---|---|
| `fabric.request.invalid_json` | 400 | Body is not valid JSON. | `parse_offset_bytes: int` |
| `fabric.request.missing_field` | 400 | A required field was absent. | `field: str` |
| `fabric.request.invalid_value` | 400 | A field's value violated its type or range. | `field: str`, `reason: str` |
| `fabric.request.tenant_in_body` | 400 | Caller tried to set `tenant_id` in the body. | none |
| `fabric.request.too_large` | 413 | Body exceeds the maximum. | `max_bytes: int` |
| `fabric.client.unsupported_version` | 400 | `X-Client-Version` is below the daemon's minimum. | `min_version: str` |

### Memos

| Code | HTTP | Meaning | Detail keys |
|---|---|---|---|
| `fabric.memo.not_found` | 404 | No memo with that id in this tenant. | `id: str` |
| `fabric.memo.conflict` | 409 | `if_match` revision did not match the stored revision. | `expected: str`, `actual: str` |
| `fabric.memo.dedup_hit` | 200 | Not an error per se; the supplied content sha256 already exists. Returned in the body of a successful `remember` call so the client can tell. | `existing_id: str` |
| `fabric.memo.type_unknown` | 400 | The supplied `type` value is not in the configured taxonomy. | `type: str`, `allowed: [str]` |
| `fabric.memo.embed_failed` | 502 | Embed model rejected the body. | `upstream_status: int` |

### Search

| Code | HTTP | Meaning | Detail keys |
|---|---|---|---|
| `fabric.search.top_k_invalid` | 400 | `top_k` out of range. | `top_k: int`, `min: int`, `max: int` |
| `fabric.search.query_empty` | 400 | Query string was empty or whitespace-only. | none |
| `fabric.search.cursor_invalid` | 400 | Pagination cursor is malformed or expired. | none |

### Events and streaming

| Code | HTTP | Meaning | Detail keys |
|---|---|---|---|
| `fabric.events.channel_unknown` | 404 | Channel is not declared in server config. | `channel: str` |
| `fabric.events.replay_beyond_window` | 410 | `Last-Event-ID` is older than the retention window. | `oldest_available_id: str` |
| `fabric.events.too_many_streams` | 429 | Per-token concurrent stream cap hit. | `max: int` |
| `fabric.events.payload_too_large` | 413 | Single event payload exceeds 16 KiB. | `max_bytes: int` |

### Graph (when enabled)

| Code | HTTP | Meaning | Detail keys |
|---|---|---|---|
| `fabric.graph.disabled` | 501 | Graph feature is not enabled on this daemon. | none |
| `fabric.graph.symbol_not_found` | 404 | No symbol matched the query. | `symbol: str` |
| `fabric.graph.depth_too_large` | 400 | Requested `depth` exceeds the configured maximum. | `requested: int`, `max: int` |

### Rate limit

| Code | HTTP | Meaning | Detail keys |
|---|---|---|---|
| `fabric.rate.token_limit` | 429 | Per-token RPM exceeded. | `limit: int`, `retry_after_s: float` |
| `fabric.rate.tenant_limit` | 429 | Per-tenant RPM exceeded. | `limit: int`, `retry_after_s: float` |

### Server

| Code | HTTP | Meaning | Detail keys |
|---|---|---|---|
| `fabric.server.shutting_down` | 503 | Daemon is draining; retry against a healthy replica. | none |
| `fabric.server.not_ready` | 503 | Readiness check failing. | `failing: [str]` |
| `fabric.server.upstream_timeout` | 504 | An upstream call (embed, DB) timed out. | `upstream: str`, `waited_ms: int` |
| `fabric.server.internal` | 500 | Unexpected failure. Stack on server. | none in prod, `stack_id: str` in dev |

### Idempotency

| Code | HTTP | Meaning | Detail keys |
|---|---|---|---|
| `fabric.idempotency.key_conflict` | 409 | Same key, different request body. | `original_request_id: str` |
| `fabric.idempotency.replay` | 200 | Not an error; returned via header `X-Idempotent-Replay: 1` plus the original response body. | none |

## Python exception hierarchy

The SDK exposes one base class and a small tree of children. Every wire code maps to a child class via `_CODE_TO_EXC` in `kronaxis_fabric/_exceptions.py`.

```python
class FabricError(Exception):
    """Base class for every fabric SDK error."""
    code: str
    detail: dict
    request_id: str

class FabricConnectionError(FabricError):
    """Network never reached the daemon (DNS, connect, TLS, mid-stream drop)."""

class FabricAuthError(FabricError):
    """Authentication or authorisation problem."""

class FabricRequestError(FabricError):
    """The request shape itself was rejected."""

class FabricNotFoundError(FabricError):
    """Target id does not exist in this tenant."""

class FabricConflictError(FabricError):
    """Optimistic concurrency or idempotency conflict."""

class FabricRateLimitError(FabricError):
    """429 with Retry-After honoured by SDK retries when idempotent."""
    retry_after_s: float

class FabricServerError(FabricError):
    """500/502/503/504 family. SDK retries idempotent calls automatically."""

class FabricFeatureDisabledError(FabricError):
    """Endpoint exists but the server has it disabled (e.g. graph)."""

class FabricVersionError(FabricError):
    """Client and server versions are too far apart."""
```

The mapping table is exhaustive and lives next to the exception classes so a new wire code without a Python mapping is a CI failure.

## How callers should write try/except

The common case is one block:

```python
from kronaxis_fabric import Client, FabricError

try:
    hits = c.memo_search("voice port", top_k=3)
except FabricError as e:
    log.warning("fabric search failed", code=e.code, request_id=e.request_id)
    hits = []
```

When the caller needs to react differently per kind of failure:

```python
from kronaxis_fabric import (
    Client, FabricNotFoundError, FabricRateLimitError, FabricServerError
)

try:
    memo = c.memo_get(id=mid)
except FabricNotFoundError:
    memo = None
except FabricRateLimitError as e:
    sleep(e.retry_after_s)
    memo = c.memo_get(id=mid)
except FabricServerError:
    raise               # let the outer retry loop see it
```

What callers should not do: catch `FabricError` and inspect `.code` as a string. The class hierarchy exists so that the string never needs to leak into business logic.

## Retry semantics by class

The SDK's `RetryPolicy` (doc 08) honours these rules:

| Exception class | Retried automatically? |
|---|---|
| `FabricConnectionError` | yes, with backoff, up to `max_attempts` |
| `FabricServerError` (502/503/504 only) | yes, with backoff |
| `FabricRateLimitError` | yes, honouring `Retry-After` |
| `FabricServerError` (500) | no — 500 is "we don't know what happened", retry is unsafe |
| `FabricAuthError` | no |
| `FabricRequestError` | no — the request body will not change between attempts |
| `FabricNotFoundError` | no |
| `FabricConflictError` | no |
| `FabricFeatureDisabledError` | no |
| `FabricVersionError` | no |

POST requests retry only when no bytes have been written for that attempt. After bytes leave the socket, the SDK defers to `Idempotency-Key` semantics on the server side rather than retrying blindly.

## What "good" looks like in operator logs

When a deployment is healthy, the only errors in journald should be:

- Occasional `FabricRateLimitError` from a misconfigured caller — these surface a misconfigured client, not a fabric bug.
- Rare `FabricConnectionError` after a host reboot, which clears on its own.

If `fabric.server.internal` shows up at all, it is a bug; the audit log plus the request id give enough to reproduce. If `fabric.memo.embed_failed` shows up, the embed host is the next thing to look at, not fabric.

## Adding a new error

Process, in order:

1. Pick a code. Reuse an existing prefix when the meaning matches. Coin a new `<area>` only when no existing area fits.
2. Add the row to the table above with HTTP status, meaning, and detail keys.
3. Add the daemon code path that raises it. Cover it with a test that asserts the JSON response shape.
4. Add the SDK mapping in `_CODE_TO_EXC` and the exception class if a new class is warranted.
5. Add an SDK test that mocks the wire response and asserts the right exception class is raised with `.code` and `.detail` populated.

Done. CI will fail if step 2 is skipped.

## See also

- [`08-python-client.md`](08-python-client.md) for the SDK that owns the exception classes.
- [`09-wire-protocol.md`](09-wire-protocol.md) for the envelope around `error`.
- [`10-observability.md`](10-observability.md) for which codes drive alerts.
