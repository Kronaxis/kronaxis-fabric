# 10 — Observability

## What "observable" means here

A small daemon, used daily, deployed on hardware the operator owns. Observability for fabric is not about distributed tracing across a hundred microservices. It is about answering four questions quickly when something feels wrong:

1. Is the daemon up and serving?
2. Is it slow, and if so, where?
3. Are clients hitting errors, and which ones?
4. What did it do in the last hour?

Everything below serves one of those four questions. Nothing here exists to be impressive on a dashboard screenshot.

## Three signals: metrics, logs, traces

### Metrics — Prometheus exposition

`GET /metrics` returns a Prometheus text exposition. Cardinality is kept low on purpose — no per-user, no per-memo, no per-request labels.

```
# HELP fabric_http_requests_total Total HTTP requests served
# TYPE fabric_http_requests_total counter
fabric_http_requests_total{endpoint="/v1/memo/search",method="POST",status="200"} 1247
fabric_http_requests_total{endpoint="/v1/memo/search",method="POST",status="429"} 3
fabric_http_requests_total{endpoint="/v1/memo/remember",method="POST",status="200"} 412

# HELP fabric_http_request_duration_seconds Request duration histogram
# TYPE fabric_http_request_duration_seconds histogram
fabric_http_request_duration_seconds_bucket{endpoint="/v1/memo/search",le="0.005"} 920
fabric_http_request_duration_seconds_bucket{endpoint="/v1/memo/search",le="0.010"} 1180
fabric_http_request_duration_seconds_bucket{endpoint="/v1/memo/search",le="0.050"} 1244
fabric_http_request_duration_seconds_bucket{endpoint="/v1/memo/search",le="+Inf"} 1247

# HELP fabric_embed_call_duration_seconds Time spent waiting on the embedding model
# TYPE fabric_embed_call_duration_seconds histogram
fabric_embed_call_duration_seconds_bucket{le="0.050"} 1100
fabric_embed_call_duration_seconds_bucket{le="0.250"} 1247
fabric_embed_call_duration_seconds_bucket{le="+Inf"} 1247

# HELP fabric_pg_pool_in_use Connections currently checked out of the pgx pool
# TYPE fabric_pg_pool_in_use gauge
fabric_pg_pool_in_use 3
fabric_pg_pool_idle 7

# HELP fabric_sse_streams Active Server-Sent Event streams
# TYPE fabric_sse_streams gauge
fabric_sse_streams 4

# HELP fabric_memos_total Memo count by type
# TYPE fabric_memos_total gauge
fabric_memos_total{type="reference"} 4218
fabric_memos_total{type="project"} 1109
fabric_memos_total{type="log"} 18420
```

A single Prometheus instance scrapes every fabric daemon every 15s. Storage is on whichever box runs Prometheus. The operator already runs node_exporter on every host; adding fabric is one scrape config block.

### Logs — structured JSON to stderr

Every log line is a single JSON object on stderr. systemd captures stderr to journald. No log files on disk.

```json
{"ts":"2026-05-25T13:42:11.034Z","level":"info","msg":"memo.search","request_id":"01J5W…","tenant":"default","query_hash":"sha256:…","top_k":3,"hits":3,"duration_ms":11}
{"ts":"2026-05-25T13:42:14.221Z","level":"warn","msg":"embed.slow","request_id":"01J5W…","duration_ms":840,"threshold_ms":500}
{"ts":"2026-05-25T13:42:18.011Z","level":"error","msg":"pg.pool.exhausted","request_id":"01J5W…","in_use":10,"max":10,"wait_ms":2500}
```

Required fields on every line: `ts`, `level`, `msg`. `request_id` appears on every line that came from an HTTP request. Free-form fields are allowed but discouraged; if you need a new field on more than three log lines, give it a name in the structured schema.

Querying:

```
journalctl -u kronaxis-fabric --since '1 hour ago' -o cat | jq -c 'select(.level == "error")'
```

That single pipe answers question four in under a second.

### Traces — OpenTelemetry, off by default

OTLP exporter ships in the binary but is disabled unless `--otlp-endpoint` is set. When on, every HTTP request becomes a root span with child spans for `pg.query`, `embed.call`, and `sse.write`. Useful when chasing a tail-latency regression; pure overhead the rest of the time.

The operator runs nothing for tracing today and is not required to start. Doc 10 mentions it so the option is documented, not so it becomes a step in any runbook.

## Health checks

| Endpoint | Liveness or readiness | What it verifies |
|---|---|---|
| `/healthz` | Liveness | Process responds at all |
| `/readyz` | Readiness | DB ping under 100ms, embed model ping under 500ms |

systemd's `WatchdogSec=30` calls `/healthz` every 15s. Failure → unit restart. Readiness controls whether load-balancers send traffic; for single-instance deployments it just feeds the operator's dashboard.

### Readyz payload

