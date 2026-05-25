# 05 — Maximisation with kronaxis-router

## The premise

Kronaxis-router today is a cost-routing proxy: it knows which model to call based on **static config rules**. Fabric turns it into a **learning system**: every routing decision is data-backed, every outcome is recorded, and the policy improves over time without human-in-the-loop tuning.

**The pairing is the novel piece.** Vector search, memory daemons, graph queries all exist as standalone tools. **A semantic-memory layer driving a cost-routing proxy is something nobody has shipped.**

## The bare contract

Two endpoints define the pairing:

### 1. Router → Fabric: "best backend for this task"

```
GET /v1/routing/best_backend
    ?task_class=code_gen_go
    &max_cost_pence=5
    &min_success_rate=0.7
    &latency_max_ms=10000

→ HTTP 200
{
  "backend": "qwen-32b-local",
  "reasoning": "best historic success/cost ratio in task_class over last 30d",
  "expected_cost_pence": 0.0001,
  "expected_success_rate": 0.92,
  "expected_duration_ms": 1450,
  "fallback_backend": "gemini-flash",
  "confidence": 0.85,
  "decision_id": "uuid-for-tracking"
}
```

**What fabric does internally**:
```sql
SELECT 
    backend,
    AVG(CASE WHEN success THEN 1.0 ELSE 0.0 END) AS success_rate,
    AVG(cost_pence) AS avg_cost,
    AVG(duration_ms) AS avg_duration,
    COUNT(*) AS sample_size,
    EXP(-(NOW() - MAX(ts))::interval / interval '7 days') AS recency_weight
FROM fabric.task_outcomes
WHERE task_class = $1
  AND ts > NOW() - interval '30 days'
GROUP BY backend
HAVING COUNT(*) >= 5  -- minimum sample size
  AND AVG(cost_pence) <= $2
  AND AVG(CASE WHEN success THEN 1.0 ELSE 0.0 END) >= $3
ORDER BY (success_rate * recency_weight) / NULLIF(avg_cost, 0) DESC
LIMIT 1;
```

**Latency**: 1-10ms (PG query, indexed).

**Caching**: result cached for 60s per (task_class, max_cost_pence, min_success_rate) combo. Inval'd on new outcome write to that task_class.

### 2. Router ← Fabric: "record this outcome"

```
POST /v1/routing/outcome
{
  "decision_id": "uuid-from-best_backend-response",
  "request_id": "router-internal-uuid",
  "task_class": "code_gen_go",
  "backend": "qwen-32b-local",
  "success": true,
  "cost_pence": 0.0001,
  "duration_ms": 1450,
  "quality_score": 0.85,        // optional, from validator
  "model_outputs_hash": "sha256:abc...",  // for dedup / cache
  "metadata": {
    "prompt_tokens": 1200,
    "completion_tokens": 350,
    "temperature": 0.6,
    "validator": "human-thumbs-up"
  }
}

→ HTTP 200
{"recorded": true, "outcome_id": "uuid"}
```

**What fabric does internally**: single INSERT into fabric.task_outcomes. Invalidates relevant routing caches. Updates rolling backend stats if needed.

**Latency**: 5ms (PG insert).

## The learning loop in concrete terms

### Day 1 (cold start)

- Operator manually seeds: "For code_gen_go, default to qwen-32b-local, fallback gemini-flash". Stored as `policy_seed` entries in fabric.task_outcomes with synthetic outcomes.
- Router queries fabric → returns seeded recommendation.
- Router dispatches to qwen-32b-local.
- Outcome recorded.

### Day 7 (initial data)

- ~50 outcomes per task_class.
- Fabric switches from seeded recommendation to data-backed: "qwen-32b-local has 94% success rate in 50 trials".
- Recommendation now has confidence interval.

### Day 30 (mature)

- ~500 outcomes per task_class.
- Fabric identifies edge cases: "task_class=code_gen_go with content containing 'gRPC' has 60% success on qwen-32b-local but 95% on gemini-pro. Demote qwen for this sub-class."
- Sub-class detection via embedding similarity on the task prompts themselves.

### Day 90 (system learns)

- Fabric autonomously adjusts backend rankings based on:
  - Recent quality drops on a backend (model regression detection)
  - Cost increases (Gemini quota tier changes mid-month)
  - New backends added to router (auto-A/B-tested against incumbents)
- Operator sees a weekly report: "Last week, fabric demoted X and promoted Y for class Z, saving £A".

## Cost savings — real numbers

### Today's pattern (rough estimate)

| Backend | Usage | Cost/call | Calls/day | £/day |
|---|---|---|---|---|
| Gemini 2.5 Flash | ~60% of LLM calls | £0.0001 | 600 | £0.06 |
| Gemini 2.5 Pro | ~10% | £0.001 | 100 | £0.10 |
| Local Qwen | ~20% | £~0 (electricity) | 200 | £0 |
| Imprint 27B | ~5% | £~0 | 50 | £0 |
| slatewick-lora | ~5% | £~0 | 50 | £0 |
| **Total daily LLM cost** | | | 1000 | **£0.16** |
| **Yearly** | | | | **£58** |

Modest. Most use is already free (local). The bigger savings are quality, not raw cost.

### With fabric routing

Same volume, but fabric:
- Demotes Gemini calls that didn't need it (~30% of Flash calls could run on local)
- Promotes local Qwen when historical quality matches
- Catches Gemini 429 quota events early, automatic failover

| Backend | Usage | Cost/call | Calls/day | £/day |
|---|---|---|---|---|
| Gemini 2.5 Flash | ~30% | £0.0001 | 300 | £0.03 |
| Gemini 2.5 Pro | ~5% (only when truly needed) | £0.001 | 50 | £0.05 |
| Local Qwen | ~50% | £0 | 500 | £0 |
| Imprint 27B | ~10% | £0 | 100 | £0 |
| slatewick-lora | ~5% | £0 | 50 | £0 |
| **Total daily** | | | 1000 | **£0.08** |
| **Yearly** | | | | **£29** |

