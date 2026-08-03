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
  -- 006 (2026-08-02): provenance (poisoning defence) + world valid-time.
  author_session TEXT,
  write_source TEXT,
  trust_tier SMALLINT NOT NULL DEFAULT 1,
  valid_from TIMESTAMPTZ,
  valid_to TIMESTAMPTZ,
  -- 007 (2026-08-02): write-time admission quarantine.
  quarantined BOOLEAN NOT NULL DEFAULT false,
  quarantine_reason TEXT,
  -- 008 (2026-08-04): trusted memo flagged by the contradiction judge.
  contested BOOLEAN NOT NULL DEFAULT false,
  contested_reason TEXT,
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
CREATE INDEX IF NOT EXISTS memos_valid_time_idx
  ON {{.Schema}}.memos (valid_from, valid_to) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS memos_trust_idx
  ON {{.Schema}}.memos (trust_tier) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS memos_quarantined_idx
  ON {{.Schema}}.memos (created_at DESC) WHERE quarantined = true AND deleted_at IS NULL;

-- ---------- memo_versions: bitemporal history (005 + 006) ----------
CREATE TABLE IF NOT EXISTS {{.Schema}}.memo_versions (
  version_id BIGSERIAL PRIMARY KEY,
  memo_id BIGINT NOT NULL,
  upsert_key TEXT,
  title TEXT, content TEXT, type TEXT, tags TEXT[], sha256 TEXT,
  valid_from TIMESTAMPTZ,                              -- transaction-time start of the retired version
  superseded_at TIMESTAMPTZ NOT NULL DEFAULT now(),   -- transaction-time end
  author_session TEXT, write_source TEXT, trust_tier SMALLINT,
  valid_from_w TIMESTAMPTZ, valid_to_w TIMESTAMPTZ    -- world valid-time carried from memos
);
CREATE INDEX IF NOT EXISTS memo_versions_key_idx
  ON {{.Schema}}.memo_versions (upsert_key) WHERE upsert_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS memo_versions_memo_idx
  ON {{.Schema}}.memo_versions (memo_id);

CREATE OR REPLACE FUNCTION {{.Schema}}.snapshot_memo_version() RETURNS trigger AS $t$
BEGIN
  IF OLD.sha256 IS DISTINCT FROM NEW.sha256 THEN
    INSERT INTO {{.Schema}}.memo_versions
      (memo_id, upsert_key, title, content, type, tags, sha256, valid_from, superseded_at,
       author_session, write_source, trust_tier, valid_from_w, valid_to_w)
    VALUES
      (OLD.id, OLD.upsert_key, OLD.title, OLD.content, OLD.type, OLD.tags, OLD.sha256, OLD.created_at, now(),
       OLD.author_session, OLD.write_source, OLD.trust_tier, OLD.valid_from, OLD.valid_to);
  END IF;
  RETURN NEW;
END;
$t$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS memo_version_snapshot ON {{.Schema}}.memos;
CREATE TRIGGER memo_version_snapshot BEFORE UPDATE ON {{.Schema}}.memos
  FOR EACH ROW EXECUTE FUNCTION {{.Schema}}.snapshot_memo_version();

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
