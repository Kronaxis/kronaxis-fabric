# 08 — Python client SDK

## Goal

A small, idiomatic Python client for the fabric daemon. Synchronous and asynchronous APIs. Strongly typed. Installable from PyPI as `kronaxis-fabric`. Zero dependencies beyond `httpx`, `pydantic`, and `anyio`.

The client is what end-users actually touch. The Go daemon is the engine; the Python SDK is the surface. If the SDK feels heavy, the whole project feels heavy.

## Design constraints

1. **Pure Python, no native extensions.** Wheels stay universal. `pip install` works on every platform without a compiler.
2. **One import, one client.** `from kronaxis_fabric import Client` — that is the whole public surface that 90% of users need.
3. **Both sync and async with a shared core.** `Client` is sync; `AsyncClient` is async. Both wrap the same `_Transport` class.
4. **Typed responses.** Every response is a `pydantic.BaseModel` or a `dataclass`. No raw dicts leaked.
5. **Errors raise, never silently return None.** Network errors → `FabricConnectionError`. Auth errors → `FabricAuthError`. Server errors → `FabricServerError(code, detail)`.
6. **No global state.** No module-level config, no monkey-patched `requests`. Everything lives on the client instance.

## Public surface

### Construction

```python
from kronaxis_fabric import Client

c = Client(
    base_url="http://dl580:8200",
    api_key="…",                 # required unless server runs with auth disabled
    timeout=10.0,                 # seconds for any single call
    retries=3,                    # automatic retry on transient failure
    user_agent="my-script/0.1",   # appears in fabric request log
)
```

Async variant is identical:

```python
from kronaxis_fabric import AsyncClient

async with AsyncClient(base_url="…", api_key="…") as c:
    result = await c.memo_search(query="lucy port", top_k=3)
```

The sync `Client` is also a context manager so connection pools close cleanly:

```python
with Client(base_url="…", api_key="…") as c:
    ...
```

### Memo operations

```python
# Save a memo
memo = c.memo_remember(
    title="F5-TTS Lucy port",
    body="Lucy F5-TTS clone runs on DL580 port 8872.",
    type="reference",
    tags=["voice", "infrastructure"],
)
# → Memo(id=UUID, title=..., body=..., score=None, ...)

# Search
results = c.memo_search(query="voice synthesis ports", top_k=5)
# → list[MemoHit] each with id, title, body_excerpt, score, type, ts

# Fetch one by id
memo = c.memo_get(id="…")
# → Memo | None

# Update
memo = c.memo_update(id="…", body="…revised body…")

# Delete (soft, sets deleted_at)
c.memo_delete(id="…")

# Iterate the corpus (server-side cursor, async-friendly)
for batch in c.memo_iter(type="reference", batch_size=200):
    for memo in batch:
        ...
```

`memo_iter` returns an iterator of batches rather than yielding one at a time so callers can decide whether to stream or buffer.

### Event operations

```python
# Subscribe to a channel (blocking generator)
for event in c.events_subscribe(channel="health", since="now"):
    print(event.payload)

# Same call, async
async for event in c.events_subscribe(channel="health", since="now"):
    print(event.payload)
```

`events_subscribe` uses Server-Sent Events under the hood. The sync version uses a worker thread; the async version uses `httpx.AsyncClient.stream`.

```python
# Publish (fire-and-forget; raises on auth/network errors only)
c.events_publish(channel="ops", payload={"kind": "deploy", "tag": "v1.2.3"})
```

### Graph operations (optional)

If the server has graph features enabled:

```python
sym = c.graph_query(symbol="synth_line_lucy")
# → list[Symbol] with file, line, body_excerpt, kind

sub = c.graph_subgraph(symbol="F5TTS", depth=2)
# → list[GraphEdge] with src, dst, edge_kind
```

If graph isn't enabled on the server, these raise `FabricFeatureDisabledError`.

## Internals

### Module layout

```
kronaxis_fabric/
  __init__.py           # exports: Client, AsyncClient, exceptions, models
  _transport.py         # httpx wrapper, retries, auth header injection
  _models.py            # pydantic models for every request/response
  _exceptions.py        # the exception hierarchy
  _sync.py              # Client (sync facade)
  _async.py             # AsyncClient (async facade)
  _sse.py               # SSE parser, used by both
  _version.py           # __version__
  py.typed              # PEP 561 marker
```

Two facades on one transport keeps behaviour identical. Bug fixes in `_transport.py` apply to both.

### Why `httpx` over `requests`

- HTTP/2 support out of the box (multiplexes streams to the daemon).
- Connection pool that is actually pool-shaped, not per-request.
- Identical API for sync and async.
- Built-in timeout primitives that distinguish connect/read/write/pool.
- Active maintenance.

### Why `pydantic` v2

- Validation at the SDK boundary catches server schema drift early.
- `model_dump()` produces JSON-ready dicts cleanly.
- Strict mode by default; we accept the server's schema verbatim.
- Code completion via generated `.pyi` is good in editors.

