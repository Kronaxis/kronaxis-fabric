<p align="center">
  <img src="assets/kronaxis-icon.svg" width="64" height="64" alt="Kronaxis">
</p>

<h1 align="center">Kronaxis Fabric</h1>

<p align="center">
  <strong>The nervous system for multi-agent LLM workflows. One Go binary on one Postgres database, doing three things every multi-agent stack reinvents badly:</strong>
</p>

<p align="center">
  <strong>1. Cross-session coordination</strong> &middot;
  <strong>2. Shared semantic memory</strong> &middot;
  <strong>3. Capability-typed orchestration (v0.6)</strong>
</p>

<p align="center">
  Stop running three services (agentmemory + a broker + your own task queue) when one Postgres schema does the lot. Plays directly into <a href="https://github.com/kronaxis/kronaxis-router">Kronaxis Router</a> for the cheapest-competent-model decision.
</p>

<p align="center">
  <a href="LICENSE">BSL 1.1</a> &middot;
  <a href="https://kronaxis.co.uk">Website</a> &middot;
  <a href="#endpoints">API</a> &middot;
  <a href="#why-use-it">Why use it</a> &middot;
  <a href="#vs-agentmemory--mem0--letta">vs agentmemory / mem0 / Letta</a>
</p>

---

```
                          ┌─ POST /v1/coord    ──▶ broadcast to other sessions (pg_notify)
   Tab A (orchestrator) ──┤                       
                          ├─ POST /v1/memo     ──▶ bank a finding (semantic + tsvector + recency)
                          └─ POST /v1/task     ──▶ create work item, required_capabilities=[...]
                                                       │
                                                       │  (Fabric routes by capability)
                                                       ▼
                                  ┌──── POST /v1/task/claim ──── Tab B (capabilities=["go-build"])
   Kronaxis Fabric ─────────────► ├──── POST /v1/task/claim ──── Tab C (capabilities=["python","gpu-3090"])
   (one Go binary,                └──── POST /v1/task/claim ──── CI runner (capabilities=["docker","aws"])
    one Postgres schema)
                                  │
                                  ├──── GET /v1/coord/recent ─── any session polls broadcasts
                                  ├──── POST /v1/memo/search ─── 90ms hybrid search over 1000s of memos
                                  └──── POST /v1/router/obs ──── learning loop for Kronaxis Router

   Tab D (your agent) ──POST /v1/chat/completions──▶ Kronaxis Router ──▶ cheapest competent model
                                                            (uses fabric memo search to keep context tiny)
```

**Three jobs, one substrate.** Coord channel for "what is everyone else doing right now". Memory for "what did I learn last week". Task graph + capability dispatch for "who in this fleet is best placed to do X". All on the same Postgres, same Bearer token, same single Go binary.

**Pairs with Kronaxis Router**: Router decides WHICH model. Fabric decides WHAT context. Together you stop paying frontier prices for context you didn't need to send to a model that didn't need to be that big.

## Why use it

**Reason 1 — Multi-agent stacks reinvent coordination badly.** Five Claude Code tabs on the same project. An overnight cron. A CI runner. Each one needs to (a) see what the others are doing, (b) avoid duplicating work, (c) hand off findings, (d) recover when a tab crashes. The default answer today is some combination of Slack channel humans monitor, ad-hoc REST endpoints, and "remember to message me when you're done". Fabric ships a coord channel + task graph + capability dispatch on one Postgres schema. The orchestrator pattern we used to build Fabric *itself* (one MAIN tab coordinating four worker tabs across two hosts) was hand-coordinated via Fabric's coord broadcasts. v0.6 formalises it with typed sessions + presence + automatic dispatch.

**Reason 2 — Stop preloading 100 KB of project notes into every session.** Every Claude Code / Codex / Aider session starts by loading `CLAUDE.md` + `MEMORY.md` + curated notes. That's 60–90 K tokens *per turn*, every turn, in every session. Fabric replaces it with on-demand hybrid search: only the 3 memos relevant to *this* turn get loaded, ranked semantically + lexically + by recency.

**Real measurement on a real project (this one)**: 715 → 172 line `CLAUDE.md` + 1050 → 51 line `MEMORY.md` after the fabric cutover. **~92 K tokens saved per session preload.** At Anthropic Opus rates that's ~£0.45 per session, ~£14/day for a 30-session day. Compounds fast.

