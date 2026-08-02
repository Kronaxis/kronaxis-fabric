-- Fabric migration 20260802_007: write-time admission quarantine (poisoning defence layer 2).
--
-- arXiv:2607.06595 S2/S3: the strongest cost/benefit defence is a write-time admission judge
-- that keeps a clearly poisoned memo out of the retrievable store entirely, not merely demoted.
-- 006 shipped the retrieval-time screen (demote + flag). This adds the second layer: a memo whose
-- content trips a SEVERE directive signal (planted secret, instruction override, role injection) or
-- two-plus distinct signals is stored but marked quarantined = true, and search excludes it until a
-- human releases it. Deliberately STRICTER than the read screen so a single weak signal only demotes
-- (stays findable) and cannot silently hide a legitimate memo.
--
-- ADDITIVE + idempotent. Existing rows default quarantined = false, so nothing already stored is hidden.
-- Only NEW writes are judged. Reversible (DROP COLUMN). Loops every active tenant.

DO $mig$
DECLARE s text;
BEGIN
  FOR s IN SELECT schema_name FROM kronaxis_meta.tenants WHERE status = 'active' LOOP
    EXECUTE format($q$ALTER TABLE %I.memos
        ADD COLUMN IF NOT EXISTS quarantined       BOOLEAN NOT NULL DEFAULT false,
        ADD COLUMN IF NOT EXISTS quarantine_reason TEXT$q$, s);
    -- partial index: the quarantine review queue is small, index only the flagged rows
    EXECUTE format($q$CREATE INDEX IF NOT EXISTS memos_quarantined_idx
        ON %I.memos (created_at DESC) WHERE quarantined = true AND deleted_at IS NULL$q$, s);
    RAISE NOTICE 'write-time quarantine installed for tenant schema %', s;
  END LOOP;
END $mig$;
