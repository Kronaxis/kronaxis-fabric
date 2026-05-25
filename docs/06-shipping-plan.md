# 06 — Six-week shipping plan

Each week ships something independently usable. Operator can pause at any week and have working tools. Each week has a Friday demo with concrete acceptance criteria.

## Week-by-week

### Week 1 — Memory MCP + HTTP + ollama embedding

**Goal**: replace `agentmemory` as the daily-driver memory service.

**Deliverables**:
- `cmd/fabric/main.go` entry point, config loading, signal handling
- `internal/store/memos.go` with full CRUD via pgx
- `internal/embed/ollama.go` HTTP client to local Ollama (default model: `all-minilm`)
- `internal/search/{semantic,hybrid}.go` with pgvector cosine + recency rerank
- `internal/api/{http_server,mcp_server,auth}.go` with the v1 endpoint set
- Schema migration `001_memos.sql` creates `fabric.memos` + `fabric.memo_links`
- systemd unit + example config
- Smoke test: import 100 memos from agentmemory, A/B compare top-K search

**LoC estimate**: 2,500 Go.

**Replaces**: `agentmemory` daemon (Python/Node + Xenova HTTP).

**Migration phase**: Phase A — fabric runs alongside agentmemory. Both receive writes. Reads still go to agentmemory. Zero risk.

**Demo (Friday week 1)**:
```
$ curl -X POST http://dl580:8200/v1/memo/search \
    -H "Authorization: Bearer $FABRIC_KEY" \
    -d '{"query":"Lucy port","top_k":3}'
{
  "results": [
    {"id":"...", "title":"Lucy F5-TTS clone on DL580", "body_excerpt":"port 8872, ...", "score":0.94, "type":"reference"},
    {"id":"...", "title":"Voice infrastructure inventory", "body_excerpt":"Lucy on port 8872, ...", "score":0.81, "type":"project"},
    ...
  ],
  "latency_ms": 11
}
```

Plus: switch `~/.claude.json` to fabric MCP for one session, eat dogfood for 24 hours.

**Acceptance**:
- p99 search latency <50ms over LAN
- ≥95% search-result parity with agentmemory on identical corpora
- Dedup works (sha256 deduplication on import + remember calls)
- Zero data loss on daemon restart

---

### Week 2 — Multi-host federation

**Goal**: laptop + DL580 + R920 each run a fabric daemon, share state via PG + pg_notify, no chan.log bridge needed.

**Deliverables**:
- `internal/coord/{pg_listen,pg_send,compat}.go` — replaces chan.log bridge
- Local cache implementation with pg_notify-driven invalidation
- `internal/store/{sessions,hosts}.go` — session + host registry
- Federation reconnect logic with checkpoint-based catch-up
- bash `kx-coord-send` rewritten as thin curl wrapper to fabric HTTP
- Schema migration `002_coord_sessions_hosts.sql`

**LoC estimate**: 1,500 Go + ~100 LoC bash refactor.

**Replaces**: chan.log bridge + bash `kx-coord-*` (kept as compat layer).

**Migration phase**: Phase B — coord traffic mirrored to both `tfs.coord_messages` AND `fabric.events`. After 1 week parallel run, switch reads to fabric.

**Demo (Friday week 2)**:
- Message sent from DL580 fabric arrives in laptop fabric within 50ms
- Simulated WireGuard drop: laptop fabric queues 50 events locally, drains on reconnect, zero loss
- Session B on DL580 + Session E on laptop both register; both visible via `mcp__fabric__session_list`

**Acceptance**:
- Cross-host latency p99 <100ms
- Network partition recovery <60s after WireGuard restoration
- Zero message loss in 1000-event simulation with random 30s outages

---

### Week 3 — Live code graph

**Goal**: replace graphify cron with tree-sitter-driven inotify-based incremental graph updates. Memos referencing changed symbols auto-flag stale.

**Deliverables**:
- `internal/graph/{extract,incremental,query}.go`
- tree-sitter integrations: Go, Python, Bash, Markdown, YAML
- inotify watcher (recursive) on `~/projects/kronaxis/` + `~/projects/kronaxis-mannequin/` etc
- Schema migration `003_symbols.sql`
- `mcp__fabric__graph_query` + `mcp__fabric__graph_subgraph` tools
- Cold-start indexer (one-shot full scan of repo tree)
- Memo-symbol reference auto-extraction on `remember()` call

