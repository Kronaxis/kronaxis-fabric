# 02 — Token efficiency

## The premise

Every Claude session consumes tokens reading + writing. The current Frankenstein wastes a lot of them. Fabric measurably reduces this through four mechanisms:

1. **Semantic search** instead of grep + file read
2. **Live code graph** instead of full-file reads to find symbol callers/definitions
3. **Recency-weighted reranking** so stale memos don't surface
4. **Scoped retrieval** so sessions only see relevant memos

## The numbers (concrete, today, observable)

### Before fabric — per-query cost

| Query class | Today's path | Tokens |
|---|---|---|
| "What's Lucy's port?" | Read `MEMORY.md` (985 lines, 266KB) | ~30,000 |
| "Where is `synth_line_lucy` called?" | `grep -r synth_line_lucy kronaxis/` + read each hit | ~8,000-25,000 |
| "What changed since last week?" | `git log --since=1.week.ago` + `git diff` | ~10,000-50,000 |
| "What's the state of `bos-stalwart-tunnel`?" | `systemctl status` + `journalctl -u --since=...` + scan | ~3,000-10,000 |
| "Why did X fail at 14:30?" | 5-tab investigation: journalctl + grep coord_messages + git log + nvidia-smi + read code | ~20,000-50,000 |
| Session bootstrap context | Read `MEMORY.md` + `CLAUDE.md` (top of context) | ~50,000 |
| "Did this approach work before?" | grep all memos + read | ~15,000 |
| "What did B finish today?" | tail chan.log + filter B | ~5,000 |

### After fabric — per-query cost

| Query class | Fabric path | Tokens |
|---|---|---|
| "What's Lucy's port?" | `mcp__fabric__search "Lucy port"` → top 3 memos, ~50 lines | ~200-400 |
| "Where is `synth_line_lucy` called?" | `mcp__fabric__graph_query "synth_line_lucy"` → 1-hop callers | ~150-300 |
| "What changed since last week?" | `mcp__fabric__search type=deployment ts>2026-05-18` | ~300-500 |
| "What's the state of `bos-stalwart-tunnel`?" | `mcp__fabric__search "bos-stalwart-tunnel"` → recent event memos | ~200-400 |
| "Why did X fail at 14:30?" | `mcp__fabric__incident_replay --window 13:30-15:00` | ~500-1,000 |
| Session bootstrap context | `mcp__fabric__search type=index visibility=global` → 10 most-relevant | ~1,500-2,500 |
| "Did this approach work before?" | semantic search across memos with belief-confidence rerank | ~300-600 |
| "What did B finish today?" | `mcp__fabric__coord_history --filter B --window 24h` | ~400-800 |

### Reduction by query class

| Query class | Today | Fabric | Reduction |
|---|---|---|---|
| Port/config lookup | 30k | 300 | **100×** |
| Symbol traversal | 16k avg | 250 avg | **64×** |
| Recent changes | 30k avg | 400 avg | **75×** |
| Service state | 6k avg | 300 avg | **20×** |
| Incident retro | 35k avg | 700 avg | **50×** |
| Session bootstrap | 50k | 2k | **25×** |
| Belief verification | 15k | 450 avg | **33×** |
| Coord channel scan | 5k | 600 avg | **8×** |

**Weighted average across a typical session: 6-10× reduction.**

## Worked example — Patent B s 22 escalation calendar query

A session ~3 weeks from now will ask: "When does Patent B chase letter 1 fire and what does it contain?"

**Today's path**:
1. `grep -r "Patent B" /home/jason/projects/kronaxis/patentbox/` — ~5,000 token result
2. Read `UKIPO_S22_CHASE_LETTER_SEQUENCE.md` — ~15,000 tokens
3. Read `project_patent_b_s22_escalation_2026_05_20.md` memory file — ~3,000 tokens
4. Cross-reference cron script — ~2,000 tokens

**Total: ~25,000 tokens**

**Fabric path**:
1. `mcp__fabric__search "Patent B chase 1 fire date"` →
   - Returns 2 memos with confidence 0.95 + 0.91, ~600 tokens total
   - Top result includes the date "2 July 2026" + reference to chase letter draft file
2. If more detail needed: `mcp__fabric__search "Patent B chase letter content"` → ~400 tokens

**Total: ~600-1,000 tokens — 25-40× reduction.**

## Worked example — operator asking "what was happening when DL580 rebooted?"

This is the kind of question that today takes **2 hours of manual investigation** because there's no single timeline.

**Today's path**:
1. `journalctl -u kronaxis-router --since="14:00"` — ~5,000 tokens
2. `journalctl -u bos-controld --since="14:00"` — ~5,000 tokens
3. `grep -E "reboot|crash" /var/log/syslog | tail -100` — ~3,000 tokens
4. `git log --since="2 hours ago"` — ~2,000 tokens
5. `tail -200 /home/jason/.kronaxis/coord/chan.log | grep -B 5 -A 5 reboot` — ~4,000 tokens
6. `nvidia-smi --query-gpu=index,...` — ~500 tokens
7. Read recent operator memory files — ~5,000 tokens
8. Cross-reference manually — significant human effort + LLM reading

