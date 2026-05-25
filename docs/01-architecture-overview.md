# 01 — Architecture overview

## Premise

One Go binary. One Postgres schema in the existing `tfs` DB (no new DB instance). One systemd unit per host. Two transport layers (HTTP REST + MCP). Storage backed by `pgvector` (already running). Embeddings via Ollama (already running). Coord via PG LISTEN/NOTIFY (already running).

Everything that's new is **glue + policy**, not new infrastructure.

## Top-level shape

```
┌────────────────────────────────────────────────────────────────────┐
│                          kronaxis-fabric                           │
│                          (single Go binary)                        │
│                                                                    │
│  ┌────────────┐  ┌────────────┐  ┌────────────┐  ┌──────────────┐  │
│  │   memos    │  │   graph    │  │   coord    │  │ orchestrator │  │
│  │  (search,  │  │ (symbols,  │  │ (events,   │  │  (sessions,  │  │
│  │ remember,  │  │   edges,   │  │  send,     │  │   tasks,     │  │
│  │  verify)   │  │  query)    │  │  history)  │  │  dispatch)   │  │
│  └────────────┘  └────────────┘  └────────────┘  └──────────────┘  │
│        │              │                │                │          │
│        └──────────────┴────────────────┴────────────────┘          │
│                              │                                     │
│                              ▼                                     │
│                ┌──────────────────────────┐                        │
│                │   store/* (pgx via pool) │                        │
│                └────────────┬─────────────┘                        │
│                             │                                      │
│                             ▼                                      │
│  ┌────────────┐    ┌────────────────┐    ┌────────────────┐        │
│  │ embed/     │    │  api/          │    │ ci/            │        │
│  │ (ollama,   │    │  (http + mcp + │    │ (gh-webhook,   │        │
│  │  onnx)     │    │   auth)        │    │  journald,     │        │
│  └────────────┘    └────────────────┘    │  git-hook)     │        │
│                                          └────────────────┘        │
│                                                                    │
│                 ┌─────────────────────────────┐                    │
│                 │  router_integration/        │                    │
│                 │  (best-backend + outcome)   │                    │
│                 └─────────────────────────────┘                    │
└────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
                ┌─────────────────────────────┐
                │   Postgres (tfs.fabric.*)   │
                │                             │
                │   memos / symbols / events  │
                │   sessions / hosts / tasks  │
                │   task_outcomes / links     │
                └─────────────────────────────┘
                              │
              ┌───────────────┼───────────────┐
              │               │               │
        ┌─────▼─────┐   ┌─────▼─────┐   ┌─────▼─────┐
        │ DL580     │   │ Laptop    │   │ R920      │
        │ fabric    │   │ fabric    │   │ fabric    │
        │ daemon    │   │ daemon    │   │ daemon    │
        └───────────┘   └───────────┘   └───────────┘
                       (read-through cache, pg_notify subscribers)
```

## Repository layout

