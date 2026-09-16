#!/usr/bin/env python3
"""One off backfill of superseded_by (migration 009) from what memos already say.

Applies only the correction family: "CORRECTION to #N", "CORRECTS #N", "SUPERSEDES #N",
"REPLACES #N", "RETIRES #N", "OVERRIDES #N", where the referring memo is newer than N
and both are live. "Delta on #N" is listed for the eye but never applied: it means
continuation as often as replacement.

  backfill_superseded.py [--apply] [--schema tenant_zero]

Without --apply it prints what it would do. Needs PGPASSWORD or TFS_DB_PASSWORD.
"""
import argparse, os, re, sys
import psycopg2

RE_APPLY = re.compile(r"(?i)\b(?:CORRECTION\s+(?:to|of)|CORRECTS|SUPERSEDES|REPLACES|RETIRES|OVERRIDES)\s+(?:memo\s+)?#(\d{2,7})\b")
RE_DELTA = re.compile(r"(?i)\bDelta\s+on\s+#(\d{2,7})\b")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--apply", action="store_true")
    ap.add_argument("--schema", default="tenant_zero")
    ap.add_argument("--dbname", default=os.environ.get("FABRIC_DB", "fabric"))
    a = ap.parse_args()
    pg = psycopg2.connect(host="127.0.0.1", user="titan", dbname=a.dbname,
                          password=os.environ.get("PGPASSWORD") or os.environ.get("TFS_DB_PASSWORD"))
    cur = pg.cursor()
    cur.execute(f"select id, title, content from {a.schema}.memos where deleted_at is null and quarantined = false order by id")
    rows = cur.fetchall()
    live = {r[0] for r in rows}
    apply_pairs, delta_pairs = [], []
    for mid, title, content in rows:
        text = (title or "") + "\n" + (content or "")
        for m in RE_APPLY.finditer(text):
            t = int(m.group(1))
            if t in live and t < mid:
                apply_pairs.append((t, mid))
        for m in RE_DELTA.finditer(text):
            t = int(m.group(1))
            if t in live and t < mid:
                delta_pairs.append((t, mid))
    # newest superseder wins when several claim the same target
    best = {}
    for t, mid in apply_pairs:
        if t not in best or mid > best[t]:
            best[t] = mid
    print(f"{len(rows)} live memos; correction family pairs {len(apply_pairs)} on {len(best)} targets; "
          f"Delta on pairs {len(delta_pairs)} (listed, not applied)")
    for t, mid in sorted(best.items())[:40]:
        print(f"  #{t} superseded by #{mid}")
    if len(best) > 40:
        print(f"  ... and {len(best) - 40} more")
    if not a.apply:
        print("dry run; pass --apply to write"); return 0
    n = 0
    for t, mid in best.items():
        cur.execute(f"update {a.schema}.memos set superseded_by=%s, superseded_at=now() "
                    f"where id=%s and superseded_by is null and deleted_at is null", (mid, t))
        n += cur.rowcount
    pg.commit()
    print(f"applied: {n} memos marked superseded")
    return 0


if __name__ == "__main__":
    sys.exit(main())
