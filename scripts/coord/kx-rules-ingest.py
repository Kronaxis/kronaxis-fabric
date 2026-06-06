#!/usr/bin/env python3
"""
ingest_rules.py — parse GOLDEN_RULES.md and POST one memo per rule to fabric.

Source of truth: /home/jason/projects/kronaxis/GOLDEN_RULES.md (17 rules).
Each rule becomes one memo of type=reference with tags:
  rule:N, topic:<derived>, version:2026-06-06-17rules,
  source:GOLDEN_RULES.md, and 'hard-floor' for the 5 mandatory ones.

After all 17 land, posts one manifest memo listing every rule with its id+topic.

Reversible: run with --dry-run to print without POSTing. Run with --purge to
search-and-delete any prior 2026-06-06-17rules memos first (clean re-ingest).
"""
from __future__ import annotations

import argparse
import json
import re
import sys
import urllib.request
import urllib.error
from pathlib import Path

FABRIC = "http://192.168.50.129:8201"
TOKEN = "test-key-1"
VERSION_TAG = "version:2026-06-06-46rules"
SOURCE_TAG = "source:GOLDEN_RULES.md"
# Canonical from the LAPTOP (DL580 local copy was stale host-drift). Conductor
# scp'd it to /tmp/GOLDEN_RULES_canonical.md, md5 37466dce940f14c6dfe96190ae13a0a5.
SRC_PATH = Path("/tmp/GOLDEN_RULES_canonical.md")
EXPECTED_MD5 = "37466dce940f14c6dfe96190ae13a0a5"
EXPECTED_RULE_COUNT = 46

# Hard-floor: rules that must remain inline in CLAUDE.md as a fabric-down safety floor.
# Per conductor (coord #982) + load-bearing voice/honesty/session-start picks.
HARD_FLOOR = {2, 6, 9, 15, 16, 27, 30, 40, 41, 44, 45, 46}

# Per-rule topic tags. Chosen so a session-name match (e.g. 'voice', 'safety',
# 'memory', 'fabric', 'coord') can pull the topically-relevant rules into context.
TOPIC = {
    1:  ["verify", "documentation"],
    2:  ["honesty", "voice"],
    3:  ["testing", "verify"],
    4:  ["layering", "voice"],
    5:  ["honesty", "voice"],
    6:  ["versioning", "safety"],
    7:  ["planning"],
    8:  ["hardware", "constraints"],
    9:  ["fabric", "memory", "session-start"],
    10: ["commercial", "legal"],
    11: ["simulations", "testing"],
    12: ["structure", "voice"],
    13: ["blockers", "honesty"],
    14: ["scope", "honesty"],
    15: ["safety", "post-task", "documentation", "backup"],
    16: ["voice", "british-english", "ai-tells"],
    17: ["sub-repos", "sync", "post-task"],
    18: ["testing"],
    19: ["voice", "british-english"],
    20: ["voice", "ai-tells"],
    21: ["i18n", "voice", "website"],
    22: ["planning"],
    23: ["verify"],
    24: ["launch", "beacon"],
    25: ["context", "session"],
    26: ["election", "priority"],
    27: ["deploy", "safety", "website"],
    28: ["website", "og-tags"],
    29: ["election", "traces", "publishing"],
    30: ["infra", "safety", "hardware", "no-laptop-compute"],
    31: ["gpu", "planning"],
    32: ["backups", "safety", "data"],
    33: ["backups", "safety"],
    34: ["data-gather", "resumable"],
    35: ["kg", "query-first", "session-start"],
    36: ["storage", "nas"],
    37: ["skills", "capabilities"],
    38: ["mindset"],
    39: ["docs", "structure"],
    40: ["ntfy", "safety", "infra"],
    41: ["qwen", "fallback", "safety", "refusal"],
    42: ["rename", "coord", "voice"],
    43: ["coord", "reply"],
    44: ["bootstrap", "session-start", "infra"],
    45: ["fabric", "session-start", "post-task"],
    46: ["push-waiter", "coord", "infra", "session-start"],
}

