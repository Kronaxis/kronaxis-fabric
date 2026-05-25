# 12 — Roadmap

## How to read this doc

Three sections: what v1 ships (a recap, not a re-pitch), what v2 picks up after v1 has been used in anger for a quarter, and what stays out indefinitely. Items move between sections only when evidence justifies the move.

The point of writing the roadmap down is so that "wouldn't it be cool if" conversations have a place to land without disrupting the v1 shipping plan in doc 06.

## v1 — six weeks, recap

Already covered in doc 06. One-line summary so this doc is self-contained:

- Week 1: memos + search + HTTP/MCP API
- Week 2: multi-host federation over Postgres LISTEN/NOTIFY
- Week 3: live code graph via tree-sitter and inotify
- Week 4: CI and systemd events as first-class memos
- Week 5: task and capability registry
- Week 6: persona memory unification, routing outcomes recorded

Exit criteria for v1: all six weekly demos accepted, observability dashboards green for two consecutive weeks, no manual intervention needed for a full operator workday.

## v2 — months three to six

Each item below has a trigger condition. Without the trigger, the item stays in v2; with the trigger, it gets promoted to a sprint.

### MCP shim package

A separate PyPI package `kronaxis-fabric-mcp` that exposes every SDK call as a Model Context Protocol tool. Useful for Claude Code, Cursor, and any other MCP-aware client.

- **Trigger**: at least one other person uses the SDK and wants MCP access.
- **Estimate**: three days.
- **Risk**: low. MCP server stubs are standard.

### CLI

`fabric` shell command for ad-hoc queries from a terminal. Wraps the SDK. Output formats: pretty, JSON, JSONL.

```
fabric memo search "voice port" --top-k 3
fabric memo remember --title "…" --body "…" --type reference
fabric events tail --channel health
```

- **Trigger**: operator finds themselves writing the same five-line Python snippet twice in a week.
- **Estimate**: two days.
- **Risk**: low. Same surface as the SDK.

### gRPC transport

A second transport that speaks gRPC for callers who need lower per-call overhead than HTTP/1.1 plus JSON. Same logical API; generated `.proto` stubs.

- **Trigger**: a real caller measures 20%+ overhead from JSON parsing on a hot path.
- **Estimate**: one week including parity tests.
- **Risk**: medium. Doubles the API-surface maintenance burden.

### Distributed embeddings

Spread embedding work across multiple Ollama instances behind a small round-robin. Today one instance is fine; if the corpus grows past a million memos, one instance becomes the bottleneck.

- **Trigger**: `fabric_embed_call_duration_seconds` p95 above 200ms for a week of normal load, AND moving to a faster single model is not preferred.
- **Estimate**: three days.
- **Risk**: low. Round-robin is well understood.

### Read replicas

Streaming replication from the canonical Postgres to one or more read replicas. Search and read endpoints fan out to replicas; writes stay on the primary.

- **Trigger**: write-amplification from search traffic causes pg_pool saturation on the primary.
- **Estimate**: one week.
- **Risk**: medium. Replica lag becomes a thing callers must reason about for read-after-write scenarios.

### Larger embed models

Today's `all-minilm` is 384 dimensions and fast. Migrating to a 1024-dimensional model improves recall on long-form memos at the cost of more storage and slower inference.

- **Trigger**: A/B test shows >5pp recall improvement on a held-out evaluation set drawn from real operator queries.
- **Estimate**: one week including a backfill re-embed pass.
- **Risk**: medium. Requires schema change to widen the vector column, plus a re-embed migration.

### Web UI

A small dashboard for non-CLI users to search memos, browse the graph, watch events. Read-only at first.

- **Trigger**: a non-operator user needs to consume fabric data and the SDK does not fit their workflow.
- **Estimate**: two weeks for a minimum-viable read-only UI.
- **Risk**: low to build, medium to maintain. UIs accumulate scope.

### Long-term retention tiering

Move memos older than 180 days to a cold-tier table on cheaper storage with a smaller vector. Search transparently spans hot and cold; cold-tier results are demoted in ranking.

- **Trigger**: hot-tier table exceeds 10 GiB on the canonical box and storage growth is faster than disk growth.
- **Estimate**: one week.
- **Risk**: medium. Cold-tier migrations are reversible but slow.

