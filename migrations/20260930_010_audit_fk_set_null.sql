-- 010: let a tenant be erased. audit_log's actor FKs had no ON DELETE rule, so a
-- tenant whose keys had ever acted could not be purged (the delete of its keys and
-- tenants row was refused). They become ON DELETE SET NULL: the audit rows stay,
-- the link to the erased key or tenant goes. purgeTenant also scrubs the detail of
-- rows about the erased tenant and records the erasure itself.
BEGIN;
ALTER TABLE kronaxis_meta.audit_log DROP CONSTRAINT IF EXISTS audit_log_actor_key_id_fkey;
ALTER TABLE kronaxis_meta.audit_log
  ADD CONSTRAINT audit_log_actor_key_id_fkey FOREIGN KEY (actor_key_id)
  REFERENCES kronaxis_meta.tenant_keys(id) ON DELETE SET NULL;
ALTER TABLE kronaxis_meta.audit_log DROP CONSTRAINT IF EXISTS audit_log_actor_tenant_id_fkey;
ALTER TABLE kronaxis_meta.audit_log
  ADD CONSTRAINT audit_log_actor_tenant_id_fkey FOREIGN KEY (actor_tenant_id)
  REFERENCES kronaxis_meta.tenants(id) ON DELETE SET NULL;
COMMIT;
