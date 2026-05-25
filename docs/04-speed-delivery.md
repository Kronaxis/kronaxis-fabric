# 04 — Speed of delivery

Two angles: **engineering velocity** (how fast does fabric ship) and **operational latency** (how fast does fabric answer).

## Engineering velocity

### Why fabric ships fast

Fabric is unusually fast to build because nearly every dependency already exists:

| Dependency | Status | Why this matters |
|---|---|---|
| Postgres on DL580 | Running, healthy | No new DB infrastructure |
| pgvector extension | Already installed (operator's notes confirm) | No vector store engineering |
| Ollama | Running on DL580 | No embedding service to deploy |
| WireGuard mesh (laptop/DL580/R920) | Established | No network engineering |
| ~/.kronaxis/env credential pattern | Established | No secret management to design |
| BSL-1.1 licence (BoS precedent) | Operator-confirmed | No legal back-and-forth |
| Existing nightly backups | Running | No backup chain to set up |
| Existing systemd ops patterns | Established | No new ops model |

Fabric is fundamentally a **policy + glue** project, not new infrastructure. That's why six weeks is realistic.

### Six-week shipping plan

| Week | Deliverable | LoC estimate | Replaces |
|---|---|---|---|
| **1** | memory MCP + HTTP + ollama embed + single-host | ~2,500 | agentmemory daemon |
| **2** | federation (3-host, pg_notify, local cache) | ~1,500 | chan.log bridge |
| **3** | live code graph (tree-sitter, inotify) | ~3,000 | graphify cron |
| **4** | CI integration (GH webhook + journald + git hook + coord parser) | ~2,000 | net new |
| **5** | orchestrator (session registry, capabilities, task graph, dispatch) | ~2,500 | hand-coordinated MAIN→A-G |
| **6** | persona absorption + router integration | ~2,000 | manual routing rules |
| **Total** | | **~13,500 LoC** (Go) | 5 Frankenstein parts replaced |

**Each week independently usable**. Operator can pause at any week and have working tools.

### Weekly demo cadence

Friday of each week: 30-min demo for operator showing the week's deliverable working end-to-end.

| Week | Demo |
|---|---|
| 1 | `mcp__fabric__search "Lucy port"` returns answer in <100ms with confidence + source |
| 2 | Coord message sent from DL580 arrives in laptop fabric daemon within 50ms |
| 3 | Edit a file → 5s later, memos referencing that symbol show `[STALE]` tag |
| 4 | Deploy something to DL580 → memo auto-emitted, visible in incident replay |
| 5 | MAIN files a task with `required_capabilities=['cycles_render']` → orchestrator dispatches to DL580 fabric, session B accepts |
| 6 | Mary calls Pearl Insurance → fabric retrieves persona memory pre-call → router picks slatewick-lora → outcome recorded → next call has full context |

### Build accelerators

**1. Server-side claude on DL580**

Claude Code is installed on DL580 (per the operator's `project_serverside_claude_proven_2026_05_21.md` memory). Heavy code-gen can dispatch to DL580 sessions, freeing operator's laptop. The fabric build itself can use this — fabric building fabric.

**2. Qwen-local for code review**

slatewick-lora is on GPU 2; Qwen-32B is available. Code reviews + boilerplate generation can route to local instead of Claude API. Drops API spend by 50-70% during build.

**3. Existing Go expertise**

The operator's BoS codebase is Go-native. behavioural-os/ already uses pgx, systemd patterns, similar layered architecture. Fabric inherits these patterns. No learning curve for Go conventions.

**4. Tree-sitter Go bindings**

`github.com/smacker/go-tree-sitter` is production-grade. Grammars for Go/Python/Bash/Markdown/YAML/TS are stable. Week 3 (the highest-risk week) has tested tooling.

### Engineering risks that COULD slow shipping

| Risk | Probability | Mitigation |
|---|---|---|
| pgvector hnsw index issues at scale | Low | Verified working at agentmemory's 600+ memos; fabric won't exceed 100k in year 1 |
| Embedding dimension mismatch (384 vs 1024 in soul_memory) | Medium | Decide before week 1; recommend 384 for fabric, write soul_memory adapter to use either |
| Tree-sitter grammar bugs | Low | Mature grammars; fallback to regex for parser edge cases |
| MCP protocol changes | Low | Wrap with internal API; MCP layer is thin |
| Federation correctness under network partition | Medium | Designed for it; test in week 2 explicitly with simulated WireGuard drop |

## Operational latency

### Hot path latencies (measured against today's stack)

| Operation | Today | Fabric (cache hit) | Fabric (cache miss) |
|---|---|---|---|
| Single memo lookup | ~200ms (agentmemory HTTP) | ~1ms | ~12ms |
| Semantic search top-K | ~200ms | ~1ms | ~15ms |
| Symbol lookup | ~50ms (grep across repo) | ~1ms | ~7ms |
| Coord send | ~10ms (PG insert + bash overhead) | ~5ms | ~5ms |
| Coord receive | ~50ms (file tail polling) | ~1ms (push) | ~1ms |
| Session register | ~5s (bash + PG) | ~10ms | ~10ms |
| Task assignment | manual (operator types) | ~10ms (orchestrator decides) | ~10ms |
| Routing decision | static config lookup | ~5ms (data-backed) | ~10ms |

### Cold path (e.g. fresh daemon, no cache)

First search after restart:
- Embed query: 50-100ms (Ollama HTTP, first call after Ollama itself idle)
- Vector search: 15ms
- Result formatting: 1ms
- **Total: ~70-120ms**

Subsequent searches: cached embedding for repeat queries, plus result caching, drop to ~10ms.

### Concurrent throughput

Single fabric daemon should comfortably handle:
- 100 search QPS (limited by pgvector + ollama embedding pipeline)
- 50 write QPS (limited by PG insert + hnsw index update)
- 1000 cached read QPS (in-memory)
- 200 coord events/min

These are 10-100× what current Frankenstein handles.

### Operator-perceived speed wins

**Today**: Operator asks Claude "what's the state of WhatsApp track?" Claude reads chan.log (5k tokens), reads relevant memos (10k tokens), summarises (3k tokens). 30 seconds wall clock.

**Fabric**: Operator asks the same. Claude calls `mcp__fabric__search "WhatsApp track state"` (200 tokens result). Claude formats answer (500 tokens output). 3 seconds wall clock.

**10× faster operator iteration on questions.**

## Speed of delivery TO router

How quickly can fabric serve router's routing-decision queries?

```
Router incoming request → router classifies task → 
GET /v1/routing/best_backend?task_class=email_compose_sales → 
fabric: cache hit? Yes (frequent class) → return in <2ms → 
router dispatches to chosen backend
```

**Routing decision latency: <2ms** (cache hit) to **<10ms** (cache miss). This is fast enough that fabric is in the request path of every routed call without adding noticeable user-perceived latency.

Outcome recording is async:

```
Backend returns → router fires outcome recording in background goroutine → 
HTTP POST /v1/routing/outcome → fabric writes to fabric.task_outcomes → 
done
```

**Outcome recording adds 0ms** to user-perceived latency (async, doesn't block response).

## How fabric stays fast as data grows

| Data class | Growth rate | Index strategy |
|---|---|---|
| Memos | ~100/day operator + ~500/day persona = ~600/day = ~220k/year | pgvector hnsw + B-tree on tenant_id, session_letter, type, created_at |
| Events | ~200/day = ~75k/year | B-tree on ts, sender, recipient |
| Symbols | ~50k initial scan, +500/day on active dev = ~200k by year 2 | pgvector hnsw + B-tree on (file_path, fully_qualified) |
| Symbol edges | ~5× symbols = ~1M | B-tree on src_id, dst_id, edge_type |
| Tasks | ~50/day = ~20k/year | B-tree on status, assigned_to, created_at |
| Task outcomes | ~1000/day (every routed call) = ~365k/year | B-tree on (task_class, backend, ts) — heavy partition candidate by ts |

**Year-2 scale estimate**: ~500MB of fabric data + ~2GB of embeddings + ~1GB of indexes = ~4GB total. Trivial for Postgres + DL580.

**Year-5 scale**: ~10GB. Still trivial. No sharding needed.

## See also

- [`02-token-efficiency.md`](02-token-efficiency.md) — Per-query token costs
- [`05-router-maximisation.md`](05-router-maximisation.md) — Router integration latency
- [`06-shipping-plan.md`](06-shipping-plan.md) — Weekly milestones
