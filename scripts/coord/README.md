# Coord channel — clients

Fabric owns the **coord channel**: the `coord_messages` PostgreSQL table on
the same PG instance as the memo store, plus a `pg_notify` trigger that
pushes every insert to every subscribed listener.

This directory holds the **client side** of that channel: shell + bash
utilities every Kronaxis session uses to send / receive / drain coord
messages, plus the session-bootstrap that arms each session to talk to
both coord and the fabric HTTP API.

| Tool | Purpose |
|---|---|
| `kx-coord-send` | POST a message to the channel. `kx-coord-send "X→Y: subject \| body"` or `-s X -r Y -t subject -b body`. |
| `kx-coord-listen` | Block on pg_notify, stream new messages as JSON-per-line (or `--pretty`). Filter by recipient with `--filter NAME`. |
| `kx-coord-tail` | Last N messages, then follow. Like `tail -F` for coord. `-n 50 --filter NAME --since '2h ago'`. |
| `kx-coord-watch` | Same as listen but with desktop notification on each new message addressed to the running user. |
| `kx-coord-pending` | One-shot drain: print messages addressed to a recipient since the last invocation, update last-seen. Designed for prompt-submit hooks. |
| `kx-coord-pending-hook` | Safe wrapper around `kx-coord-pending` for `~/.claude/settings.json` `UserPromptSubmit` hook. Sid-keyed per-pty lookup, 3s hard timeout. |
| `kx-coord-listen-direct` | Listen variant that talks PG directly without going through any local relay. |
| `kx-coord-bridge-pg-to-file` | One-way bridge: stream coord into a tail-able log file for hosts that need a file-based interface. |
| `kx-session-bootstrap` | First action of every Claude session. Arms a coord listener filtered on session name, verifies fabric is alive, pulls top-3 relevant memos, drains backlog, registers per-shell-sid identity. |
| `kx-session-bootstrap-host.service` | systemd user unit that runs `kx-session-bootstrap` at boot under the host short hostname. Gives non-interactive cron / scheduled workloads a permanent coord identity. |

## Install on a fresh host

```bash
mkdir -p ~/bin
cp scripts/coord/kx-*  ~/bin/
chmod +x ~/bin/kx-*

# UserPromptSubmit hook for any Claude Code installation
# Add to ~/.claude/settings.json:
#   "hooks": {
#     "UserPromptSubmit": [
#       { "hooks": [ { "type": "command", "command": "$HOME/bin/kx-coord-pending-hook" } ] }
#     ]
#   }

mkdir -p ~/.config/systemd/user
cp scripts/coord/kx-session-bootstrap-host.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable kx-session-bootstrap-host.service
systemctl --user start  kx-session-bootstrap-host.service
sudo loginctl enable-linger "$USER"   # so user systemd survives logout
```

## Per-session

```bash
kx-session-bootstrap <session_name>
```

That's the entire on-ramp. The hook drains coord on every prompt, so the
session stays current without manual polling.

## Protocol rules

Defined in the main `kronaxis` repo at `GOLDEN_RULES.md` §44 and §45:

- §44 — bootstrap mandatory as first action of every session.
- §45 — `mcp__fabric__search` before any non-trivial decision,
  `mcp__fabric__remember` after. Target band 5–20 searches / hour,
  1–5 remembers / hour.

Full methodology and architecture rationale in
`kronaxis/docs/coord-protocol-v1.md`.

## Postgres credentials

All tools read these env vars, falling back to dev defaults on DL580:

```
KX_PG_HOST    default 192.168.50.129
KX_PG_USER    default titan
KX_PG_DB      default tfs
KX_PG_PASS    default $TFS_DB_PASSWORD or sovereign_hardened_2026
```

Override per-host via shell env or `~/.kronaxis/env`.
