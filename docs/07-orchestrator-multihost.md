# 07 — Orchestrator + multi-host: secondary sessions on differing hardware

## The pattern that exists today (and breaks regularly)

Kronaxis ops already runs a multi-host, multi-agent pattern:

- **Laptop** (16GB, control plane): MAIN session + operator + some lighter workers
- **DL580** (944GB RAM, 4 GPUs — 2× RTX 3090 + 2× RTX 3070, multi-CPU): heavy compute, GPU work, vLLM, Lucy TTS, slatewick-lora, Blender Cycles render, persona inference
- **R920** (Cuttlefish AVD host, lower-spec but dedicated): Android persona work, AVD spawning, Magisk armouring
- **VPS** (mail.slatewick.co.uk): Stalwart, public-facing services
- **Future**: customer-hosted nodes when BoS goes multi-tenant

Sessions are tagged by **letter** (A/B/C/D/E/MAIN/etc) and **role**. Today the host-binding is informal — operator decides "this work goes on DL580 because GPU" or "this work goes on R920 because Cuttlefish". The decision lives in operator's head; sessions discover their host by being spawned there.

**What breaks today**:

- No formal capability inventory — "does this host have GPU?" requires operator memory
- Manual dispatch — operator types `ssh dl580 'tmux attach -t kx-foo'`
- Lost continuity — when DL580 reboots, dispatched sessions die; restart-and-recover is manual (operator saw the OAuth-token-loss pattern tonight)
- Coord channel is presence-blind — D's listener died for 4hr and nobody noticed
- Tasks fire-and-forget — MAIN sends "do X"; if worker doesn't reply, who follows up?
- Refusal rerouting is manual — MAIN saw Claude refuse signup-audit, dispatched to qwen-local by hand
- No load awareness — could dispatch a Cycles render to DL580 while it's already running Piper distillation

Fabric formalises every one of these.

## How fabric handles multi-host orchestration

### 1. Hosts register themselves

On startup, each fabric daemon writes/updates its host row:

```sql
INSERT INTO fabric.hosts (
  hostname, ip_address, ip_wireguard, capabilities, status
) VALUES (
  'dl580',
  '192.168.50.129',
  '10.66.66.2',
  '{
    "gpu": ["RTX 3090 #0 @ 24GB", "RTX 3090 #1 @ 24GB", "RTX 3070 #0 @ 8GB", "RTX 3070 #1 @ 8GB"],
    "cpu_cores": 64,
    "ram_gb": 944,
    "storage_tb": 2.0,
    "services_running": ["lucy-tts", "slatewick-lora", "imprint-27b", "ltx-video", "bos-controld", ...],
    "claude_code_available": true,
    "ollama_available": true
  }'::jsonb,
  'online'
)
ON CONFLICT (hostname) DO UPDATE SET ...
```

Heartbeats every 30s update `last_heartbeat_at`. Operator can query:

```
mcp__fabric__host_list
→ [
  {hostname: "laptop-jason-XPS-13", capabilities: {...}, status: "online", sessions: ["MAIN", "E"]},
  {hostname: "dl580", capabilities: {...}, status: "online", sessions: ["A", "B", "DL580-vanguard"]},
  {hostname: "r920", capabilities: {...}, status: "online", sessions: ["F"]}
]
```

### 2. Sessions register themselves with capabilities

When a session starts, it claims a letter and declares its capabilities:

```
mcp__fabric__session_register
  letter: "B"
  host: "dl580"
  role: "messaging-channel-engineering"
  capabilities: [
    "go_development",
    "cuttlefish_avd",
    "magisk_armouring",
    "wireprotocol_implementation",
    "cycles_render",
    "gpu_3090"
  ]
  cadence_min: 30
```

`fabric.sessions` row inserted. Letter collisions detected at insert (PRIMARY KEY on (letter, host) — same letter on different hosts is fine; same letter same host conflicts).

### 3. Capability inventory — standardised taxonomy

Fabric ships with a known capability taxonomy. Sessions claim from it; orchestrator matches against it.

**Sample taxonomy**:

