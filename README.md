<p align="center">
  <img src="assets/kronaxis-icon.svg" width="64" height="64" alt="Kronaxis">
</p>

<h1 align="center">Kronaxis Fabric</h1>

<p align="center">
  <strong>Memory + coordination MCP for multi-agent LLM workflows. One Go binary. Postgres + pgvector hybrid search.</strong>
</p>

<p align="center">
  <a href="LICENSE">BSL 1.1</a> &middot;
  <a href="https://kronaxis.co.uk">Website</a>
</p>

---

Single-file Go server (~700 LoC) backed by Postgres and Ollama `nomic-embed-text` 768-dim embeddings. Hybrid semantic + tsvector + recency ranking over a memo store, plus a cross-session coord channel.

## Status

**v0.2.0.** Single shared Bearer token, no multi-tenancy, no RBAC. Production-ready for single-operator / small-team use; the items in [Roadmap](#roadmap) gate broader deployment.

## What v0.2.0 ships

- **Bearer-token auth** (single shared `FABRIC_KEY`) on every endpoint except `/v1/health`
- **Memo CRUD**: `POST /v1/memo`, `GET/PUT/DELETE /v1/memo/:id` with sha256 dedup via `ON CONFLICT`
- **Hybrid search**: `POST /v1/memo/search` with three modes
  - `hybrid` (default) — cosine 50% + tsvector 30% + recency 20%
  - `semantic` — embeddings only (cosine + recency)
  - `tsvector` — full-text only (rank + recency)
- **Embeddings** via Ollama `nomic-embed-text` (`OLLAMA_URL`, default `http://localhost:11434`), 768-dim, stored in pgvector with ivfflat index. Best-effort on write; falls back to NULL on Ollama failure
- **Backfill**: `POST /v1/memo/backfill` embeds any memos with NULL `embedding`
- **Coord channel**: `POST /v1/coord` (send) + `GET /v1/coord/recent` (poll) via `public.coord_messages`
- **Soft delete**: `DELETE` sets `deleted_at`; all reads filter `deleted_at IS NULL`

## Endpoints

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/v1/health` | none | db ping + version + embedding model |
| POST | `/v1/memo` | Bearer | create; sha256 dedup |
| GET | `/v1/memo/:id` | Bearer | fetch single |
| PUT | `/v1/memo/:id` | Bearer | partial update; re-embed on change |
| DELETE | `/v1/memo/:id` | Bearer | soft delete |
| POST | `/v1/memo/search` | Bearer | `{query, top_k, type, mode}` |
| POST | `/v1/memo/backfill` | Bearer | embed all NULL-embedding memos |
| POST | `/v1/coord` | Bearer | `{sender, recipient, subject, body}` |
| GET | `/v1/coord/recent` | Bearer | recent coord messages |

## Environment

| Var | Required | Default |
|---|---|---|
| `FABRIC_KEY` | yes | — |
| `FABRIC_PG_DSN` | yes | — |
| `FABRIC_LISTEN` | no | `:8201` |
| `OLLAMA_URL` | no | `http://localhost:11434` |

## Build + run

```bash
go build -o fabric ./cmd/fabric
FABRIC_KEY=... FABRIC_PG_DSN="postgres://..." ./fabric
```

Schema (memos + indexes) auto-applies on startup; the target database needs the `vector` extension installed.

## MCP shim + import helper

- `scripts/kx-fabric-mcp.py` — MCP stdio shim. Wire under `~/.claude.json` `mcpServers.fabric`. Exposes `search`, `remember`, `health` tools.
- `scripts/kx-fabric-import.py` — bulk markdown importer. Walks a directory of `*.md` files (with optional YAML frontmatter) and POSTs each via `/v1/memo`. Idempotent via sha256 dedup.

Both honour `FABRIC_URL` + `FABRIC_KEY` env vars.

## Layout

```
cmd/fabric/main.go      single-file server (~700 LoC)
deploy/migrations/      schema migrations (schema is currently inline in main.go)
deploy/systemd/         sample systemd unit
deploy/examples/        sample env + curl
scripts/                kx-fabric-import.py + kx-fabric-mcp.py
LICENSE                 BSL-1.1 (Change Date 25 May 2030)
```

## Roadmap

In priority order:

- `pg_notify` pub/sub on `coord_messages` for sub-second cross-session delivery
- Per-tenant API keys + RBAC (currently single shared key)
- Live code-graph ingestion (tree-sitter + inotify)
- Session presence + heartbeat for orchestrator dispatch

## Licence

[BSL-1.1](LICENSE). Source-available; Change Date 25 May 2030, Change License Apache 2.0.

For commercial licensing: contact@kronaxis.co.uk