**LoC estimate**: 3,000 Go.

**Replaces**: graphify cron (Python).

**Migration phase**: Phase C — graphify cron still runs but its output ignored. Fabric is the source of truth. After 1 week stable, graphify cron disabled.

**Demo (Friday week 3)**:
- Edit `voice.py` function `synth_line_lucy`
- 5 seconds later: `mcp__fabric__graph_query "synth_line_lucy"` returns updated function + 3 memos now marked `[STALE]`
- `mcp__fabric__graph_subgraph --symbol "F5TTS" --depth 2` returns 12 related symbols in ~20ms

**Acceptance**:
- Code change → graph update <10s wall clock
- Cold start (full kronaxis tree scan) <5 minutes
- Memo staleness flag fires correctly on symbol body_sha256 change
- Graph query latency p99 <50ms

---

### Week 4 — CI integration

**Goal**: every deploy, commit, service restart, daily-check failure writes a memo. Fabric becomes the unified timeline of "what happened when".

**Deliverables**:
- `internal/ci/{github_webhook,systemd_watcher,git_hook,memo_emit}.go`
- GitHub Actions webhook receiver on fabric:8200/v1/ci/github
- journald subscriber via `sdjournal` Go bindings
- post-receive git hook on bare repo on DL580
- coord-channel auto-extractor for "DONE", "BLOCKER", "FAIL" patterns
- New memo types: `deployment`, `commit`, `service_state`, `health_alert`, `revenue_event`, `audit`
- `mcp__fabric__incident_replay` tool that correlates a time window

**LoC estimate**: 2,000 Go.

**Replaces**: nothing existing — net new capability.

**Demo (Friday week 4)**:
- Push a commit to `kronaxis-mannequin` → fabric memo emitted within 5s with diff summary
- `systemctl restart kronaxis-router` on DL580 → memo emitted within 1s
- daily-checks.sh fails an isolation test → memo emitted + ntfy fired
- `mcp__fabric__incident_replay --window 14:00-15:00` returns ordered timeline of all events

**Acceptance**:
- 100% of GitHub deploy events captured
- 100% of systemd unit state changes captured (filtered for noise — only restarts/failures by default)
- Incident replay completes <2s for 1-hour window with 100 events

---

### Week 5 — Orchestrator (session registry, capabilities, task graph, dispatch)

**Goal**: replace hand-coordinated MAIN→A-G pattern with typed session capabilities, task graph, automatic dispatch by capability match.

**Deliverables**:
- `internal/orchestrator/{registry,dispatch,status,escalation}.go`
- `internal/store/tasks.go`
- Session capability inventory (defined per session at registration)
- Task creation with `required_capabilities` array
- Dispatch logic: capability match + host load + recency-of-last-activity
- Cadence enforcement (sessions must heartbeat per their declared cadence)
- Escalation: missing heartbeat → operator NTFY
- Schema migration `005_tasks.sql`

**LoC estimate**: 2,500 Go.

**Replaces**: hand-coordinated MAIN→A/B/C/D/E pattern.

**Migration phase**: Phase D — operator can continue using `kx-coord-send` style; orchestrator runs in parallel. Workers opt-in to orchestrator dispatch.

**Demo (Friday week 5)**:
- MAIN files: `mcp__fabric__task_create title="Render mannequin scene 5" required=["cycles_render","gpu_3090"]`
- Orchestrator picks Session B on DL580 (has cycles_render + has 3090 + lowest current load)
- B accepts via `mcp__fabric__task_accept`
- B completes via `mcp__fabric__task_update status=done`
- Orchestrator records outcome including duration

**Acceptance**:
- Dispatch decision latency <50ms
- Capability-mismatch tasks correctly reject
- Heartbeat-loss → operator notified within 60s

---

### Week 6 — Persona absorption + router integration

**Goal**: unify persona memory (`soul_memory`) under fabric. Activate router learning loop.

**Deliverables**:
- Migration: read `soul_memory` rows into `fabric.memos` with `tenant_id='kronaxis'` + `type='persona_lived'`
- vanguard_interactions write hook: every interaction → fabric memo
- `internal/router_integration/{outcome_record,routing_policy,classifier}.go`
- Schema migration `006_task_outcomes.sql`
- kronaxis-router patch: query fabric for `best_backend` before dispatch
- Daily routing report via NTFY
- Operator-facing `mcp__fabric__routing_report` tool