### Loki for logs

If a second daemon instance appears and `journalctl` across hosts becomes painful, add Loki as the log sink behind the existing journald collector.

- **Trigger**: more than one production daemon instance, and operator-reported log search pain.
- **Estimate**: three days for Loki itself plus existing journal-shipper config.
- **Risk**: low. Loki is well-trodden.

### Tracing turned on by default

OTLP exporter is in v1 but disabled. Default-on when a real performance-debug workflow exists that benefits from always-on sampling.

- **Trigger**: a tail-latency investigation needs traces and turning them on after the fact missed the window.
- **Estimate**: one day to flip the default plus tune sampling.
- **Risk**: low.

## v3 — six months and beyond

Items here are speculative. They are listed so that future thinking has a stable reference.

### Plugin system

Third-party extensions register via a small interface (a handful of hooks: pre-store, post-store, custom-search-rerank, custom-event-channel). Plugins are Go shared objects or Python subprocess workers. Useful if fabric becomes a platform rather than a tool.

### Federated tenants across organisations

Today multi-tenant means "multiple logical tenants in one daemon". A future model could allow two organisations to share specific memos via a read-only federation token. Compliance implications are non-trivial; this is a v3 conversation, not a v2 build.

### Active reranking via a learned model

Search reranks are currently rule-based: cosine similarity plus recency. A learned reranker trained on operator click-throughs could improve recall further. Worth doing only after the click-through dataset is large enough to train against (rough estimate: 50k logged clicks).

### Bidirectional sync with external systems

Linear, Notion, GitHub Issues, the operator's wikis. Memos and external items reference each other; updates flow both ways. Each integration is its own design exercise.

### Embedded mode

Run fabric as an embedded library inside another process rather than as a separate daemon. Useful for single-binary distribution of downstream tools. Adds significant API surface; only worth doing if a real consumer asks.

## What stays out indefinitely

These are decisions, not deferrals. Each has a one-line rationale.

- **Cloud-hosted SaaS fabric.** Wrong shape of product; the value is in local control.
- **Custom binary wire protocol.** HTTP and gRPC together cover the universe.
- **Custom auth provider beyond bearer + mTLS.** SSO integration is the caller's job; the daemon stays simple.
- **Mobile clients.** No realistic use case for fabric on a phone.
- **Browser-based MCP runtime.** MCP is a process-local protocol; bridging it to a browser is a different product.
- **Replacing Postgres with anything.** The schema, the LISTEN/NOTIFY, and the operator's familiarity with pg are load-bearing assets.
- **Replacing tree-sitter for graph extraction.** Mature, multi-language, well-supported. No reason to invent a parser.

## How proposals get added

A v2 or v3 candidate enters the roadmap when:

1. It has a one-paragraph problem statement (what does it solve).
2. It has a trigger condition (what would prove it is worth building).
3. It has an estimate within an order of magnitude.

Proposals without all three sit in a `notes/` folder until they grow into a complete entry. This keeps the roadmap honest: each item is something we would actually build if the trigger fired tomorrow.

## Cadence

The roadmap is reviewed once a quarter. The review answers three questions:

1. Did any v2 item's trigger fire? If yes, schedule it.
2. Did any item move from "good idea" to "obviously wrong"? If yes, move it to the "stays out" section with a one-line rationale.
3. Did any v3 item mature into a real candidate with all three roadmap fields filled in?

Quarterly is often enough to keep the document alive, infrequent enough to avoid roadmap-as-procrastination.

## Closing note

Fabric is small on purpose. The roadmap is a record of what we are deliberately not building yet, as much as it is a record of what we are. Items that never get built are not failures; they are choices that paid off in reduced surface area.

## See also

- [`01-architecture-overview.md`](01-architecture-overview.md) for what fabric is in its simplest form.
- [`06-shipping-plan.md`](06-shipping-plan.md) for what v1 looks like week by week.
- [`08-python-client.md`](08-python-client.md), [`09-wire-protocol.md`](09-wire-protocol.md), [`10-observability.md`](10-observability.md), [`11-error-model.md`](11-error-model.md) for the contracts v2 items must preserve.