| Capability category | Specific capabilities |
|---|---|
| Code work | `go_development`, `python_development`, `bash_scripting`, `typescript_development`, `code_review`, `refactor_planning` |
| GPU compute | `gpu_3090`, `gpu_3070`, `cycles_render`, `vllm_inference`, `lora_training`, `tts_synthesis`, `image_generation`, `video_generation` |
| Persona work | `mary_voice_orchestration`, `holly_email_orchestration`, `vanguard_pipeline`, `outbound_sequence_execution` |
| Channel work | `email_channel_engineering`, `voice_channel_engineering`, `whatsapp_channel_engineering`, `video_channel_engineering` |
| Infrastructure | `postgres_admin`, `nas_storage`, `cuttlefish_avd`, `magisk_armouring`, `systemd_admin`, `wireguard_admin` |
| Business | `leaddistro_buyer_outreach`, `slatewick_vertical_dev`, `compliance_review`, `legal_drafting`, `grants_compliance` |
| Operator | `ops_maintenance`, `health_monitoring`, `incident_response`, `nas_migration` |

Operator can extend the taxonomy via `fabric.capabilities` table (TBD as v2 — for v1 sessions can claim arbitrary strings).

### 4. Task dispatch by capability + load match

When MAIN files a task:

```
mcp__fabric__task_create
  title: "Render Patisserie Valerie shot 12"
  description: "Re-render with new mannequin asset, Cycles GPU"
  required_capabilities: ["cycles_render", "gpu_3090"]
  estimated_duration_min: 30
  priority: "medium"
```

Orchestrator matches:

```sql
SELECT s.letter, s.host, h.capabilities, 
       (NOW() - s.last_heartbeat_at) AS staleness,
       (SELECT COUNT(*) FROM fabric.tasks WHERE assigned_to_letter = s.letter 
        AND assigned_to_host = s.host AND status = 'in_progress') AS active_load
FROM fabric.sessions s
JOIN fabric.hosts h ON h.hostname = s.host
WHERE s.status = 'alive'
  AND s.last_heartbeat_at > NOW() - interval '5 minutes'
  AND s.capabilities @> ARRAY['cycles_render', 'gpu_3090']
  AND h.status = 'online'
ORDER BY active_load ASC, staleness ASC
LIMIT 1;
```

Result: best session for the task. Orchestrator marks task `assigned_to_letter` + `assigned_to_host`. Session is notified via pg_notify within ms.

**Why this matters operationally**:

- Today: operator types `ssh dl580 tmux attach -t ...` after deciding which host. Cognitive load on operator for every dispatch.
- Fabric: operator (or MAIN) files a task with capability requirements. Orchestrator picks the right session on the right host. Operator's cognitive load: zero.

### 5. Hardware-aware constraints

Fabric also captures hardware constraints:

| Constraint | How fabric enforces |
|---|---|
| Task needs 3090 (24GB VRAM) | `required_capabilities: ["gpu_3090"]` — only sessions on hosts with 3090 match |
| Task is CPU-heavy, don't run on laptop | `required_capabilities: ["heavy_cpu"]` — laptop sessions don't claim this capability |
| Task is GPU-heavy + Piper is training | Orchestrator queries `fabric.events` for `service:slatewick-lora-training:state=active`; if active, GPU 3 tasks blocked |
| Task is Cuttlefish AVD work | `required_capabilities: ["cuttlefish_avd"]` — R920 sessions match |
| Task is laptop control-plane work | `required_host: "laptop-jason-XPS-13"` — locks dispatch to specific host |

### 6. Presence + heartbeat

Every session heartbeats every 30s (sessions can declare longer cadence at registration). Missing 3 consecutive heartbeats:

- Session marked `status='paused'` 
- Active tasks revert to `status='pending'` (re-dispatchable)
- Operator notified via NTFY if any in-progress tasks were re-queued
- Session resuming: heartbeat resumes, status → `alive`, pending tasks become eligible again

**Concrete fix for tonight's pain**: D's listener died, nobody noticed for 4hr. Fabric: D's heartbeat would have stopped at minute 5, session marked paused at minute 5+90s, MAIN would have seen "D paused" in session list within 2 min, operator NTFY at minute 7.

### 7. Handoff between sessions

Sessions can transfer work explicitly:

```
mcp__fabric__session_handoff
  from: "B"
  to: "C"
  task_ids: ["uuid1", "uuid2"]
  summary: "Half-done WA registration; B handing to C while debugging cvd. Branch: wa-register-wip; last commit abc123."
```

Fabric:
- Updates task `assigned_to_letter` and `assigned_to_host`
- Writes a memo of type `task_handoff` recording the transfer
- Notifies C via pg_notify
- C sees handoff context with task list + summary

**Today**: handoffs are messy `kx-coord-send "B→C: please pick up X"` messages. C might miss it.

**Fabric**: handoff is a typed event with full context, can't be missed (pg_notify push + task list on C's session view).

### 8. Refusal-class auto-routing

When MAIN's banked rule (tonight) says "Claude refuses certain task classes → route to Qwen-local":

```
mcp__fabric__task_create
  title: "Audit signup flow for vulnerabilities"
  required_capabilities: ["security_analysis"]
  routing_hint: "claude_refusal_likely"
```

Orchestrator: sees `routing_hint=claude_refusal_likely` → picks a session running on a model that won't refuse (Qwen-local session) OR adds a routing_policy override in the task that says "skip Claude, go direct to local". Outcome recorded for learning ("Qwen handled this without refusal, success").

Over time: orchestrator learns from outcomes which task types Claude refuses + routes them appropriately on autopilot.

### 9. Cost-aware dispatch

Task creation can include cost constraints:

```
mcp__fabric__task_create
  title: "Compose 20 sales emails"
  required_capabilities: ["email_compose_sales"]
  max_cost_pence: 0   # local only
```

Orchestrator picks a session on a host with local LLMs (slatewick-lora available), bypassing Claude/Gemini entirely. Free.

```
mcp__fabric__task_create
  title: "Write critical compliance memo"
  required_capabilities: ["legal_drafting"]
  max_cost_pence: 10   # quality matters
  min_quality_score: 0.9
```

Orchestrator picks a session that can route to highest-quality backend (Gemini Pro / Claude Opus). Paid but high-quality.

### 10. Multi-host visualisation

Operator can ask at any time:

```
mcp__fabric__status_board

→ {
  "hosts": [
    {
      "name": "laptop-jason-XPS-13",
      "status": "online",
      "load": {"cpu_pct": 23, "ram_pct": 67},
      "sessions": [
        {"letter": "MAIN", "role": "orchestrator", "current_task": "monitoring", "heartbeat_age_s": 14},
        {"letter": "E", "role": "ops-maintenance", "current_task": "task #16: NAS migration", "heartbeat_age_s": 22}
      ]
    },
    {
      "name": "dl580",
      "status": "online",
      "load": {"cpu_pct": 67, "ram_pct": 38, "gpu_3090_0": 0, "gpu_3090_1": 89},
      "sessions": [
        {"letter": "B", "role": "messaging-channel-engineering", "current_task": "task #142: WA register debug", "heartbeat_age_s": 11},
        {"letter": "DL580-vanguard", "role": "vanguard-demo", "current_task": "task #98: 5 test emails", "heartbeat_age_s": 8}
      ]
    },
    {
      "name": "r920",
      "status": "online",
      "load": {"cpu_pct": 12, "ram_pct": 9},
      "sessions": [
        {"letter": "F", "role": "cuttlefish-avd-ops", "current_task": "task #134: cvd boot debug", "heartbeat_age_s": 19}
      ]
    }
  ],
  "tasks": {
    "pending": 4,
    "in_progress": 5,
    "blocked": 1,
    "completed_24h": 23
  },
  "demo_ready": "EMAIL✅ VOICE✅ WHATSAPP❌ VIDEO✅"
}
```

Single query, complete state of the system. Today this needs grep + ssh + tmux ls + journalctl + glance at chan.log.

## Concrete example — the orchestrator pattern in action

### Today (what happens during a Vanguard demo build)

1. Operator: "We need to ship Vanguard demo end-to-end tonight."
2. MAIN session in Claude Code sends `kx-coord-send` messages assigning A/B/C/D/E.
3. Each session reads coord, claims responsibility, starts work.
4. Sessions ssh into DL580 or R920 manually based on what their task needs.
5. Status comes back via `kx-coord-send`. MAIN tracks in their head.
6. Operator periodically reads coord channel to see progress.
7. When something blocks, operator picks it up.

