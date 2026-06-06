-- Migration: 20260527_001_meta.sql
-- Creates kronaxis_meta schema (in the same `tfs` database) with the tenant
-- registry, per-tenant bearer keys, audit log, and the provision_tenant()
-- helper. Lives ALONGSIDE the existing `fabric` schema; does not touch it.
--
-- Per spec section 2.4 — kept in tfs DB rather than a separate kronaxis_meta DB
-- so the single fabric pgxpool can serve both without a second pool. Cross-DB
-- queries are not needed; we use schema-qualified table names exclusively.
--
-- Run from psql as the `titan` superuser:
--   psql -h 192.168.50.129 -U titan -d tfs -f migrations/20260527_001_meta.sql
--
-- Idempotent: every CREATE uses IF NOT EXISTS / OR REPLACE.

CREATE SCHEMA IF NOT EXISTS kronaxis_meta;

-- ---------- tenants ----------
CREATE TABLE IF NOT EXISTS kronaxis_meta.tenants (
  id UUID PRIMARY KEY,
  parent_tenant_id UUID REFERENCES kronaxis_meta.tenants(id),
  display_alias TEXT NOT NULL UNIQUE,         -- e.g. 'persona_mary'
  schema_name TEXT NOT NULL UNIQUE,           -- e.g. 'tenant_01977f7e'
  tenant_type TEXT NOT NULL,                  -- 'platform' | 'customer' | 'persona'
  status TEXT NOT NULL DEFAULT 'active',      -- 'active' | 'soft_deleted' | 'archived'
  embedder_url TEXT,                          -- NULL = use server default
  rerank_url TEXT,                            -- NULL = use server default
  retention_days INT,                         -- NULL = forever
  metadata JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  soft_deleted_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS tenants_parent_idx
  ON kronaxis_meta.tenants (parent_tenant_id);
CREATE INDEX IF NOT EXISTS tenants_alias_lower_idx
  ON kronaxis_meta.tenants (lower(display_alias));
CREATE INDEX IF NOT EXISTS tenants_status_idx
  ON kronaxis_meta.tenants (status);

-- ---------- per-tenant bearer keys ----------
CREATE TABLE IF NOT EXISTS kronaxis_meta.tenant_keys (
  id BIGSERIAL PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES kronaxis_meta.tenants(id) ON DELETE CASCADE,
  key_hash TEXT NOT NULL,                     -- sha256(plaintext_bearer)
  key_prefix TEXT NOT NULL,                   -- first 8 chars of plaintext, ops lookup
  scope TEXT NOT NULL DEFAULT 'tenant',       -- 'tenant' | 'admin'
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ,
  last_used_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS tenant_keys_hash_idx
  ON kronaxis_meta.tenant_keys (key_hash);
CREATE INDEX IF NOT EXISTS tenant_keys_tenant_idx
  ON kronaxis_meta.tenant_keys (tenant_id) WHERE revoked_at IS NULL;

-- ---------- audit log ----------
CREATE TABLE IF NOT EXISTS kronaxis_meta.audit_log (
  id BIGSERIAL PRIMARY KEY,
  actor_key_id BIGINT REFERENCES kronaxis_meta.tenant_keys(id),
  actor_tenant_id UUID REFERENCES kronaxis_meta.tenants(id),
  action TEXT NOT NULL,
  target_tenant_id UUID,
  detail JSONB NOT NULL DEFAULT '{}',
  ts TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_log_ts_idx
  ON kronaxis_meta.audit_log (ts DESC);
CREATE INDEX IF NOT EXISTS audit_log_actor_idx
  ON kronaxis_meta.audit_log (actor_tenant_id, ts DESC);
CREATE INDEX IF NOT EXISTS audit_log_action_idx
  ON kronaxis_meta.audit_log (action, ts DESC);