# Short titles for the manifest line — keep punchy.
SHORT = {
    1: "Read the code; ignore documentation status labels.",
    2: "Brutal honesty is the baseline.",
    3: "Every claim needs a test criterion.",
    4: "Separate Vision/Design/Code/Working.",
    5: "If the answer is no, say no first.",
    6: "One canonical version; delete the rest.",
    7: "Dependency order before task list.",
    8: "Hardware is a primary constraint.",
    9: "Context persists: fabric first, memory files second.",
    10: "Commercial/legal framing must be realistic.",
    11: "Simulations must simulate (can return FAIL).",
    12: "Organise with structure, not prose.",
    13: "Flag blockers immediately.",
    14: "Ambition is the asset; implementation gap is the risk.",
    15: "Post-task safety: sweep, document, verify, backup.",
    16: "No em dashes, no AI tells, British English.",
    17: "Sync sub-repos from monorepo after every session.",
    18: "FULL testing: every button, every route, every system.",
    19: "No hyphenated compound words in any written content.",
    20: "No AI attribution. Ever.",
    21: "Every page change includes all 13 languages.",
    22: "Plan before building. Always.",
    23: "Do not answer from assumption; check the code first.",
    24: "Every product launch goes through Beacon.",
    25: "Monitor context usage; ask before window fills.",
    26: "Election work takes absolute priority near limits.",
    27: "Every website update completes the full deployment checklist.",
    28: "Every page has specific, context-aware OG tags.",
    29: "Causal traces are the product; capture and publish every election run.",
    30: "All heavy compute on DL580; never on the XPS-13 laptop.",
    31: "GPU time estimates include full setup overhead.",
    32: "Triangulate every data asset: live + local + offsite.",
    33: "Every piece of work lands on all four backup legs within 24h.",
    34: "Long-running data gathering streams output + is resumable.",
    35: "Knowledge graph runs nightly at 5am; always query it first.",
    36: "Use the NAS for storage; never fill /mnt/external.",
    37: "Check ~/.claude/skills/ before claiming a capability is missing.",
    38: "Brick walls are there to stop people who don't want it badly enough.",
    39: "Use the new docs/ tree; pick the right home for every kind of writing.",
    40: "All notifications go to the private self-hosted ntfy; never ntfy.sh.",
    41: "Claude refusal or output-filter trip => pass to Qwen-local. Always.",
    42: "On /rename, adopt the new name as your coord-channel sender label.",
    43: "Reply to coord pings IMMEDIATELY; holding reply at minimum.",
    44: "Bootstrap every session with kx-session-bootstrap <name> first.",
    45: "Fabric search BEFORE any non-trivial decision; remember AFTER.",
    46: "Every session arms a coord push-waiter immediately after bootstrap.",
}


def parse_rules(text: str) -> list[tuple[int, str, str]]:
    """Return [(number, title, body), ...] in order, where body is the
    rule text from after the H2 header up to (but not including) the next H2.
    Strips trailing horizontal-rule separators and surrounding blank lines.
    """
    pattern = re.compile(r"^## (\d+)\.\s+(.+?)$", re.MULTILINE)
    matches = list(pattern.finditer(text))
    out = []
    for i, m in enumerate(matches):
        num = int(m.group(1))
        title = m.group(2).strip()
        body_start = m.end()
        body_end = matches[i + 1].start() if i + 1 < len(matches) else len(text)
        body = text[body_start:body_end].strip()
        # Strip a leading "---" separator if present, and any trailing one.
        body = re.sub(r"^-{3,}\s*\n", "", body)
        body = re.sub(r"\n-{3,}\s*$", "", body)
        body = body.strip()
        out.append((num, title, body))
    return out