**LoC estimate**: 2,000 Go + ~200 LoC router patch.

**Replaces**: ad-hoc routing rules + dormant `soul_memory`.

**Migration phase**: Phase E — fabric is primary memory for both operator + personas. agentmemory daemon decommissioned. Router queries fabric.

**Demo (Friday week 6)**:
- Mary about to call Pearl Insurance: fabric retrieves 5 prior interaction memos in <20ms
- Router queries fabric: `best_backend` for `email_compose_sales` task_class returns slatewick-lora-v9 with confidence 0.92
- Daily routing report at 09:00 BST: "1247 routed calls, £0.32 saved vs static baseline, 3 backends demoted, 2 promoted"

**Acceptance**:
- Persona memory retrieval p99 <50ms
- Router-fabric round-trip <10ms
- 100+ routing outcomes recorded in 24h
- Operator can see backend rankings via report tool

---

## Total resource cost

| Item | Estimate |
|---|---|
| Total LoC | ~13,500 Go + ~300 LoC supporting |
| Engineering weeks | 6 focused weeks (could compress to 4 with server-side claude code-gen) |
| Operator review time | ~2-3h/week (demo + design feedback) |
| New infrastructure | None (Postgres + Ollama already running) |
| New paid services | None |
| Net new ongoing operational cost | £0 (replaces 3 daemons with 1 binary) |

## Migration phases (non-destructive throughout)

| Phase | Week | Description | Rollback |
|---|---|---|---|
| A | 1 | Fabric writes both to agentmemory + fabric. Reads still agentmemory. | Disable fabric service. |
| B | 2 | Reads switch to fabric. agentmemory still receives writes (compat). | Revert `~/.claude.json` MCP config. |
| C | 3 | agentmemory writes stopped. Fabric primary. agentmemory data preserved. | Restart agentmemory daemon. |
| D | 5 | Orchestrator runs alongside hand-coord. Opt-in dispatch. | Stop using `mcp__fabric__task_*` calls. |
| E | 6 | Persona memory unified. Router queries fabric. | Reset router config to static. |
| F | post-6 | Decom agentmemory + chan-bridge + graphify cron. | Reinstall + manual data restore from PG backup. |

## Risks specific to shipping plan

1. **Week 1 schema lock-in**: Once `fabric.memos` has 10k rows, schema migration is costly. **Mitigation**: design review in week 0, minimal v1 fields, JSONB metadata for flex.

2. **Week 3 tree-sitter coverage**: Some languages will have edge cases (Cuttlefish config, systemd unit files). **Mitigation**: priorities Go/Python/Bash; regex fallback for niche formats.

3. **Week 4 GitHub Actions webhook auth**: Operator needs to add fabric URL as a GH secret. **Mitigation**: low-friction setup script.

4. **Week 5 orchestrator semantics**: Capability inventory needs to be standardised across sessions. **Mitigation**: define capability taxonomy in week 4, get operator review before implementing.

5. **Week 6 soul_memory migration**: 10,545 rows of `lived` entries, vector(1024) → fabric.memos vector(384). Need to re-embed via Ollama. **Mitigation**: keep `soul_memory` untouched until fabric.memos validated for 1 week post-migration.

## Build accelerators

- **Server-side claude code-gen on DL580**: heavy module work dispatched there saves laptop attention
- **Qwen-local for code review**: drops API spend during build by 50-70%
- **Existing BoS Go patterns**: pgx, systemd, layered architecture all already in operator's codebase — fabric inherits patterns
- **Tree-sitter Go bindings** (`smacker/go-tree-sitter`): production-grade, no R&D needed
- **pgvector + hnsw**: already running, no R&D needed
- **systemd ops experience**: operator has 50+ systemd units operational; fabric is just one more

## What's deferred to post-week 6

- Web UI / dashboard
- Multi-tenant ACL beyond `tenant_id` field
- gRPC inter-node transport
- Cypher-like graph query language
- OAuth / SSO
- Plugin system for community contributions
- Distributed embedding (multiple ollama instances load-balanced)

These all become real questions once fabric is the production system. For shipping, simple is correct.

## See also

- [`01-architecture-overview.md`](01-architecture-overview.md)
- [`07-orchestrator-multihost.md`](07-orchestrator-multihost.md)
