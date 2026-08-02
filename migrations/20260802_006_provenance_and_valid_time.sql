-- Fabric migration 20260802_006: (a) memory-poisoning provenance + (b) world valid-time.
--
-- (a) PROVENANCE — arXiv:2607.06595 "When Agents Remember Too Much". Fabric hands every
--     session a `remember` tool that writes arbitrary content, and ranks retrieval by
--     cosine+tsvector+recency — the exact surface a poisoned memo optimises against. We add
--     author_session / write_source / trust_tier so retrieval can weight by who wrote a memo
--     and a directive-content screen can flag "memories as commands". Cheapest structural fix:
--     record what we already know (the writing session + key scope) and use it defensively.
--
-- (b) WORLD VALID-TIME — arXiv:2607.26520. Yesterday's 005 gave us transaction-time history
--     (memo_versions: valid_from=created_at, superseded_at). This adds the second SQL:2011 axis:
--     valid_from/valid_to = when the fact is TRUE in the world (vs when we recorded it), enabling
--     an `as_of` retrieval filter. NULL = unbounded (existing rows always match — safe, no rewrite).
--
-- ADDITIVE + idempotent + reversible. No change to the hot INSERT path or existing indexes.
-- Loops every active tenant; re-run picks up new ones. Trigger is CREATE OR REPLACE.

DO $mig$
DECLARE s text;
BEGIN
  FOR s IN SELECT schema_name FROM kronaxis_meta.tenants WHERE status = 'active' LOOP
    -- (a) + (b) columns on the live memos table
    EXECUTE format($q$ALTER TABLE %I.memos
        ADD COLUMN IF NOT EXISTS author_session TEXT,
        ADD COLUMN IF NOT EXISTS write_source   TEXT,
        ADD COLUMN IF NOT EXISTS trust_tier      SMALLINT NOT NULL DEFAULT 1,
        ADD COLUMN IF NOT EXISTS valid_from      TIMESTAMPTZ,
        ADD COLUMN IF NOT EXISTS valid_to        TIMESTAMPTZ$q$, s);

    -- history parity on memo_versions (created by 005). world_* carry the valid-time axis;
    -- the existing valid_from/superseded_at columns remain the transaction-time axis.
    EXECUTE format($q$ALTER TABLE %I.memo_versions
        ADD COLUMN IF NOT EXISTS author_session TEXT,
        ADD COLUMN IF NOT EXISTS write_source   TEXT,
        ADD COLUMN IF NOT EXISTS trust_tier      SMALLINT,
        ADD COLUMN IF NOT EXISTS valid_from_w    TIMESTAMPTZ,
        ADD COLUMN IF NOT EXISTS valid_to_w      TIMESTAMPTZ$q$, s);

    -- valid-time lookup index for the as_of filter (current rows only)
    EXECUTE format($q$CREATE INDEX IF NOT EXISTS memos_valid_time_idx
        ON %I.memos (valid_from, valid_to) WHERE deleted_at IS NULL$q$, s);
    -- trust index so a poisoned low-trust memo is cheap to filter/deprioritise
    EXECUTE format($q$CREATE INDEX IF NOT EXISTS memos_trust_idx
        ON %I.memos (trust_tier) WHERE deleted_at IS NULL$q$, s);

    -- extend the snapshot trigger to carry provenance + world valid-time into history
    EXECUTE format($q$
      CREATE OR REPLACE FUNCTION %I.snapshot_memo_version() RETURNS trigger AS $t$
      BEGIN
        IF OLD.sha256 IS DISTINCT FROM NEW.sha256 THEN
          INSERT INTO %I.memo_versions
            (memo_id, upsert_key, title, content, type, tags, sha256, valid_from, superseded_at,
             author_session, write_source, trust_tier, valid_from_w, valid_to_w)
          VALUES
            (OLD.id, OLD.upsert_key, OLD.title, OLD.content, OLD.type, OLD.tags, OLD.sha256, OLD.created_at, now(),
             OLD.author_session, OLD.write_source, OLD.trust_tier, OLD.valid_from, OLD.valid_to);
        END IF;
        RETURN NEW;
      END;
      $t$ LANGUAGE plpgsql$q$, s, s);

    RAISE NOTICE 'provenance + valid-time installed for tenant schema %', s;
  END LOOP;
END $mig$;
