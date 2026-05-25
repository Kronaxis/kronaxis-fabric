<p align="center">
  <img src="assets/kronaxis-icon.svg" width="64" height="64" alt="Kronaxis">
</p>

<h1 align="center">Kronaxis Fabric</h1>

<p align="center">
  <strong>The memory layer your routed LLM stack is missing. One Go binary. Postgres + pgvector hybrid search. Sub-100&nbsp;ms recall over thousands of memos. The right context to send, decided in milliseconds, before <a href="https://github.com/kronaxis/kronaxis-router">Kronaxis Router</a> picks which model to send it to.</strong>
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
your agent ─POST /v1/memo/search─▶ Kronaxis Fabric ─▶ Postgres + pgvector + tsvector
   (Claude Code,                       │             ─▶ Ollama nomic-embed-text (768d)
    Codex, Aider,                      │             ─▶ recency rerank (30-day half-life)
    your own runtime)                  │
                                       ├─ hybrid rank in <100 ms p50 (LAN)
                                       ├─ sha256 dedup, soft delete, ON CONFLICT upsert
                                       ├─ MCP stdio shim (drops into ~/.claude.json natively)
                                       └─ pg_notify coord channel for cross-session events

your agent ─POST /v1/chat/completions─▶ Kronaxis Router ─▶ chooses cheapest competent model
                                                              (sovereign 7-9B / frontier / CLI agent)
```

**Router decides WHICH model. Fabric decides WHAT context to send.** Pair them and you stop paying frontier prices for context you didn't need to send to a model that didn't need to be that big.

## Why use it

**Stop preloading 100 KB of project notes into every session.** Every Claude Code / Codex / Aider session starts by loading `CLAUDE.md` + `MEMORY.md` + curated notes. That's 60–90 K tokens *per turn*, every turn, in every session. Fabric replaces it with on-demand hybrid search: only the 3 memos relevant to *this* turn get loaded, ranked semantically + lexically + by recency.

**Real measurement on a real project (this one)**: 715 → 172 line `CLAUDE.md` + 1050 → 51 line `MEMORY.md` after the fabric cutover. **~92 K tokens saved per session preload.** At Anthropic Opus rates that's ~£0.45 per session, ~£14/day for a 30-session day. Compounds fast.

**Better than grepping your own notes**:
- **Semantic search** — query "cellsocks IWLAN egress block" finds memos that say "Pixel 4 carrier-side data context blocked" without sharing a single word
- **Hybrid rank** — cosine (50%) + tsvector (30%) + recency (20%) so a fresh half-baked memo doesn't outrank a verified 3-week-old reference
- **Score-comparable across queries** — 0–1 cosine-based, not a per-query magic number

**Drops into your existing stack without rewiring**:
- MCP stdio shim → `~/.claude.json` config = native `mcp__fabric__search` tool in Claude Code
- Plain HTTP + Bearer = curl from anything else (shell scripts, CI, other agents)
- Postgres-backed = your DBA already knows how to back it up
- Embeddings via local Ollama = zero per-query cost, zero data leaving your box

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

| Dimension | **Fabric** | agentmemory | mem0 | Letta/MemGPT |
|---|---|---|---|---|
| **Stack** | 1 Go binary + Postgres + Ollama | Node + SQLite + iii-engine | Python + Qdrant/pgvector | Python + Postgres + vector DB |
| **Lines of code** | ~700 (single file) | thousands across hooks/UI | thousands | tens of thousands (full agent runtime) |
| **Search** | Hybrid (cosine + tsvector + recency) | BM25 + vector + graph (RRF) | Vector + graph | Vector only |
| **Multi-agent** | MCP stdio + plain HTTP (any agent) | MCP + REST + leases | API only | Letta runtime only |
| **External deps** | Postgres + Ollama (both standard) | None (SQLite) | Qdrant/pgvector | Postgres + vector DB |
| **Ops surface** | Standard Postgres backup/restore | Custom SQLite + iii engine state | Multi-service | Multi-service |
| **MCP wire-up** | `~/.claude.json` 5-line stanza | Same | Manual | Manual |
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

- **v0.3** — Per-tenant Bearer keys, RBAC scopes, audit log table
- **v0.4** — Native MCP stdio (no Python shim), WebSocket subscriptions on coord
- **v0.5** — Code graph integration (tree-sitter), inotify-driven indexing
- **v0.6** — Multi-host federation via pg_notify + replication slots
- **v0.7** — Router learning loop (record outcomes, train routing weights)
- **v1.0** — Multi-tenancy, SSO, signed release artifacts

## Licence

[BSL-1.1](LICENSE). Source-available; internal + non-commercial use granted. Production / commercial use requires a separate licence — contact `contact@kronaxis.co.uk`.

Same licence model as [Kronaxis Behavioural OS](https://github.com/kronaxis/kronaxis-behavioural-os) and [Kronaxis Router](https://github.com/kronaxis/kronaxis-router).