```
kronaxis-fabric/
├── cmd/fabric/main.go              entry, signal handling, config
├── internal/
│   ├── config/                     TOML + env override
│   ├── store/                      pgx-based access
│   │   ├── memos.go
│   │   ├── symbols.go
│   │   ├── events.go
│   │   ├── sessions.go
│   │   ├── hosts.go
│   │   ├── tasks.go
│   │   └── task_outcomes.go
│   ├── embed/                      Embedder interface + backends
│   │   ├── interface.go
│   │   ├── ollama.go               primary (HTTP to local ollama)
│   │   └── onnx.go                 backup (in-process, deferred)
│   ├── search/                     query layer
│   │   ├── semantic.go             pgvector cosine
│   │   ├── hybrid.go               semantic + recency + scope
│   │   └── rerank.go               belief-decay reranking
│   ├── graph/                      live code graph (week 3)
│   │   ├── extract.go              tree-sitter parsing
│   │   ├── incremental.go          inotify-driven updates
│   │   └── query.go                subgraph + traversal
│   ├── coord/                      coord channel (compat with tfs.coord_messages)
│   │   ├── pg_listen.go
│   │   ├── pg_send.go
│   │   └── compat.go               bridge during migration
│   ├── orchestrator/               (week 4-5)
│   │   ├── registry.go
│   │   ├── dispatch.go
│   │   ├── status.go
│   │   └── escalation.go
│   ├── ci/                         (week 4)
│   │   ├── github_webhook.go
│   │   ├── systemd_watcher.go
│   │   ├── git_hook.go
│   │   └── memo_emit.go
│   ├── router_integration/         (week 6)
│   │   ├── outcome_record.go
│   │   ├── routing_policy.go
│   │   └── classifier.go
│   └── api/
│       ├── http_server.go          REST
│       ├── mcp_server.go           MCP over stdio/SSE
│       ├── auth.go                 API key + bearer
│       └── handlers/               per-endpoint
├── pkg/sdk/                        Go client SDK (for other services)
├── deploy/
│   ├── systemd/kronaxis-fabric.service
│   ├── examples/config.toml
│   └── migrations/                 PG schema versions
└── docs/
```

## Postgres schema (single source of truth)

All in `tfs.fabric.*`. Reuses existing DB, existing backup chain.

