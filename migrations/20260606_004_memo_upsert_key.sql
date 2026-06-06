-- Migration: 20260606_004_memo_upsert_key.sql
-- Adds `upsert_key` column to memos for the supersession primitive.
--
-- Problem: memos are deduped by sha256(title+content). Banking a fresh
-- "CURRENT STATE 2026-06-06" memo with different content from the May
-- equivalent creates a new row instead of overwriting the stale one. Search
-- then returns whichever has higher cosine similarity, which is often the
-- stale May memo on lexical overlap. Fabric tells you the past, not the
-- present.
--
-- Fix: optional upsert_key column. If provided on POST /v1/memo, the handler
-- does ON CONFLICT (upsert_key) WHERE upsert_key IS NOT NULL AND
-- deleted_at IS NULL DO UPDATE — the row at that key is replaced in place
-- (same id, fresh title/content/type/embedding/updated_at). If omitted,
-- behaviour is unchanged (sha256 dedup as before).
--
-- Backward compatible: existing rows have upsert_key=NULL and continue to
-- be deduped by sha256. Only new memos that opt into a key get supersession.
--
-- Search semantics: at most ONE row per non-null upsert_key ever exists in
-- the table, so searching for "voice current state" returns the freshest
-- bank, not an archaeology of past banks.
--
-- Idempotent. Safe to run on production.
--
-- IMPORTANT: memos tables live in *tenant-scoped* schemas
-- (kronaxis_meta.tenants.schema_name like 'tenant_%'), not in a single
-- 'fabric' schema. This migration iterates every active tenant.
--
-- Run from psql as the `titan` superuser:
--   psql -h 192.168.50.129 -U titan -d tfs -f migrations/20260606_004_memo_upsert_key.sql

\set ON_ERROR_STOP on

BEGIN;

DO $$
DECLARE
  r RECORD;
  s TEXT;
BEGIN
  FOR r IN
    SELECT schema_name
    FROM kronaxis_meta.tenants
    WHERE status = 'active'
      AND soft_deleted_at IS NULL
      AND schema_name LIKE 'tenant_%'
    ORDER BY schema_name
  LOOP
    s := quote_ident(r.schema_name);

    -- Skip tenants whose memos table doesn't exist yet (newly provisioned
    -- mid-migration would be unusual but cheap to guard).
    IF NOT EXISTS (
      SELECT 1 FROM information_schema.tables
      WHERE table_schema = r.schema_name AND table_name = 'memos'
    ) THEN
      RAISE NOTICE 'skipping %: no memos table', r.schema_name;
      CONTINUE;
    END IF;

    EXECUTE format(
      'ALTER TABLE %s.memos ADD COLUMN IF NOT EXISTS upsert_key TEXT',
      s
    );

    EXECUTE format(
      'CREATE UNIQUE INDEX IF NOT EXISTS memos_upsert_key_uniq
         ON %s.memos (upsert_key)
         WHERE upsert_key IS NOT NULL AND deleted_at IS NULL',
      s
    );

    -- Plain (non-unique) index for fast lookup-by-key reads. Optional but
    -- the unique partial index above already covers the same predicate, so
    -- skip the duplicate.

    RAISE NOTICE 'upsert_key added to %.memos', r.schema_name;
  END LOOP;
END $$;

COMMIT;

-- Per-tenant template note ----------------------------------------------
-- migrations/20260527_002_per_tenant_template.sql is the canonical reference
-- DDL applied to every newly provisioned tenant. It has been edited in the
-- same commit as this migration to include the `upsert_key` column +
-- `memos_upsert_key_uniq` partial index, so any tenant created AFTER the
-- fabric binary deploy gets the column natively without needing this
-- migration re-run.
