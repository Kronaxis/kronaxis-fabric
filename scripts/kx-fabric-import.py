#!/usr/bin/env python3
"""
kx-fabric-import — bulk-import a directory of markdown memos into fabric.

Reads YAML frontmatter (name, description, metadata.type) + body,
POSTs to /v1/memo with sha256 dedup. Idempotent; safe to re-run.

Environment overrides:
  FABRIC_URL  (default: http://localhost:8201)
  FABRIC_KEY  (required; set or pass --fabric-key)
"""
import argparse, json, os, sys, urllib.request, re
from pathlib import Path

def parse_memo(path: Path):
    text = path.read_text(encoding='utf-8', errors='replace')
    # Strip YAML frontmatter ---...---
    fm = {}
    body = text
    if text.startswith('---'):
        m = re.match(r'^---\n(.*?)\n---\n(.*)$', text, re.DOTALL)
        if m:
            raw_fm = m.group(1)
            body = m.group(2).lstrip('\n')
            # Very small YAML parser — just key: value pairs, ignores nested
            cur_key = None
            for line in raw_fm.splitlines():
                if re.match(r'^[a-zA-Z_]+:', line):
                    k, _, v = line.partition(':')
                    fm[k.strip()] = v.strip()
                    cur_key = k.strip()
            # Strip surrounding quotes if any
            for k, v in list(fm.items()):
                if v.startswith(('"', "'")) and v.endswith(('"', "'")) and len(v) >= 2:
                    fm[k] = v[1:-1]
    title = fm.get('name', path.stem.replace('_', ' ').replace('-', ' '))
    if fm.get('description'):
        title = fm['description'][:200]
    mtype = fm.get('type') or 'general'
    # If "metadata" line was seen but type wasn't pulled, default
    if mtype not in ('user', 'feedback', 'project', 'reference', 'general'):
        mtype = 'general'
    return title, body, mtype


def post_memo(url: str, key: str, title: str, content: str, mtype: str, tags=None):
    body = json.dumps({
        'title': title,
        'content': content,
        'type': mtype,
        'tags': tags or [],
    }).encode()
    req = urllib.request.Request(
        f"{url}/v1/memo",
        data=body,
        headers={'Authorization': f'Bearer {key}', 'Content-Type': 'application/json'},
        method='POST',
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors='replace')[:300]
    except Exception as e:
        return 0, str(e)


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--memory-dir', required=True, help='directory of *.md files to import')
    p.add_argument('--fabric-url', default=os.environ.get('FABRIC_URL', 'http://localhost:8201'))
    p.add_argument('--fabric-key', default=os.environ.get('FABRIC_KEY', ''))
    p.add_argument('--limit', type=int, default=0, help='0 = no limit')
    p.add_argument('--skip-index', action='store_true', help='skip a top-level index file named MEMORY.md / README.md')
    args = p.parse_args()

    if not args.fabric_key:
        sys.exit("FABRIC_KEY required (set env var or pass --fabric-key)")

    memdir = Path(args.memory_dir)
    if not memdir.is_dir():
        sys.exit(f"memory dir not found: {memdir}")

    files = sorted(memdir.glob('*.md'))
    if args.skip_index:
        files = [f for f in files if f.name.upper() not in ('MEMORY.MD', 'README.MD')]

    if args.limit:
        files = files[:args.limit]

    print(f"importing {len(files)} memos -> {args.fabric_url}")
    stats = {'ok': 0, 'deduped': 0, 'failed': 0, 'empty': 0}
    failures = []

    for i, path in enumerate(files, 1):
        try:
            title, body, mtype = parse_memo(path)
        except Exception as e:
            stats['failed'] += 1
            failures.append((path.name, f'parse: {e}'))
            continue
        if not body.strip():
            stats['empty'] += 1
            continue
        code, resp = post_memo(args.fabric_url, args.fabric_key, title, body, mtype, tags=[path.stem])
        if code == 200 and isinstance(resp, dict):
            if resp.get('deduped'):
                stats['deduped'] += 1
            else:
                stats['ok'] += 1
        else:
            stats['failed'] += 1
            failures.append((path.name, f'{code}: {str(resp)[:120]}'))
        if i % 50 == 0:
            print(f"  {i}/{len(files)} | ok={stats['ok']} deduped={stats['deduped']} fail={stats['failed']}")

    print(f"\nDONE. Stats: {json.dumps(stats)}")
    if failures:
        print(f"\nFirst 10 failures:")
        for fname, err in failures[:10]:
            print(f"  {fname}: {err}")


if __name__ == '__main__':
    main()
