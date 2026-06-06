-- Migration: 20260527_002_per_tenant_template.sql
-- Reference DDL applied to every new tenant schema. NOT executed directly —
-- fabric runs this template (parameterised on schema name) via Go's
-- text/template inside the provisioning code (see cmd/fabric/meta.go).
--
-- This file is the canonical reference. Keep in sync with the Go template
-- string `perTenantSchemaTpl` in cmd/fabric/meta.go.
--
-- Schema name validator: ^tenant_[a-f0-9]{8}(_[0-9]+)?$ enforced in Go before
-- interpolation. Do NOT take a schema name from request input at runtime.

-- {{.Schema}} is replaced by the validated schema name.

CREATE SCHEMA IF NOT EXISTS {{.Schema}};

-- ---------- memos ----------
CREATE TABLE IF NOT EXISTS {{.Schema}}.memos (
  id BIGSERIAL PRIMARY KEY,
  title TEXT NOT NULL DEFAULT '',
  content TEXT NOT NULL,
  type TEXT NOT NULL DEFAULT 'general',
  tags TEXT[] NOT NULL DEFAULT '{}',
  sha256 TEXT NOT NULL,
  tsv tsvector GENERATED ALWAYS AS (
    setweight(to_tsvector('english', coalesce(title,'')), 'A') ||
    setweight(to_tsvector('english', content), 'B')
  ) STORED,
  embedding vector(768),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at TIMESTAMPTZ,
  -- v0.11 (2026-06-06): optional upsert_key for supersession semantics.
  -- POST /v1/memo with upsert_key= ON CONFLICT (upsert_key) DO UPDATE.
  upsert_key TEXT,
  CONSTRAINT memos_sha256_uniq UNIQUE (sha256)
);

CREATE INDEX IF NOT EXISTS memos_tsv_idx
  ON {{.Schema}}.memos USING gin (tsv);
CREATE INDEX IF NOT EXISTS memos_type_idx
  ON {{.Schema}}.memos (type) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS memos_created_idx
  ON {{.Schema}}.memos (created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS memos_embedding_ivf
  ON {{.Schema}}.memos USING ivfflat (embedding vector_cosine_ops)
  WITH (lists = 100);
-- Unique partial index on upsert_key so each non-null key has at most one
-- live row at a time. Supersession primitive for "CURRENT STATE" memos.
CREATE UNIQUE INDEX IF NOT EXISTS memos_upsert_key_uniq
  ON {{.Schema}}.memos (upsert_key)
  WHERE upsert_key IS NOT NULL AND deleted_at IS NULL;

-- ---------- prospect_interactions (persona-only; harmless on others) ----------
CREATE TABLE IF NOT EXISTS {{.Schema}}.prospect_interactions (
  id BIGSERIAL PRIMARY KEY,
  prospect_id TEXT NOT NULL,
  call_id TEXT NOT NULL,
  turn_idx INT NOT NULL,
  vertical TEXT NOT NULL DEFAULT '',
  stage TEXT NOT NULL DEFAULT '',
  user_text TEXT,
  agent_text TEXT NOT NULL,
  outcome TEXT NOT NULL DEFAULT '',
  latency_ms INT NOT NULL DEFAULT 0,
  embedding vector(768),
  memo_id BIGINT REFERENCES {{.Schema}}.memos(id) ON DELETE SET NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS prospect_interactions_prospect_idx
  ON {{.Schema}}.prospect_interactions (prospect_id, created_at DESC);
CREATE INDEX IF NOT EXISTS prospect_interactions_call_idx
  ON {{.Schema}}.prospect_interactions (call_id, turn_idx);
CREATE INDEX IF NOT EXISTS prospect_interactions_embedding_ivf
  ON {{.Schema}}.prospect_interactions USING ivfflat (embedding vector_cosine_ops)
  WITH (lists = 50);