```
GET /readyz

{
  "ok": true,
  "checks": {
    "db":    {"ok": true, "latency_ms": 2},
    "embed": {"ok": true, "latency_ms": 38, "model": "all-minilm"},
    "disk":  {"ok": true, "free_gb": 1240.4}
  },
  "uptime_s": 84210,
  "version": "0.1.3"
}
```

Any check failing flips `ok` to false and changes the response to 503. The operator's existing daily-checks pipeline already parses this shape from other services; fabric reuses the same field names.

## Dashboards

One Grafana dashboard, six panels, lives in `ops/grafana/fabric.json`:

1. **Request rate per endpoint** — stacked area, last 6h.
2. **Latency p50/p95/p99 per endpoint** — three lines, last 6h.
3. **Error rate** — `5xx + 4xx` as a percentage of total, last 24h.
4. **Embed call duration** — single histogram, last 1h.
5. **PG pool in_use vs idle** — two stacked gauges, instant.
6. **Memo count by type** — bar chart, instant.

Anything beyond these six panels is YAGNI for a single-daemon deployment. When a real second instance lands, a "by-instance" repeat row on every panel gives free per-host breakdowns without redesign.

## Alerts

| Alert | Condition | Channel |
|---|---|---|
| Daemon down | `up{job="fabric"} == 0` for 2 min | NTFY topic `fabric-ops` |
| Error rate elevated | `5xx / total > 0.02` over 10 min | NTFY topic `fabric-ops` |
| p99 search latency | `histogram_quantile(0.99, fabric_http_request_duration_seconds_bucket{endpoint=~".*memo/search.*"}) > 0.1` over 10 min | NTFY topic `fabric-ops` |
| Embed model slow | p95 `fabric_embed_call_duration_seconds > 0.5` over 10 min | NTFY topic `fabric-ops` |
| Disk low | `node_filesystem_avail_bytes{mountpoint="/var/lib/postgresql"} < 50e9` | NTFY topic `fabric-ops` |

Alerts route through the existing Alertmanager → NTFY pipeline. No new tooling.

Volume target: under five alerts per week in steady state. More than that and either the threshold is wrong or there is a real problem; both warrant attention.

## Audit log

Every mutation is recorded in `fabric.audit_log` in addition to its primary table:

```sql
CREATE TABLE fabric.audit_log (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  ts           timestamptz NOT NULL DEFAULT now(),
  tenant_id    text NOT NULL,
  actor_token  text NOT NULL,            -- sha256 of the bearer token
  action       text NOT NULL,            -- e.g. 'memo.remember', 'memo.delete'
  target_kind  text NOT NULL,            -- 'memo', 'event_channel'
  target_id    text NOT NULL,
  request_id   text NOT NULL,
  prev_hash    text NOT NULL,            -- sha256 of previous row's hash field
  hash         text NOT NULL             -- sha256 of (prev_hash || canonical_json(this row))
);
```

The `prev_hash`/`hash` columns form a chain. `fabric audit verify` walks the chain and confirms no rows were edited or deleted out of band. This pattern is already in use elsewhere in the operator's BoS code; fabric reuses the implementation rather than inventing one.

Audit log retention defaults to 365 days. Operators can extend or compress.

## Capacity signals

The dashboards already show what is happening. These metrics specifically inform "do I need to add capacity":

- `fabric_pg_pool_in_use` regularly hitting `max`: bump pool size or add a read replica.
- `fabric_embed_call_duration_seconds` p95 climbing without code change: the embed model host is under load.
- `fabric_memos_total{type="log"}` growing faster than disk grows: consider TTL'ing log memos older than N days.
- `process_resident_memory_bytes` (from the standard process collector) crossing 1 GiB: investigate. Idle RSS should sit around 150 MiB.

None of these need an alert. The operator looks at the dashboard once a week and notices.

## Runbook integration

Every alert above has a one-paragraph runbook entry in `ops/runbooks/fabric.md`. The alert's NTFY message includes the runbook anchor:

```
[fabric-ops] Embed model slow (p95 0.72s > 0.5s for 10m)
Runbook: ops/runbooks/fabric.md#embed-slow
```

The runbook entry gives:
- What this means in plain English.
- Three things to check first.
- The one command that resolves the common cause.
- When to escalate to the operator.

A fresh hand on the system fixes the common case in five minutes without paging anyone.

## What is intentionally absent

- **No log aggregation pipeline.** journald + `jq` covers single-host. If multi-host fabric ever needs it, we add Loki later — additive change, no schema rework.
- **No anomaly detection.** Threshold alerts catch real failures. Anomaly detection on a small system adds noise without value.
- **No per-request traces sampled at 100%.** Traces are off by default; enable when investigating a specific regression, then disable.
- **No custom UI.** Grafana for graphs, journald for logs, `psql` for the audit table. Three tools the operator already knows.

## See also

- [`06-shipping-plan.md`](06-shipping-plan.md) for which week each metric and dashboard lands.
- [`11-error-model.md`](11-error-model.md) for the error codes that appear in logs and alerts.
- [`12-roadmap.md`](12-roadmap.md) for observability items deferred past v1.