**Direct API savings: ~£29/year.** Modest in absolute terms. But:

- That's 50% cost reduction
- It scales linearly: if Kronaxis grows 10× (more sessions, more personas), savings become £290/year, then £2,900
- And every backend regression catch saves operator debug time + bad-output recovery

The non-cost wins:

- **Quality consistency**: backend drift detected automatically
- **Quota resilience**: Gemini hits quota → fabric auto-promotes Qwen until window passes
- **Operator confidence**: every dispatch traceable to a data-backed decision
- **Cross-session learning**: session #1's bad outcome saves session #1000 from repeating

## The quality_score angle

Most routing systems only measure cost + success-rate (200 OK or not). Fabric supports `quality_score` — a 0-1 number from a validator.

Validators in v1:

1. **Schema validators** — for structured outputs (JSON parses correctly = 1.0; otherwise lower based on partial match)
2. **Length validators** — output meets requested length ±20% = 1.0; else penalty
3. **Pattern validators** — required phrases present = 1.0; else penalty
4. **Human thumbs up/down** — operator marks outputs in UI; thumbs-up = 1.0
5. **Downstream success** — e.g. email_compose_sales → did the email get a reply? Backward propagate as quality signal

Without validators, success-rate is just "did the LLM respond?" — basically always 1.0 for working models. With validators, quality becomes measurable.

**Recommended v1**: schema validators (easy) + thumbs in Claude Code (free via MCP). Get richer validators later.

## How fabric pairs with multi-host orchestration

The pairing extends beyond routing for LLM choice:

```
MAIN orchestrator (laptop) needs code-gen task done:

  mcp__fabric__task_create
    title: "Implement fabric.search semantic ranker"
    required_capabilities: ["go_development", "pgvector_knowledge"]
    estimated_duration_min: 30
    
  → fabric.tasks row created
  → fabric.orchestrator picks best session:
    - Filter by required_capabilities
    - Rank by current load + host capabilities
    - Sessions on DL580 prefer over R920 if both qualify (CPU/RAM heavier task)
  → task assigned to Session B on DL580
  → notification to B via coord channel
  → B accepts, fabric updates status
  
  B starts work:
    B calls mcp__fabric__routing_best_backend ?task_class=code_gen_go
    → fabric: "qwen-32b-local"
    → B dispatches to router → router → qwen-32b-local
    → outcome recorded
    → B reviews + commits
    → B calls mcp__fabric__task_update status=done
```

**Two levels of optimal selection**:
1. **Task → Session**: pick the right worker session for the work (capability + load match)
2. **Task → Backend**: pick the right LLM for the LLM-side of the work (cost + quality + speed match)

Both data-backed. Both learning.

## Anti-patterns (what NOT to do)

### Anti-pattern 1: route everything through fabric

Don't. Fabric is the **decision** layer. Routing transport stays in kronaxis-router. Don't proxy LLM responses through fabric — that adds latency for no value.

### Anti-pattern 2: synchronous outcome recording

Don't block the user's response on outcome recording. Always async (background goroutine on router side).

### Anti-pattern 3: per-call fabric reads on hot paths

If router queries fabric on every single LLM call, that's a fabric call per LLM call. Cache routing decisions for 60s per task_class. 95% cache hit rate easy.

### Anti-pattern 4: ignoring sample size

A backend with 2 successes and 0 failures isn't "100% success" — it's "insufficient sample". Use Wilson confidence intervals OR minimum sample size threshold (default 5).

## Operator-facing tooling

For the operator to understand what fabric+router is doing:

```
mcp__fabric__routing_report
  --task_class email_compose_sales
  --window 7d
  
→ Returns:
{
  "task_class": "email_compose_sales",
  "total_calls": 234,
  "by_backend": [
    {
      "backend": "slatewick-lora-v9",
      "calls": 156,
      "success_rate": 0.94,
      "avg_cost_pence": 0,
      "avg_duration_ms": 1200,
      "quality_score_p50": 0.85
    },
    {
      "backend": "gemini-2.5-flash",
      "calls": 78,
      "success_rate": 0.92,
      "avg_cost_pence": 0.0001,
      "avg_duration_ms": 800,
      "quality_score_p50": 0.83
    }
  ],
  "currently_recommended": "slatewick-lora-v9",
  "rationale": "Similar quality, zero marginal cost"
}
```

Daily summary delivered via NTFY at 09:00:

```
[fabric/routing daily summary]
1247 calls routed over last 24h
Saved ~£0.32 vs static "always Gemini" baseline
3 backends demoted (low recent quality):
  - imprint-27b on persona_voice_reply (0.62 → 0.45 quality)
2 backends promoted (proven local-equiv):
  - qwen-32b-local on code_gen_go (94% success at zero cost)
Next review: 2026-05-26 09:00
```

## The bigger picture

Fabric+router pairing turns LLM routing from a **vibe-based config file** into a **data-backed adaptive system**. This is what makes Kronaxis's operator workflow genuinely different from "person + Claude + Cursor + local models" patterns elsewhere:

- Other systems pick a backend per use case statically
- Fabric+router pick per **task class instance** dynamically
- And the system gets better at picking without anyone tuning it

That's the differentiator. That's the **product story** if fabric ever goes BSL-1.1 public.

## See also

- [`02-token-efficiency.md`](02-token-efficiency.md) — Token cost angle
- [`07-orchestrator-multihost.md`](07-orchestrator-multihost.md) — How routing fits with multi-host dispatch
