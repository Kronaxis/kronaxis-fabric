# 03 — Inter-process communication design

## What fabric replaces

Today's IPC stack is layered:

| Pattern | Used for | Pain |
|---|---|---|
| `ssh + bash + tmux` | Multi-host command dispatch | Brittle. SSH drops kill long-running processes. |
| `HTTP` to agentmemory:3111 | Memory queries from Claude | Roundtrip; extra Python process; rate-limit prone (Gemini quota incidents) |
| `HTTP` to LeadDistro / Stalwart / Lucy / LTX / various | Each service its own API | Per-service auth, per-service URL, per-service downtime |
| `PG LISTEN/NOTIFY` on `tfs.coord_messages` | Cross-session coordination | Working, but bash-script wrapping is fragile |
| `tail -F chan.log` | Legacy mirror for laptop sessions | Listeners die on SSH drops (D missed 4hr of messages tonight) |
| Manual `rsync` / `scp` | Asset + log transfer | Manual, no audit |
| `journalctl` / `grep` / `tail` | State investigation | Distributed, no unified history |

Fabric's IPC stack collapses this to **four transports**, each chosen for a specific access pattern:

## The four IPC patterns

### 1. MCP (Claude session ↔ fabric)

**Used for**: Claude Code sessions calling fabric tools.

**Wire**: MCP protocol over stdio (Claude Code default) OR SSE (Claude Code can be configured for streaming).

**Authentication**: API key per session-letter, stored in operator's `~/.claude.json` config.

**Latency budget**: <100ms p99 (local fabric daemon).

**Failure mode**: MCP server restart loses streaming subscribers; Claude Code reconnects automatically.

**Why MCP for Claude**: It's the native protocol Anthropic ships with Claude Code. No bridges, no shims. Replaces `mcp__agentmemory__*` cleanly.

### 2. HTTP REST (everything else ↔ fabric)

**Used for**: CI workflows, bash scripts, future LLM clients (Cursor, Qwen-local), human-curl curiosity.

**Wire**: HTTP/1.1 (no need for HTTP/2 at this scale). JSON payloads.

**Authentication**: Bearer token. One token per client identity. Tokens in `~/.kronaxis/env` or `/etc/kronaxis-fabric/api_keys/`.

**Latency budget**: <50ms p99 over LAN/WireGuard, <200ms over WAN.

**Failure mode**: Standard HTTP retry semantics. Idempotent endpoints support `Idempotency-Key` header.

**Why HTTP**: Universal. Curl-able for debugging. Easy to script. No language SDK lock-in.

### 3. PG LISTEN/NOTIFY (fabric ↔ fabric across hosts)

**Used for**: Cache invalidation across federated fabric daemons.

**Wire**: Postgres native pub/sub. Channels:
- `fabric_memo_write` — fires when a memo is inserted/updated
- `fabric_event_published` — fires when a coord event lands
- `fabric_symbol_changed` — fires when a code symbol's body_sha256 changes
- `fabric_session_state_change` — fires when a session registers/heartbeats/drops
- `fabric_task_state_change` — fires when a task transitions

**Authentication**: Postgres connection credentials. One DB user (`fabric_node`) shared by all daemons.

**Latency**: ~1ms LAN, ~5ms over WireGuard. pg_notify is at-least-once.

**Failure mode**: NOTIFY can drop on disconnect. Mitigation: each daemon tracks a `last_seen_event_id` checkpoint and runs a catch-up SELECT on reconnect.

**Why PG_NOTIFY over NATS / Kafka / RabbitMQ**: We already run Postgres. Adding a broker adds a service to operate, monitor, back up, secure. For our scale (3-5 hosts, <1000 events/hour), PG is genuinely sufficient.

### 4. gRPC (high-volume node-to-node, deferred to v2)