```sql
CREATE SCHEMA fabric;

-- Core memory store (the operator's + personas' belief system)
CREATE TABLE fabric.memos (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id TEXT NOT NULL,             -- 'kronaxis' for ops; per-customer when multi-tenant lands
  session_letter TEXT,                 -- A/B/C/D/E/MAIN — null = operator-banked
  visibility TEXT NOT NULL DEFAULT 'private',  -- private | shared | global
  type TEXT NOT NULL,                  -- project | feedback | user | reference | deployment | revenue_event | persona_interaction
  title TEXT NOT NULL,
  body TEXT NOT NULL,
  body_sha256 BYTEA NOT NULL,          -- native dedup
  embedding vector(384),               -- MiniLM-L6 dim (Ollama: nomic-embed-text or all-minilm)
  confidence REAL DEFAULT 0.5,         -- 0.0-1.0
  importance REAL DEFAULT 0.5,         -- 0.0-1.0
  staleness_flag BOOLEAN DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  accessed_at TIMESTAMPTZ DEFAULT now(),
  ttl_at TIMESTAMPTZ,                  -- optional decay
  metadata JSONB DEFAULT '{}',
  UNIQUE (tenant_id, body_sha256)
);
CREATE INDEX memos_embed_hnsw ON fabric.memos USING hnsw (embedding vector_cosine_ops);
CREATE INDEX memos_tenant_session ON fabric.memos (tenant_id, session_letter);
CREATE INDEX memos_type ON fabric.memos (type);
CREATE INDEX memos_recency ON fabric.memos (created_at DESC);

-- Memo wikilinks (graph between memos)
CREATE TABLE fabric.memo_links (
  src_id UUID REFERENCES fabric.memos(id) ON DELETE CASCADE,
  dst_id UUID REFERENCES fabric.memos(id) ON DELETE CASCADE,
  link_type TEXT,                      -- ref | alias | supersedes | implements
  PRIMARY KEY (src_id, dst_id)
);

-- Live code graph (replaces graphify)
CREATE TABLE fabric.symbols (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id TEXT NOT NULL,
  language TEXT,
  file_path TEXT NOT NULL,
  kind TEXT,                           -- function | type | const | module | file
  name TEXT,
  fully_qualified TEXT,
  body TEXT,
  body_sha256 BYTEA,
  start_line INT,
  end_line INT,
  embedding vector(384),
  last_modified_at TIMESTAMPTZ,
  UNIQUE (tenant_id, file_path, fully_qualified)
);
CREATE INDEX symbols_embed_hnsw ON fabric.symbols USING hnsw (embedding vector_cosine_ops);
CREATE INDEX symbols_name ON fabric.symbols (name);

CREATE TABLE fabric.symbol_edges (
  src_id UUID REFERENCES fabric.symbols(id) ON DELETE CASCADE,
  dst_id UUID REFERENCES fabric.symbols(id) ON DELETE CASCADE,
  edge_type TEXT NOT NULL,             -- imports | calls | defines | references | implements
  metadata JSONB DEFAULT '{}',
  PRIMARY KEY (src_id, dst_id, edge_type)
);

CREATE TABLE fabric.memo_symbol_refs (
  memo_id UUID REFERENCES fabric.memos(id) ON DELETE CASCADE,
  symbol_id UUID REFERENCES fabric.symbols(id) ON DELETE CASCADE,
  ref_strength REAL,                   -- 0.0-1.0 — how much memo depends on symbol's truth
  PRIMARY KEY (memo_id, symbol_id)
);

-- Coord channel (replaces tfs.coord_messages)
CREATE TABLE fabric.events (
  id BIGSERIAL PRIMARY KEY,
  ts TIMESTAMPTZ NOT NULL DEFAULT now(),
  sender TEXT NOT NULL,
  recipient TEXT NOT NULL,
  subject TEXT,
  body TEXT,
  metadata JSONB DEFAULT '{}'
);
CREATE INDEX events_recency ON fabric.events (ts DESC);
CREATE INDEX events_sender ON fabric.events (sender);
CREATE INDEX events_recipient ON fabric.events (recipient);

-- Session registry
CREATE TABLE fabric.sessions (
  letter TEXT NOT NULL,
  host TEXT NOT NULL,
  role TEXT,
  capabilities TEXT[],                 -- e.g. ['code_gen_go', 'cycles_render', 'cuttlefish_avd']
  status TEXT,                         -- alive | paused | stopped
  last_heartbeat_at TIMESTAMPTZ DEFAULT now(),
  registered_at TIMESTAMPTZ DEFAULT now(),
  metadata JSONB DEFAULT '{}',         -- e.g. {pid: 12345, claude_session_id: '...'}
  PRIMARY KEY (letter, host)
);

-- Host registry (multi-host federation)
CREATE TABLE fabric.hosts (
  hostname TEXT PRIMARY KEY,
  ip_address INET,
  ip_wireguard INET,
  last_heartbeat_at TIMESTAMPTZ,
  capabilities JSONB,                  -- {gpu: ['3090', '3090', '3070', '3070'], cpu_cores: 64, ram_gb: 1024}
  status TEXT                          -- online | degraded | offline
);

-- Task graph (orchestrator)
CREATE TABLE fabric.tasks (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id TEXT NOT NULL,
  assigned_to_letter TEXT,
  assigned_to_host TEXT,
  status TEXT NOT NULL DEFAULT 'pending',  -- pending | in_progress | blocked | done | failed
  title TEXT NOT NULL,
  description TEXT,
  required_capabilities TEXT[],
  depends_on UUID[],
  created_by_session TEXT,             -- letter
  created_at TIMESTAMPTZ DEFAULT now(),
  updated_at TIMESTAMPTZ DEFAULT now(),
  completed_at TIMESTAMPTZ,
  metadata JSONB DEFAULT '{}'
);

-- Router learning data (the fabric/router pairing)
CREATE TABLE fabric.task_outcomes (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  request_id UUID,
  task_class TEXT,                     -- 'code_gen_go' | 'email_compose_sales' | 'persona_voice_reply' ...
  backend TEXT,                        -- 'qwen-32b-local' | 'gemini-2.5-flash' | 'imprint-27b'
  success BOOLEAN NOT NULL,
  cost_pence NUMERIC(10,4),
  duration_ms INT,
  quality_score REAL,                  -- 0.0-1.0, from validator OR human feedback
  ts TIMESTAMPTZ NOT NULL DEFAULT now(),
  metadata JSONB DEFAULT '{}'
);
CREATE INDEX task_outcomes_class_backend ON fabric.task_outcomes (task_class, backend, ts DESC);
```

## MCP surface (what Claude sessions see)

