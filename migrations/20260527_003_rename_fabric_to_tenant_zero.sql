-- Migration: 20260527_003_rename_fabric_to_tenant_zero.sql
-- Phase 1 of the multi-tenant rollout (spec section 7.1, refined).
--
-- Moves the per-tenant-scoped tables (memos, chunks) from the `fabric`
-- schema into a NEW `tenant_00000000` schema.
--
-- Leaves the global-infrastructure tables (sessions, tasks, federation_peers,
-- router_observations, symbols, symbol_edges) where they are inside `fabric`
-- — per spec section 3.4 they are NOT tenant-scoped. Code-graph symbols
-- explicitly stay global because the codebase is one shared corpus.
--
-- coord_messages remains in `public` (unchanged from current).
--
-- Registers tenant-zero in kronaxis_meta.tenants and seeds tenant-zero's
-- admin-scope bearer key with the existing FABRIC_KEY (passed as :legacy_key).
--
-- IDEMPOTENT.
--
-- Usage (after applying 20260527_001_meta.sql):
--   psql -h 192.168.50.129 -U titan -d tfs \
--     -v legacy_key="'PASTE_FABRIC_KEY_PLAINTEXT'" \
--     -f migrations/20260527_003_rename_fabric_to_tenant_zero.sql
--
-- Rollback:
--   ALTER TABLE tenant_00000000.memos SET SCHEMA fabric;
--   ALTER TABLE tenant_00000000.chunks SET SCHEMA fabric;  -- if it exists
--   DROP SCHEMA tenant_00000000 CASCADE;
--   DELETE FROM kronaxis_meta.tenant_keys WHERE tenant_id = '00000000-0000-7000-8000-000000000000';
--   DELETE FROM kronaxis_meta.tenants WHERE id = '00000000-0000-7000-8000-000000000000';

CREATE SCHEMA IF NOT EXISTS tenant_00000000;

-- Move memos + chunks if still in fabric and not yet in tenant_00000000.
-- Leave symbols + symbol_edges in fabric (global per spec section 3.4).
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
             WHERE n.nspname='fabric' AND c.relname='memos' AND c.relkind='r') THEN
    EXECUTE 'ALTER TABLE fabric.memos SET SCHEMA tenant_00000000';
    RAISE NOTICE 'Moved fabric.memos -> tenant_00000000.memos';
  END IF;
  IF EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
             WHERE n.nspname='fabric' AND c.relname='chunks' AND c.relkind='r') THEN
    EXECUTE 'ALTER TABLE fabric.chunks SET SCHEMA tenant_00000000';
    RAISE NOTICE 'Moved fabric.chunks -> tenant_00000000.chunks';
  END IF;
END
$$;

-- Register tenant-zero in the registry. UUID v7 hand-crafted to be visually
-- identifiable (timestamp portion = all zeros).
INSERT INTO kronaxis_meta.tenants (
  id, parent_tenant_id, display_alias, schema_name, tenant_type, status, metadata
) VALUES (
  '00000000-0000-7000-8000-000000000000'::uuid,
  NULL,
  'kronaxis_claude_session',
  'tenant_00000000',
  'platform',
  'active',
  jsonb_build_object(
    'note', 'Tenant-zero: the original Claude session memory pre-multi-tenant.',
    'migrated_at', now()
  )
)
ON CONFLICT (id) DO NOTHING;

-- Seed admin-scope bearer key for tenant-zero using the legacy FABRIC_KEY
-- so existing tooling keeps working.
INSERT INTO kronaxis_meta.tenant_keys (
  tenant_id, key_hash, key_prefix, scope
)
SELECT
  '00000000-0000-7000-8000-000000000000'::uuid,
  encode(sha256(convert_to(:legacy_key, 'UTF8')), 'hex'),
  substr(:legacy_key, 1, 8),
  'admin'
WHERE NOT EXISTS (
  SELECT 1 FROM kronaxis_meta.tenant_keys
  WHERE key_hash = encode(sha256(convert_to(:legacy_key, 'UTF8')), 'hex')
);

INSERT INTO kronaxis_meta.audit_log (action, target_tenant_id, detail)
VALUES (
  'migration.tenant_zero_seed',
  '00000000-0000-7000-8000-000000000000'::uuid,
  jsonb_build_object(
    'moved_tables', jsonb_build_array('memos','chunks'),
    'memos_at_migration', (SELECT count(*) FROM tenant_00000000.memos)
  )
);
