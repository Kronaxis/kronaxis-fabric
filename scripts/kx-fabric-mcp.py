#!/usr/bin/env python3
"""
kx-fabric-mcp — MCP stdio server for kronaxis-fabric.

Wraps the fabric HTTP API as MCP tools so Claude Code sessions can call
fabric.search / fabric.remember / fabric.health directly.

Wire into ~/.claude.json:
  "mcpServers": {
    "fabric": {
      "command": "python3",
      "args": ["/path/to/kx-fabric-mcp.py"],
      "env": {
        "FABRIC_URL": "http://localhost:8201",
        "FABRIC_KEY": "<your-fabric-bearer-token>"
      }
    }
  }
"""
import os
import json
import sys
import urllib.request
import urllib.error
from mcp.server.fastmcp import FastMCP

FABRIC_URL = os.environ.get('FABRIC_URL', 'http://localhost:8201').rstrip('/')
FABRIC_KEY = os.environ.get('FABRIC_KEY', '')
if not FABRIC_KEY:
    print("FABRIC_KEY env var required", file=sys.stderr)
    sys.exit(1)

mcp = FastMCP("fabric")


def _post(path: str, body: dict, timeout: int = 10) -> dict:
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        f"{FABRIC_URL}{path}",
        data=data,
        headers={'Authorization': f'Bearer {FABRIC_KEY}', 'Content-Type': 'application/json'},
        method='POST',
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        return {'error': f"HTTP {e.code}: {e.read().decode(errors='replace')[:200]}"}
    except Exception as e:
        return {'error': f"{type(e).__name__}: {e}"}


def _get(path: str, timeout: int = 5) -> dict:
    req = urllib.request.Request(
        f"{FABRIC_URL}{path}",
        headers={'Authorization': f'Bearer {FABRIC_KEY}'},
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        return {'error': f"HTTP {e.code}: {e.read().decode(errors='replace')[:200]}"}
    except Exception as e:
        return {'error': f"{type(e).__name__}: {e}"}


@mcp.tool()
def search(query: str, top_k: int = 10, type: str = "") -> dict:
    """
    Search fabric memos by semantic + recency rank (tsvector).
    Returns list of {id, title, excerpt, score, type, created_at}.

    Args:
      query: search terms (English; uses Postgres plainto_tsquery)
      top_k: max results (default 10)
      type: optional filter — general|reference|project|feedback|user
    """
    body = {"query": query, "top_k": top_k}
    if type:
        body["type"] = type
    return _post("/v1/memo/search", body)


@mcp.tool()
def remember(content: str, title: str = "", type: str = "general", tags: list = None) -> dict:
    """
    Create or upsert a memo in fabric. Dedup via sha256(title + content).
    Returns {id, sha256, deduped}.

    Args:
      content: the memo body (required)
      title: short description (default: empty)
      type: general|reference|project|feedback|user (default general)
      tags: optional list of tag strings
    """
    body = {"content": content, "title": title, "type": type, "tags": tags or []}
    return _post("/v1/memo", body)


@mcp.tool()
def health() -> dict:
    """Check fabric server health + version + db status."""
    return _get("/v1/health")


if __name__ == "__main__":
    mcp.run()