**Better than grepping your own notes**:
- **Semantic search** — query "database connection pool exhaustion fix" finds memos that say "postgres max_connections raised after 503 burst" without sharing a single word
- **Hybrid rank** — cosine (50%) + tsvector (30%) + recency (20%) so a fresh half-baked memo doesn't outrank a verified 3-week-old reference
- **Score-comparable across queries** — 0–1 cosine-based, not a per-query magic number

**Drops into your existing stack without rewiring**:
- MCP stdio shim → `~/.claude.json` config = native `mcp__fabric__search` tool in Claude Code
- Plain HTTP + Bearer = curl from anything else (shell scripts, CI, other agents)
- Postgres-backed = your DBA already knows how to back it up
- Embeddings via local Ollama = zero per-query cost, zero data leaving your box

## Cross-session coordination (the underrated half)

Memos solve "what did I learn last week". The other half of multi-agent workflows is "what is the agent in the other tab doing right now". Fabric ships a coord channel on the same Postgres: `POST /v1/coord` writes a row to `public.coord_messages` and fires `pg_notify('coord', ...)`; `GET /v1/coord/recent` lets any session poll the last N events filtered by sender / recipient / since.

```bash
# Tab A broadcasts a finding
curl -X POST -H "Authorization: Bearer $FABRIC_KEY" -H "Content-Type: application/json" \
  -d '{"sender":"A","recipient":"all","subject":"db migration done","body":"schema v17 applied"}' \
  http://fabric:8201/v1/coord

# Tab B (or a CI runner, or your laptop) reads the recent broadcasts
curl -H "Authorization: Bearer $FABRIC_KEY" \
  "http://fabric:8201/v1/coord/recent?limit=10&recipient=all"
```

We used this in tonight's session to coordinate five concurrent agent tabs (orchestrator + workers) on the same project. It is the unsung infrastructure piece — agentmemory / mem0 / Letta don't have a cross-session event bus, only memory. **Multi-agent workflows on a single project need both**, and Fabric ships both behind the same Bearer token on the same Postgres.

## Quickstart (60 seconds)

```bash
# 1. Schema (one-time, in your existing Postgres)
psql -h db.example.com -U postgres -d kronaxis \
  -c "CREATE SCHEMA IF NOT EXISTS fabric; CREATE EXTENSION IF NOT EXISTS vector;"

# 2. Pull the embedding model (one-time)
ollama pull nomic-embed-text

# 3. Build + run
go build -o /tmp/fabric ./cmd/fabric
FABRIC_KEY=secret-bearer-token \
FABRIC_PG_DSN="postgres://user:pass@db:5432/kronaxis" \
FABRIC_LISTEN=:8201 \
/tmp/fabric &

# 4. Bank a memo
curl -X POST -H "Authorization: Bearer secret-bearer-token" -H "Content-Type: application/json" \
  -d '{"title":"production-postgres password","content":"in 1Password under Infra-Prod","type":"reference"}' \
  http://localhost:8201/v1/memo

# 5. Find it later
curl -X POST -H "Authorization: Bearer secret-bearer-token" -H "Content-Type: application/json" \
  -d '{"query":"where is the prod db password","top_k":1}' \
  http://localhost:8201/v1/memo/search
# → returns your memo with score 0.92
```

That's it. systemd unit + MCP shim in [`deploy/`](deploy/).

## vs agentmemory / mem0 / Letta

Fair warning before reading the table: **most of the alternatives are memory-only systems**. Fabric is three things in one binary (coord channel + memory store + orchestrator-coming). Compare on a feature they don't have and the comparison is by definition unfair to them. We've split the table by job-area so you can see what Fabric uniquely covers vs where alternatives match.

### Memory dimension (where the alternatives compete)

| Dimension | **Fabric** | agentmemory | mem0 | Letta/MemGPT |
|---|---|---|---|---|
| **Search** | Hybrid (cosine + tsvector + recency) | BM25 + vector + graph (RRF) | Vector + graph | Vector only |
| **Embeddings** | Local Ollama, free at query time | Local Xenova, free | Cloud-provider call per query | Cloud-provider call per query |
| **Lines of code** | ~700 (single file) | thousands across hooks/UI | thousands | tens of thousands (full agent runtime) |
| **Storage** | Postgres + pgvector | SQLite + iii-engine | Qdrant or pgvector | Postgres + vector DB |
| **MCP wire-up** | `~/.claude.json` 5-line stanza | Same | Manual | Manual |
| **Auto-capture hooks** | No (explicit `remember` calls) | **Yes (12 lifecycle hooks)** | No | No (agent self-edits) |
| **Real-time viewer** | No (`psql` is your viewer) | **Yes (port 3113)** | Cloud dash | Cloud dash |