**Used for**: If/when bulk data transfer is needed between fabric daemons (e.g. backfilling a new host's cache).

**Wire**: HTTP/2 with protobuf.

**Status**: NOT in v1. PG + HTTP is enough.

## Local cache architecture

Each fabric daemon has an in-process LRU cache backed by `freecache` or `bigcache`:

```
┌──────────────────────────────────────────┐
│  Fabric daemon (one Go binary)           │
│                                          │
│  ┌────────────────────────────────────┐  │
│  │  In-process LRU cache (256MB)      │  │
│  │  - hot memo bodies                 │  │
│  │  - recent search results           │  │
│  │  - symbol → callers graph slices   │  │
│  │  - session presence state          │  │
│  └────────────┬───────────────────────┘  │
│               │                          │
│  ┌────────────▼──────┐  ┌─────────────┐  │
│  │  pgx pool         │  │  pg LISTEN  │  │
│  │  (PG read/write)  │  │  consumer   │  │
│  └────────┬──────────┘  └──────┬──────┘  │
└───────────┼─────────────────────┼────────┘
            │                     │
            ▼                     │
     ┌──────────────────────┐     │
     │  Postgres (DL580)    │◄────┘
     │  tfs.fabric.*        │   (NOTIFY events)
     └──────────────────────┘
```

**Cache hit path**: ~1ms (in-process map lookup).
**Cache miss path**: ~10ms (PG SELECT + cache fill).
**Cache invalidation**: pg_notify message tells daemon to evict (or refresh) specific cache keys.

## Concrete latency budgets per operation

| Operation | Local cache hit | Local cache miss | Cross-host |
|---|---|---|---|
| Memo search (top-5) | 1ms | 12ms | 15ms |
| Memo write | n/a (always writes through) | 8ms | 15ms |
| Graph 1-hop query | 1ms | 7ms | 12ms |
| Coord send | 5ms (sync write to PG) | 5ms | 10ms |
| Coord receive (pg_notify) | 1ms (callback fires) | 1ms | 5ms |
| Task create | 8ms | 8ms | 15ms |
| Task list (10 items) | 1ms | 10ms | 15ms |
| Routing best-backend | 1ms | 8ms | 12ms |
| Outcome record | 5ms (async-able) | 5ms | 10ms |
| Embedding (Ollama HTTP) | 50-100ms (first call) | 5ms (Ollama-side cache) | n/a — local Ollama |

**Compared to today**:
- agentmemory smart-search: ~200ms (HTTP roundtrip + Python overhead)
- chan.log tail: ~50ms (file I/O + grep)
- ssh dispatch: ~500-2000ms (SSH handshake)

**10-50× latency improvement on hot paths.**

## Failure modes + recovery

### Postgres on DL580 goes down

**Detected by**: `pgx` connection errors on each fabric daemon.

**Behaviour**:
- Writes queue locally (in `/var/lib/kronaxis-fabric/write_queue/`, capped at 10,000 events)
- Reads served from local cache; misses return `503 Service Unavailable`
- Operator alerted via NTFY within 30s of detection
- Recovery: PG comes back → daemon drains write queue → cache rehydrates on next reads

**Data loss risk**: Zero, assuming write queue doesn't overflow. With 256MB cache + 10k queue entries, that's ~24hr of survival.

### Network partition (laptop ↔ DL580 over WireGuard)

**Detected by**: pg_notify subscriber disconnects.

**Behaviour**:
- Laptop fabric continues to serve reads from cache (gradually stale)
- Writes from laptop queue locally
- DL580 + R920 continue normally (they share LAN)
- Operator notified after 60s
- Recovery: WireGuard reconnects → catch-up runs → cache refreshes

### Single fabric daemon crashes

**Detected by**: systemd `RestartSec=5` + heartbeat absence on `fabric.sessions`.

**Behaviour**:
- systemd restarts within 5s
- Local cache lost but rehydrates on first reads
- Other fabric nodes unaffected
- Sessions that were registered to this host re-register on restart

### pg_notify drops messages

**Detected by**: monotonic `event.id` gaps on subscriber.

**Behaviour**:
- Each daemon tracks `last_processed_event_id` per channel
- On reconnect OR periodic check (every 60s), runs `SELECT * FROM fabric.events WHERE id > $last_id` to catch up
- Idempotent processing (events have unique IDs)

## Compared to today's IPC

| Failure type | Today's behaviour | Fabric behaviour |
|---|---|---|
| SSH drops mid-listener (D tonight, 4hr loss) | Listener dies silently; backlog lost | Daemon reconnects, fills from PG using checkpoint |
| chan-bridge daemon dies on laptop sleep | Coord channel half-broken until manual restart | Fabric resumes on wake, syncs delta |
| agentmemory Python process OOM | Memory queries 500 until manual restart | Go daemon restart in 5s; cache rebuilds |
| Postgres restart | Most things break, manual coordination | Fabric write queue absorbs; auto-recovers |
| WireGuard tunnel flap | Manual reconnect of multiple bash listeners | Single daemon's pg connection reconnects |

## Wire-level protocol summary

| Connection | Protocol | Authentication | Encryption |
|---|---|---|---|
| Claude Code → fabric (local) | MCP/stdio | per-session API key | filesystem perms |
| Claude Code → fabric (remote) | MCP/SSE | per-session API key | TLS (via reverse proxy) |
| Bash/curl/CI → fabric | HTTP REST | bearer token | TLS optional in v1 (LAN only) |
| Fabric daemon → Postgres | pgx (binary protocol) | DB user/pass | SSL to PG (PG already supports) |
| Fabric daemon → Fabric daemon (v2) | gRPC | mTLS | mTLS |
| Fabric daemon → Ollama | HTTP REST | none | none (localhost only) |
| Router → Fabric | HTTP REST | bearer token | TLS optional |

## See also

- [`04-speed-delivery.md`](04-speed-delivery.md) — Latency + engineering velocity
- [`07-orchestrator-multihost.md`](07-orchestrator-multihost.md) — How multi-host federation uses these patterns