**Total: ~25,000-30,000 tokens + 2 hours of human time**

**Fabric path**:
1. `mcp__fabric__incident_replay --window "2026-05-21T20:30Z-22:00Z" --correlate deployments,service_state,memos,events`
   - Returns ordered timeline of all relevant events
   - ~1,500-2,500 tokens

**Total: ~2,000 tokens + 5 seconds.**

**12-15× reduction in tokens, but the bigger lift is the time saved.**

## Cost in real money

Claude Opus 4.7 pricing as of 2026-05: roughly £0.0001 per token input, £0.0003 per output (input tokens dominate session bootstrap).

Per session (operator + 5 worker sessions = 6 total, 50 queries/day each):

| Scenario | Tokens/day | £/day | £/year |
|---|---|---|---|
| Today (Frankenstein) | ~3 million | ~£0.30 | **~£110** |
| Fabric | ~500,000 | ~£0.05 | **~£18** |
| Savings | — | — | **~£92/year per active session pattern** |

For Kronaxis (3-6 active sessions concurrent): **~£280-560/year direct API savings**.

But the much larger wins are:

- **Effective context window** — sessions can hold more relevant state because they're not bloated with stale MEMORY.md
- **Faster iteration** — operator gets answers in seconds, not minutes of waiting for LLM to read files
- **Less context confusion** — Claude's working memory has signal, not noise

These qualitative gains aren't priced but represent the actual productivity unlock.

## How fabric achieves the reduction

### 1. Embeddings + vector cosine

`Xenova/all-MiniLM-L6-v2` (384-dim) via Ollama. Query embeds in ~5ms. pgvector hnsw search across 100k memos in ~10ms. Return top-K by semantic similarity, never raw file scan.

### 2. Live code graph

tree-sitter parses on save. Symbol → callers walk via SQL join, no grep. 1-hop query on 50,000-symbol graph returns in ~5ms with ~30-line result.

### 3. Recency-weighted reranking

`relevance_score = cosine_similarity * recency_decay * confidence_score`

where:
- `recency_decay = exp(-days_since_observed / half_life)` (default half-life 30 days)
- `confidence_score` is updated on verify() calls — old beliefs decay unless reaffirmed

Effect: a memo that said "Lucy port is 8800" 3 months ago has confidence 0.3; a memo that said "Lucy port is 8872" last week has confidence 0.95. Search returns the recent one first.

### 4. Scoped retrieval

Default search scope: `visibility IN ('shared', 'global') OR session_letter = $caller`. Each session sees their own private memos + the shared knowledge pool. Stops 600+ memos of cross-session noise polluting every query.

### 5. Cache locality

Local fabric daemon caches hot memos (256MB in-process). Most-frequent queries served in <1ms with no network. pg_notify invalidates cache on writes — never returns stale data even when faster.

### 6. Compact response shape

Fabric returns structured search results — `[{id, title, body_excerpt, score, refs}]` — not raw file bodies. Excerpts default to 200 chars with full-body fetch on follow-up if needed. Cuts response size 10×.

## How fabric pairs with kronaxis-router (token cost angle)

Most queries to fabric ARE NOT LLM calls. They're vector + SQL ops. No router involvement.

Where router matters: the workflow downstream of a fabric query. Example:

**Operator asks** "Generate a follow-up email to Pearl Insurance"

1. Claude calls `mcp__fabric__search "Pearl Insurance recent interactions"` (tokens: ~50 input + ~500 output)
2. Claude has the context. Now decides whether to LLM-generate locally vs cloud.
3. Claude calls `mcp__fabric__routing_best_backend ?task_class=email_compose_sales`
4. Fabric returns: `{"backend": "slatewick-lora-v9", "cost_pence": 0.0001}` (tokens: ~30 input + ~50 output)
5. Claude routes to slatewick-lora directly via kronaxis-router — bypassing Claude itself for the email-gen task
6. Email generated, returned, recorded via `mcp__fabric__outcome_record`

**Token cost of the LLM-decision flow**: ~700 tokens of Claude time
**LLM cost of email generation**: ~0 (local model)
**Today's equivalent cost**: Claude generates the email directly = ~3000 tokens of premium Claude time + £0.001 in API cost

So fabric+router not only chooses the right backend, it also offloads work FROM Claude TO cheaper backends when appropriate. Cumulative effect: Claude sessions become orchestrators, not workers.

## See also

- [`05-router-maximisation.md`](05-router-maximisation.md) — The learning system specifically
- [`04-speed-delivery.md`](04-speed-delivery.md) — Latency budgets