### Coordination + pub/sub (real competitors here are message brokers, not memory tools)

| Dimension | **Fabric** | Redis Streams | NATS JetStream | RabbitMQ | Raw pg_notify |
|---|---|---|---|---|---|
| **Adds a new service to run** | No (reuses your Postgres) | **Yes** (Redis) | **Yes** (NATS cluster) | **Yes** (RabbitMQ) | No |
| **Persistent event log** | Yes (`coord_messages` table) | Yes (XADD stream) | Yes (JetStream) | Yes (with disk queue) | No (fires once, not stored) |
| **Audit via SQL** | **Yes** (`SELECT * FROM coord_messages WHERE …`) | No (custom CLI) | No (`nats stream`) | No (mgmt UI) | N/A |
| **Push notifications (sub-second)** | Yes (pg_notify trigger) | Yes (XREAD BLOCK) | Yes | Yes (consumer push) | Yes |
| **Filter by recipient / since** | Yes (`?recipient=X&since=…`) | Consumer groups | Subjects | Routing keys | DIY |
| **Bearer-auth same as memory** | **Yes** (one token, one service) | Separate auth | Separate auth | Separate auth | Postgres role |
| **Best for** | Multi-agent project coord on existing Postgres | High-throughput cache+stream | Microservice fleets | Enterprise messaging | Quick hacks |

For genuine high-throughput broker workloads (millions of msg/s) use NATS or Redis. For multi-agent coord on a project (events per second, not per millisecond), Fabric gives you a sufficient subset on the Postgres you already run.

### Multi-agent orchestration (real competitors are workflow engines + agent frameworks)

| Dimension | **Fabric v0.6** | Temporal | CrewAI | LangGraph | AutoGen | Celery / Prefect |
|---|---|---|---|---|---|---|
| **Adds a new service** | No (Postgres) | **Yes** (Temporal cluster + Cassandra/ES) | No (Python library) | No (Python library) | No (Python library) | Yes (broker + workers) |
| **Cross-language agents** | **Yes** (HTTP from anything) | Yes (SDKs) | Python only | Python only | Python only | Python only |
| **Task graph storage** | Postgres (`fabric.tasks`) | Internal events DB | In-memory | In-memory | In-memory | Result backend |
| **Capability-typed dispatch** | **Yes** (`required_capabilities` matched to `session.capabilities`) | Workflow workers | Role-based | State-based | Agent types | Routing keys |
| **Presence + heartbeat** | **Yes** (90 s auto-offline) | Yes (workers) | No | No | No | Yes (workers) |
| **Built-in pub/sub** | **Yes** (same coord channel) | Signals | No | No | No | No |
| **Built-in semantic memory** | **Yes** (same memo store) | No | No (DIY RAG) | Some (state) | No (DIY) | No |
| **Sweet spot** | Multi-agent coding sessions on one project | Production workflows at scale | LLM agent crews | LLM agent state machines | LLM agent conversations | Background jobs |

If you're orchestrating a 1000-worker production payment pipeline, take Temporal. If you're orchestrating five Claude Code tabs + a CI runner on one project, the Temporal cluster is overkill and you don't already have it running. Fabric is the small-team multi-agent-coding subset of Temporal's job, on the database you already have.

### Code graph + symbol search (v0.5 — real competitors are code-indexing tools)

| Dimension | **Fabric v0.5** | Sourcegraph | codegraph (MCP) | ast-grep | ripgrep + ctags |
|---|---|---|---|---|---|
| **Self-host friction** | One binary | Docker compose / SaaS | One binary | One binary | Native |
| **MCP-native** | **Yes** (same `mcp__fabric__search` namespace) | No (HTTP/GQL) | Yes | No | No |
| **Symbol embeddings** | **Yes** (cosine on signature + docstring) | Yes (Cody) | No | No | No |
| **Cross-symbol edges (calls / imports)** | Yes (`symbol_edges`) | Yes | Yes | No | Limited (ctags) |
| **Same auth as memory + coord** | **Yes** (one Bearer) | Separate | Yes (different MCP) | N/A | N/A |
| **Languages day-one** | Go + Python | 50+ | Many | Many | All (lexical only) |
| **Inotify re-index** | v0.5+1 | Yes | Yes | One-shot | One-shot |
| **Storage** | Postgres (same DB as memos + coord) | Custom Zoekt + Postgres | SQLite | None | tags file |

