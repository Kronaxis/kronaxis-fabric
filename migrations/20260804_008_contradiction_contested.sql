-- Fabric migration 20260804_008: false-fact / contradiction defence (poisoning layer 3).
--
-- RAGuard (arXiv:2607.26339) exposes the gap 006/007 do not cover: a memo that quietly asserts a
-- FALSE FACT ("the client risk tier is low") reads as data, trips no directive regex, and sails
-- through. RAGuard's ZKIP catches it by answer influence, at a 6x per-query generation cost we will
-- not pay on a read. This ports the idea OFF the hot path: after a memo is written, a background
-- check retrieves its nearest TRUSTED neighbours and asks a judge whether the new memo contradicts
-- them. An untrusted contradiction is quarantined (reusing 007); a TRUSTED contradiction is not
-- hidden (that would break legitimate belief revision) but is marked `contested` and surfaced as a
-- flag at retrieval, so a reader weighs it against the established record.
--
-- ADDITIVE + idempotent. Existing rows default contested = false. Loops every active tenant.

DO $mig$
DECLARE s text;
BEGIN
  FOR s IN SELECT schema_name FROM kronaxis_meta.tenants WHERE status = 'active' LOOP
    EXECUTE format($q$ALTER TABLE %I.memos
        ADD COLUMN IF NOT EXISTS contested        BOOLEAN NOT NULL DEFAULT false,
        ADD COLUMN IF NOT EXISTS contested_reason  TEXT$q$, s);
    RAISE NOTICE 'contradiction/contested column installed for tenant schema %', s;
  END LOOP;
END $mig$;
