-- Fabric migration 20260916_009: typed supersession pointer, honoured at read time.
--
-- Measured on 16 September 2026 (docs/internal/research/radar_deep_dives_2026-09-16/
-- AGENT_MEMORY_TRIO.md): on 36 neutral queries over 18 topics that had been corrected,
-- fabric returned the SUPERSEDED memo as the top hit 9 times (25%). Same day flip flops
-- by one session score within 0.001 of each other, and an accumulation of nine stale
-- versions of one state buried the current one at rank three. Recency boosts and a
-- model based conflict detector both reproduced the paper's negative results
-- (arXiv 2609.16073, "semantic shadowing" and the majority vote trap).
--
-- The fix is a column, not an engine: a memo that supersedes another writes its own id
-- onto the older row's `superseded_by`, and search excludes rows with a pointer unless the
-- caller asks for history. 2,402 memos already say "CORRECTION to #N" or "Delta on #N"
-- in prose; the backfill (scripts/backfill_superseded.py) applies the CORRECTION family
-- only, because "Delta on" means continuation as often as replacement.
--
-- ADDITIVE + idempotent. Existing rows default NULL (live). Loops every active tenant.

DO $mig$
DECLARE s text;
BEGIN
  FOR s IN SELECT schema_name FROM kronaxis_meta.tenants WHERE status = 'active' LOOP
    EXECUTE format($q$ALTER TABLE %I.memos
        ADD COLUMN IF NOT EXISTS superseded_by BIGINT,
        ADD COLUMN IF NOT EXISTS superseded_at TIMESTAMPTZ$q$, s);
    EXECUTE format($q$CREATE INDEX IF NOT EXISTS memos_superseded_by_idx
        ON %I.memos (superseded_by) WHERE superseded_by IS NOT NULL$q$, s);
    RAISE NOTICE 'superseded_by installed for tenant schema %', s;
  END LOOP;
END $mig$;