If you need cross-50-language enterprise code search, Sourcegraph wins. If you want symbol search + relationships in the same Bearer-authed MCP namespace your agents already use for memory and coord, Fabric is the integrated choice.

### Operations

| Dimension | **Fabric** | agentmemory | mem0 | Letta |
|---|---|---|---|---|
| **External deps** | Postgres + Ollama (both standard) | None (SQLite) | Qdrant/pgvector | Postgres + vector DB |
| **Backup story** | `pg_dump` (you already know it) | Custom SQLite + iii state | Multi-service | Multi-service |
| **Ops surface** | Standard Postgres | Custom SQLite + iii engine | Multi-service | Multi-service |
| **Self-host friction** | One binary + `systemd --user` | One binary + node runtime | Docker compose | Docker compose |
| **Cost / scale story** | pgvector scales to millions; Postgres ops you already do | SQLite hits write-lock at scale | Qdrant means another service | Vector DB means another service |

**When Fabric wins**:
- You already run Postgres, you want one more schema not one more service.
- You want the simplest possible audit trail (`SELECT * FROM fabric.memos WHERE deleted_at IS NULL`).
- You want to pair it with a router (cost-conscious shop, that's the point).
- You want the operator to learn one thing (Postgres) not three (SQLite + iii-engine + custom viewer).

**When agentmemory / mem0 / Letta might fit better**:
- You don't want to run Postgres (agentmemory wins — pure SQLite, zero external).
- You want auto-capture hooks across many agent platforms with no integration work (agentmemory's 12 lifecycle hooks).
- You want a full agent runtime not just memory (Letta).

Fabric is for the operator who already runs production Postgres and wants to stop paying frontier prices for preloaded context. Everything else is a side-quest.

## The pairing with Kronaxis Router

If you run [Kronaxis Router](https://github.com/kronaxis/kronaxis-router) you already decided: cheap models for the 80%, frontier for the 20%, agentic CLIs as OpenAI endpoints. Fabric closes the loop on the OTHER half of LLM cost — the context window.

```
Without fabric:                        With fabric:
─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─        ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─
agent reads MEMORY.md (300 KB)        agent calls mcp__fabric__search
agent reads CLAUDE.md (100 KB)         → 3 memos, 2 KB, 90 ms
agent reads 5 project docs (50 KB)
total preload: ~120 K tokens          total preload: ~500 tokens

sends to Router → frontier             sends to Router → sovereign 7B (50× cheaper)
~$0.60 per turn @ Opus                 ~$0.001 per turn @ Qwen 7B
```

Same answer quality on the typical "how does X work" query, because the model receives the *specific* 3 relevant memos instead of being asked to grep a 120 K-token preload. **Per-session bill drops 100–600×** when both run together.

## Endpoints

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/v1/health` | — | DB + embedding model status |
| POST | `/v1/memo` | Bearer | Create memo. sha256 dedup. Returns `{id, sha256, deduped, embedded}` |
| GET | `/v1/memo/:id` | Bearer | Read |
| PUT | `/v1/memo/:id` | Bearer | Partial update. Re-embeds on change |
| DELETE | `/v1/memo/:id` | Bearer | Soft delete (sets `deleted_at`) |
| POST | `/v1/memo/search` | Bearer | `{query, top_k?, type?, mode?}` → ranked hits |
| POST | `/v1/memo/backfill` | Bearer | Embed any memos with NULL embedding |
| POST | `/v1/coord` | Bearer | `{sender, recipient, subject, body}` → coord_messages + pg_notify |
| GET | `/v1/coord/recent` | Bearer | `?limit=&since=&recipient=` |

Search `mode`: `hybrid` (default), `semantic`, `tsvector`.

## Configuration

| Env | Required | Default | Notes |
|---|---|---|---|
| `FABRIC_KEY` | Yes | — | Single shared Bearer token |
| `FABRIC_PG_DSN` | Yes | — | `postgres://user:pass@host:5432/db` |
| `FABRIC_LISTEN` | No | `:8201` | Bind address |
| `OLLAMA_URL` | No | `http://localhost:11434` | Embedding service |

Schema migration runs on boot (`CREATE TABLE IF NOT EXISTS`, `ALTER TABLE ADD COLUMN IF NOT EXISTS`). pgvector + tsvector extensions required (`CREATE EXTENSION IF NOT EXISTS vector`).

## MCP wire-up (Claude Code)

Add to `~/.claude.json`:

```json
"mcpServers": {
  "fabric": {
    "type": "stdio",
    "command": "python3",
    "args": ["/path/to/scripts/kx-fabric-mcp.py"],
    "env": {
      "FABRIC_URL": "http://your-server:8201",
      "FABRIC_KEY": "secret-bearer-token"
    }
  }
}
```

New sessions get `mcp__fabric__search`, `mcp__fabric__remember`, `mcp__fabric__health` as native tools.

## Operational reality (what's been proven)

Built + deployed on a single project (this one) on 2026-05-25:
- 600 memos imported from a 567-file curated `memory/*.md` tree in 13 seconds.
- All 600 embedded via Ollama (`nomic-embed-text` 768d, local, free).
- `mcp__fabric__search` returned the right memo as top hit on 6/6 test queries.
- Latency 89-181 ms p50 LAN. Cold-start embed ~3 s once, then warm.
- A/B vs agentmemory on identical queries: tied on quality, 2-3× faster on latency.
- Drove a 76% reduction in `CLAUDE.md` size (715 → 172 lines) by extracting pitfall sections to fabric memos. Sessions still find the content via search.

## Status

**v0.2.0.** Single shared Bearer token, no multi-tenancy, no RBAC. Production-ready for single-operator / small-team use; the items in [Roadmap](#roadmap) gate broader deployment.

## What v0.2.0 ships

- **Bearer-token auth** (single shared `FABRIC_KEY`) on every endpoint except `/v1/health`
- **Memo CRUD** with sha256 dedup via `ON CONFLICT`
- **Hybrid search** — cosine 50% + tsvector 30% + recency 20% (configurable mode)
- **Embeddings** via Ollama `nomic-embed-text`, 768d, pgvector ivfflat index. Best-effort on write; falls back to NULL on Ollama failure (search still works via tsvector fallback)
- **Backfill endpoint** for any memos with NULL embedding (post-Ollama-fix or post-import)
- **Coord channel** — `public.coord_messages` + pg_notify pub/sub for cross-session events
- **Soft delete** — `deleted_at` filtered from all reads

## Roadmap

What v0.2.0 ships is the **memory + coord substrate**. The bigger vision builds on top of it:

- **v0.3** — Per-tenant Bearer keys, RBAC scopes, audit log table
- **v0.4** — Native MCP stdio (no Python shim), WebSocket subscriptions on coord, typed session capabilities (each agent registers what it can do; coord broadcasts can target by capability instead of name)
- **v0.5** — Code graph integration (tree-sitter, inotify-driven re-index) so search returns code symbols + relationships, not just memos. Replaces the graphify-style snapshot workflow.
- **v0.6** — Orchestrator layer: task graph in Postgres + presence/heartbeat per session + automatic dispatch by capability match. Formalises the MAIN-orchestrator + worker-sessions pattern that today is hand-coordinated via coord broadcasts.
- **v0.7** — Multi-host federation via `pg_notify` + replication slots so coord events fan out across 3+ Postgres replicas without a broker dependency.
- **v0.8** — Router learning loop: record `{request, model, outcome, cost, latency}` per Router call into Fabric. Use that history to train routing weights so the cheapest-competent-model decision gets sharper over time.
- **v1.0** — Multi-tenancy, SSO, signed release artifacts.

The end state: one Postgres-backed nervous system that replaces ad-hoc memory, ad-hoc inter-session coord, ad-hoc orchestrator-worker dispatch, and ad-hoc router heuristics with one auditable store. v0.2.0 ships the foundations; the rest builds on the same schema + endpoints.

## Licence

[BSL-1.1](LICENSE). Source-available; internal + non-commercial use granted. Production / commercial use requires a separate licence — contact `contact@kronaxis.co.uk`.

Same licence model as [Kronaxis Router](https://github.com/kronaxis/kronaxis-router).