def post_memo(content: str, title: str, tags: list[str], type_: str, dry: bool) -> int | None:
    if dry:
        print(f"[DRY] POST memo: title={title!r} tags={tags} type={type_} bytes={len(content)}")
        return None
    payload = json.dumps({
        "content": content,
        "title": title,
        "tags": tags,
        "type": type_,
    }).encode()
    req = urllib.request.Request(
        f"{FABRIC}/v1/memo",
        data=payload,
        method="POST",
        headers={
            "Authorization": f"Bearer {TOKEN}",
            "Content-Type": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            resp = json.loads(r.read().decode())
            return resp.get("id")
    except urllib.error.HTTPError as e:
        sys.stderr.write(f"POST FAILED: {e.code} {e.read().decode()[:200]}\n")
        return None


def search_and_purge(dry: bool) -> int:
    """Find all memos tagged version:2026-06-06-17rules and delete them.
    Returns count deleted. Search returns top results by score; loop until empty.
    """
    deleted = 0
    while True:
        req = urllib.request.Request(
            f"{FABRIC}/v1/memo/search",
            data=json.dumps({"query": VERSION_TAG, "limit": 100}).encode(),
            method="POST",
            headers={
                "Authorization": f"Bearer {TOKEN}",
                "Content-Type": "application/json",
            },
        )
        with urllib.request.urlopen(req, timeout=20) as r:
            data = json.loads(r.read().decode())
        ids = []
        for hit in data.get("results", []):
            # Only purge memos whose excerpt OR a fetched-tag check actually mentions our version tag.
            # Search is hybrid (text+vector); the version tag is in tags not content, so we'd need a tag
            # filter. To stay safe, we do a per-id GET and check tags.
            mid = hit["id"]
            # GET to read tags
            try:
                with urllib.request.urlopen(
                    urllib.request.Request(
                        f"{FABRIC}/v1/memo/{mid}",
                        headers={"Authorization": f"Bearer {TOKEN}"},
                    ),
                    timeout=10,
                ) as gr:
                    memo = json.loads(gr.read().decode())
                if VERSION_TAG in (memo.get("tags") or []):
                    ids.append(mid)
            except urllib.error.HTTPError:
                continue
        if not ids:
            break
        for mid in ids:
            if dry:
                print(f"[DRY] DELETE memo id={mid}")
            else:
                try:
                    urllib.request.urlopen(
                        urllib.request.Request(
                            f"{FABRIC}/v1/memo/{mid}",
                            method="DELETE",
                            headers={"Authorization": f"Bearer {TOKEN}"},
                        ),
                        timeout=10,
                    )
                    deleted += 1
                except urllib.error.HTTPError as e:
                    sys.stderr.write(f"DELETE {mid} failed: {e.code}\n")
        if dry:
            break  # don't loop forever in dry mode
    return deleted


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--purge", action="store_true",
                    help="Search-and-delete any prior memos tagged " + VERSION_TAG + " before ingest")
    args = ap.parse_args()

    import hashlib
    raw = SRC_PATH.read_bytes()
    actual_md5 = hashlib.md5(raw).hexdigest()
    if actual_md5 != EXPECTED_MD5:
        sys.stderr.write(
            f"FATAL: {SRC_PATH} md5={actual_md5} expected={EXPECTED_MD5} — refuse to ingest stale/wrong file\n"
        )
        sys.exit(1)
    text = raw.decode()
    rules = parse_rules(text)
    if len(rules) != EXPECTED_RULE_COUNT:
        sys.stderr.write(f"FATAL: expected {EXPECTED_RULE_COUNT} rules, parsed {len(rules)}\n")
        sys.exit(1)
    print(f"Parsed {len(rules)} rules from {SRC_PATH} (md5 OK)")

    if args.purge:
        n = search_and_purge(args.dry_run)
        print(f"Purged {n} prior memos tagged {VERSION_TAG}")

    posted: list[tuple[int, int | None, str]] = []  # (rule_no, memo_id, title)
    for num, title, body in rules:
        topics = TOPIC.get(num, [])
        tags = [f"rule:{num}", SOURCE_TAG, VERSION_TAG] + [f"topic:{t}" for t in topics]
        if num in HARD_FLOOR:
            tags.append("hard-floor")
        memo_title = f"Golden Rule {num}: {title}"
        memo_content = f"# Golden Rule {num}: {title}\n\n{body}\n"
        mid = post_memo(memo_content, memo_title, tags, "reference", args.dry_run)
        posted.append((num, mid, title))
        print(f"  rule {num:2d}: id={mid} hard-floor={num in HARD_FLOOR} topics={topics}")

    # Manifest memo
    manifest_lines = [
        f"# Golden Rules manifest ({EXPECTED_RULE_COUNT} rules, version {VERSION_TAG.split(':',1)[1]})",
        "",
        f"Source: {SRC_PATH} (md5 {EXPECTED_MD5})",
        f"Canonical authored file lives at /home/jason/projects/kronaxis/GOLDEN_RULES.md on the LAPTOP.",
        f"Hard-floor rules (must remain inline in CLAUDE.md as fabric-down fallback): {sorted(HARD_FLOOR)}",
        "",
        "| # | Memo ID | Hard-floor | Topics | Title |",
        "|---|---|---|---|---|",
    ]
    for num, mid, title in posted:
        hf = "YES" if num in HARD_FLOOR else ""
        topics = ",".join(TOPIC.get(num, []))
        manifest_lines.append(
            f"| {num} | {mid or '-'} | {hf} | {topics} | {SHORT.get(num, title)} |"
        )
    manifest_lines += [
        "",
        "## How fabric distributes these",
        "",
        "- `kx-session-bootstrap` MUST fetch all memos with tag `hard-floor` and print them",
        "  to the session context every session (mandatory floor).",
        "- For each session name, bootstrap ALSO fetches memos whose `topic:*` tag matches",
        "  the session-name keyword (best-effort topical match). E.g. session 'voicepush'",
        "  pulls topic:voice rules; session 'safety' pulls topic:safety rules.",
        "- CLAUDE.md keeps the 5 hard-floor rules INLINE as a fabric-down fallback.",
        "- `GOLDEN_RULES.md` remains the canonical human-authored source; a one-way sync",
        "  re-ingests it into fabric on change (script: ingest_rules.py --purge).",
        "",
        "## Brief reconciliation note",
        "",
        "The conductor's brief said 'all 46 golden rules'. The DL580's local copy was",
        "STALE host-drift (17 rules) — the LAPTOP's canonical has 46 rules",
        f"(md5 {EXPECTED_MD5}). Conductor scp'd it to /tmp/GOLDEN_RULES_canonical.md",
        "via coord #982 (2026-06-06 15:35:30). This is EXACTLY the single-source",
        "problem the rulesfab task is designed to fix — once Phase 2 ships the",
        "bootstrap-fetches-rules-from-fabric flow, every host loads the same set.",
    ]
    manifest = "\n".join(manifest_lines) + "\n"
    mid = post_memo(
        manifest,
        f"Golden Rules manifest 2026-06-06 ({EXPECTED_RULE_COUNT} rules, kronaxis canonical)",
        [VERSION_TAG, SOURCE_TAG, "manifest", "topic:rules-index"],
        "reference",
        args.dry_run,
    )
    print(f"\nManifest memo id={mid}")
    print(f"Total: {len(rules)} rule memos + 1 manifest = {len(rules) + 1}")


if __name__ == "__main__":
    main()