The single tradeoff: pydantic v2 has a Rust core. The pure-Python pin is `pydantic.v1` only. We accept the binary wheel because pydantic ships wheels for every relevant target.

### Retry policy

```python
class RetryPolicy:
    max_attempts: int = 3
    backoff_initial_s: float = 0.25
    backoff_max_s: float = 5.0
    backoff_factor: float = 2.0
    jitter_pct: float = 0.30
    retry_on: tuple[int, ...] = (429, 502, 503, 504)
```

Idempotent calls (GETs, search, get-by-id) retry by default. Non-idempotent calls (POST `memo_remember`, POST `events_publish`) retry only on connection errors before any bytes are sent. The transport keeps a `request_started` flag and refuses to retry once it has flipped.

### Auth header

Every call sets `Authorization: Bearer <api_key>`. If the server is configured for mutual TLS, the client picks up `KRONAXIS_FABRIC_CA_BUNDLE` from environment and `KRONAXIS_FABRIC_CLIENT_CERT`/`_KEY` for mTLS. No code changes needed for cert rotation.

## Examples

### Replace a tiny `requests`-based memo logger

Before:

```python
import requests
requests.post("http://dl580:8200/v1/memo/remember",
              headers={"Authorization": f"Bearer {KEY}"},
              json={"title": t, "body": b, "type": "log"})
```

After:

```python
from kronaxis_fabric import Client
c = Client(base_url="http://dl580:8200", api_key=KEY)
c.memo_remember(title=t, body=b, type="log")
```

Five lines saved, one type-checked surface, retries free.

### Streaming health events

```python
from kronaxis_fabric import Client

with Client(base_url="http://dl580:8200", api_key=KEY) as c:
    for ev in c.events_subscribe(channel="health"):
        if ev.payload.get("severity") == "critical":
            page_oncall(ev)
```

The generator runs until the connection drops or the iterator is closed. Internal reconnect logic re-establishes within `backoff_initial_s` on transient drops.

### Async batch search

```python
import asyncio
from kronaxis_fabric import AsyncClient

queries = ["voice", "email", "video", "whatsapp"]

async def run() -> None:
    async with AsyncClient(base_url="…", api_key=KEY) as c:
        results = await asyncio.gather(*[c.memo_search(q, top_k=3) for q in queries])
        for q, r in zip(queries, results):
            print(q, len(r))

asyncio.run(run())
```

Four searches in parallel, single HTTP/2 connection, total wall time of the slowest single search rather than the sum.

## Packaging

```toml
# pyproject.toml
[project]
name = "kronaxis-fabric"
version = "0.1.0"
description = "Python client for kronaxis-fabric"
readme = "README.md"
license = { text = "BSL-1.1" }
requires-python = ">=3.10"
dependencies = [
    "httpx>=0.27,<0.29",
    "pydantic>=2.7,<3",
    "anyio>=4,<5",
]

[project.optional-dependencies]
dev = ["pytest>=8", "pytest-asyncio>=0.23", "respx>=0.21", "mypy>=1.10", "ruff>=0.5"]
```

Build with `python -m build`. Publish via `twine upload`. Wheels are pure-Python and universal.

## Testing the client without a daemon

A `respx` fixture mocks the HTTP layer so unit tests run without standing up fabric:

```python
import respx, httpx
from kronaxis_fabric import Client

@respx.mock
def test_memo_search_returns_hits():
    respx.post("http://test/v1/memo/search").mock(
        return_value=httpx.Response(200, json={"results":[{"id":"u","title":"t","body_excerpt":"b","score":0.9,"type":"reference","ts":"2026-05-25T00:00:00Z"}]})
    )
    c = Client(base_url="http://test", api_key="k")
    hits = c.memo_search("q", top_k=1)
    assert len(hits) == 1
    assert hits[0].score == 0.9
```

Integration tests run against a real daemon in CI via `docker compose`.

## Versioning

The SDK follows the daemon's major version. SDK `0.x` works against daemon `0.x`. Breaking API changes bump the major. Deprecations carry one minor version of warning before removal.

The transport sends `X-Client-Version` and the daemon sends `X-Server-Version` back. On mismatch beyond the supported window, the SDK raises `FabricVersionError` with a clear message rather than allowing a silent miscall.

## What this doc does not cover

- The exact MCP wrapper around the SDK. That lives in a separate `kronaxis-fabric-mcp` package and is a thin shim. See doc 12.
- Library-level caching of search results. Out of scope for v1; the daemon already caches.
- A CLI. The SDK is a library. A CLI can be built on top in `kronaxis-fabric-cli` if there is demand.

## See also

- [`03-ipc-design.md`](03-ipc-design.md) for the wire shape this SDK targets.
- [`09-wire-protocol.md`](09-wire-protocol.md) for the exact framing the transport speaks.
- [`11-error-model.md`](11-error-model.md) for the exception hierarchy in detail.