```
mcp__fabric__search                 — semantic + graph-aware memo retrieval
mcp__fabric__remember               — write memo with dedup + linking
mcp__fabric__forget                 — soft-delete (TTL → now)
mcp__fabric__verify                 — re-check belief against current state

mcp__fabric__graph_query            — symbol lookup with edges
mcp__fabric__graph_subgraph         — min-graph for a question

mcp__fabric__coord_send             — replaces kx-coord-send
mcp__fabric__coord_history          — replaces kx-coord-tail
mcp__fabric__coord_listen           — subscribe to incoming (MCP streaming)

mcp__fabric__task_create            — file new task; orchestrator dispatches
mcp__fabric__task_update            — status/progress
mcp__fabric__task_list              — filter by status/assignee

mcp__fabric__session_register       — claim letter + capabilities
mcp__fabric__session_heartbeat      — keep-alive
mcp__fabric__session_handoff        — explicit work transfer between sessions

mcp__fabric__routing_best_backend   — router queries this
mcp__fabric__outcome_record         — router writes outcomes here

mcp__fabric__incident_replay        — correlate window across all event types
mcp__fabric__demo_ready             — 1-line "what channels are demo-ready right now"
```

## HTTP REST API surface

Same operations exposed for non-Claude clients (CI, scripts, bash, future LLMs). Bearer-token auth.

```
POST   /v1/memo/search          {query, filter?, top_k?, scope?}
POST   /v1/memo/remember        {title, body, type, visibility?, tags?, references?}
POST   /v1/memo/forget          {id}
POST   /v1/memo/verify          {id}

POST   /v1/graph/query          {symbol?, file_path?, kind?}
POST   /v1/graph/subgraph       {seed_symbol, depth?}

POST   /v1/coord/send           {sender, recipient, subject, body, metadata?}
GET    /v1/coord/history        ?n=50&filter=letter
GET    /v1/coord/listen         (SSE stream)

POST   /v1/task/create          {title, description, required_capabilities, depends_on?}
PATCH  /v1/task/{id}            {status, notes?}
GET    /v1/task/list            ?status=pending&assignee=A

POST   /v1/session/register     {letter, host, role, capabilities}
POST   /v1/session/heartbeat    {letter, host}
POST   /v1/session/handoff      {from, to, task_ids, summary}

GET    /v1/routing/best_backend ?task_class=X&max_cost_pence=Y&min_success_rate=Z
POST   /v1/routing/outcome      {request_id, backend, task_class, success, cost_pence, duration_ms, quality_score?}

GET    /v1/incident/replay      ?from=TS1&to=TS2&correlate=deployments,memos,events
GET    /v1/demo_ready
GET    /healthz
GET    /readyz
GET    /metrics                 (Prometheus)
```

## Auth

- **Per-session API key** — each Claude session registers + receives a key. Used for MCP + HTTP calls.
- **Bearer for cross-host federation** — fabric daemons authenticate to each other.
- **Operator master key** — for emergency admin access. Stored in `~/.kronaxis/env`.
- **No PKI** — all keys are simple bearer strings. Rotation via config file edit.

## What this does NOT include in v1

- Web UI (operator queries via Claude — no separate dashboard)
- Multi-tenant ACL beyond `tenant_id` field (defer until first BoS customer signs)
- Graph cypher-like query language (use simple traversal API)
- gRPC interface (HTTP + MCP enough for 3-5 hosts)
- Plugin system (core is plenty)
- Real-time subscriptions for clients beyond pg_notify
- OAuth/OIDC (defer)

## See also

- [`02-token-efficiency.md`](02-token-efficiency.md)
- [`03-ipc-design.md`](03-ipc-design.md)
- [`04-speed-delivery.md`](04-speed-delivery.md)
- [`05-router-maximisation.md`](05-router-maximisation.md)
- [`06-shipping-plan.md`](06-shipping-plan.md)
- [`07-orchestrator-multihost.md`](07-orchestrator-multihost.md)