**Cognitive load**: MAIN tracks ~15 things in their head. Operator tracks who's doing what.

### Tomorrow (with fabric orchestrator)

1. Operator: "We need to ship Vanguard demo end-to-end tonight."
2. MAIN files tasks: 
   - "Implement WA registration via cvd" req=[`wireprotocol_implementation`,`cuttlefish_avd`]
   - "Wire send_email natural-sender" req=[`email_channel_engineering`,`go_development`]
   - "Daily-checks pass on all services" req=[`ops_maintenance`,`health_monitoring`]
   - ...10 more tasks
3. Orchestrator dispatches each to the best session by capability+load match.
4. Sessions accept tasks; orchestrator tracks status.
5. Operator gets a single dashboard: `mcp__fabric__status_board`.
6. When a task blocks → fabric NTFY operator.
7. When all tasks → fabric NTFY "Vanguard demo ready".

**Cognitive load**: operator looks at one dashboard. MAIN doesn't track in head — fabric tracks.

## The federation model recap

Three hosts. Each runs `kronaxis-fabric` daemon. All share state via central PG on DL580. Local read-through cache. pg_notify for invalidation.

```
           ┌─ Operator looks here ─┐
           │                       │
           ▼                       ▼
      ┌──────────────┐      ┌──────────────┐
      │  Laptop      │      │  R920        │
      │  fabric      │      │  fabric      │
      │  ┌────────┐  │      │  ┌────────┐  │
      │  │ MAIN   │  │      │  │ F      │  │
      │  └────────┘  │      │  └────────┘  │
      │  ┌────────┐  │      │              │
      │  │ E      │  │      │              │
      │  └────────┘  │      │              │
      └──────┬───────┘      └──────┬───────┘
             │                     │
             │ pg LISTEN/NOTIFY    │
             │ + writes go central │
             └───────┬─────────────┘
                     │
                     ▼
              ┌──────────────┐
              │  DL580       │
              │  fabric      │
              │  + Postgres  │  ◄── canonical state
              │  (fabric.*)  │
              │  ┌────────┐  │
              │  │ A      │  │
              │  ├────────┤  │
              │  │ B      │  │
              │  ├────────┤  │
              │  │ DL580- │  │
              │  │ vangd  │  │
              │  └────────┘  │
              └──────────────┘
```

## Failure modes specific to multi-host

| Failure | Detection | Behaviour |
|---|---|---|
| Single host fabric daemon crashes | Heartbeat absence on `fabric.sessions` | Sessions revert to pending; other hosts continue |
| WireGuard tunnel down between laptop ↔ DL580 | pg connection drops on laptop daemon | Laptop reads from cache; writes queue locally; recovers on reconnect |
| Central PG (DL580) down | All daemons see pg connection errors | All reads from cache; writes queue; operator NTFY |
| Session letter collision (e.g. two Es on same host) | INSERT conflict on `(letter, host)` | New session refused with error; operator picks different letter |
| Stale capability claim (session declared `cycles_render` but service down) | Task dispatched but execution fails | Task reverts to pending; session demoted in capability via outcome learning |

## Open questions for week 5 implementation

1. **Capability taxonomy**: who maintains it? v1 = operator-curated in source; v2 = fabric.capabilities table editable via MCP.
2. **Task priority semantics**: simple numeric (1-10) or named tiers (low/medium/high/critical)?
3. **Stale task semantics**: if a task is in_progress for >N minutes without status update, what happens?
4. **Cross-task dependencies**: task B depends on task A — does B wait, or get queued in a chain?
5. **Manual override**: operator can force-assign a task to a specific session, bypassing capability check (escape hatch).
6. **Handoff vs reassignment**: handoff = explicit voluntary transfer; reassignment = orchestrator decides. Both supported. Different audit trail.

## See also

- [`01-architecture-overview.md`](01-architecture-overview.md) — Schema for sessions/hosts/tasks
- [`03-ipc-design.md`](03-ipc-design.md) — How federation transport works
- [`05-router-maximisation.md`](05-router-maximisation.md) — How routing pairs with multi-host dispatch
- [`06-shipping-plan.md`](06-shipping-plan.md) — Week 5 orchestrator deliverables
