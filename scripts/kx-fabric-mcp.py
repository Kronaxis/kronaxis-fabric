#!/usr/bin/env python3
"""Kronaxis Fabric MCP shim — bridges Claude Code → fabric HTTP API.

Tools exposed (v0.9):
  - fabric__health
  - fabric__search        (memo search)
  - fabric__remember      (memo create)
  - fabric__symbol_search
  - fabric__symbol_callers
  - fabric__symbol_callees
  - fabric__symbol_context

Speaks MCP stdio (JSON-RPC 2.0). Configure in ~/.claude.json mcpServers.fabric
with command python3 + args [this file path] + env FABRIC_URL + FABRIC_KEY.
"""
from __future__ import annotations

import json
import os
import sys
import traceback
from typing import Any
from urllib import request as urlreq
from urllib.error import HTTPError, URLError

FABRIC_URL = os.environ.get("FABRIC_URL", "http://localhost:8201").rstrip("/")
FABRIC_KEY = os.environ.get("FABRIC_KEY", "")
TIMEOUT = float(os.environ.get("FABRIC_TIMEOUT", "20"))


def _call(method: str, path: str, body: Any | None = None) -> Any:
    url = f"{FABRIC_URL}{path}"
    data = None
    headers = {"Authorization": f"Bearer {FABRIC_KEY}"}
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    req = urlreq.Request(url, data=data, headers=headers, method=method)
    try:
        with urlreq.urlopen(req, timeout=TIMEOUT) as r:
            return json.loads(r.read().decode() or "null")
    except HTTPError as e:
        msg = e.read().decode(errors="replace") if e.fp else str(e)
        raise RuntimeError(f"fabric {method} {path} -> {e.code}: {msg}") from None
    except URLError as e:
        raise RuntimeError(f"fabric {method} {path} unreachable: {e.reason}") from None


def tool_health(_args: dict) -> dict:
    return _call("GET", "/v1/health")


def tool_search(args: dict) -> dict:
    return _call("POST", "/v1/memo/search", {
        "query": args.get("query", ""),
        "top_k": int(args.get("top_k") or 10),
        "type":  args.get("type", ""),
        "mode":  args.get("mode", "hybrid"),
    })


def tool_remember(args: dict) -> dict:
    return _call("POST", "/v1/memo", {
        "title":   args.get("title", ""),
        "content": args.get("content", ""),
        "type":    args.get("type", "general"),
        "tags":    args.get("tags", []),
    })


def tool_symbol_search(args: dict) -> dict:
    return _call("POST", "/v1/symbol/search", {
        "query":       args.get("query", ""),
        "top_k":       int(args.get("top_k") or 10),
        "repo":        args.get("repo", ""),
        "symbol_kind": args.get("symbol_kind", ""),
        "mode":        args.get("mode", "hybrid"),
    })


def tool_symbol_callers(args: dict) -> dict:
    sid = args.get("symbol_id")
    if sid is None:
        raise ValueError("symbol_id required")
    return _call("GET", f"/v1/symbol/{int(sid)}/callers")


def tool_symbol_callees(args: dict) -> dict:
    sid = args.get("symbol_id")
    if sid is None:
        raise ValueError("symbol_id required")
    return _call("GET", f"/v1/symbol/{int(sid)}/callees")


def tool_symbol_context(args: dict) -> dict:
    hits = _call("POST", "/v1/symbol/search", {
        "query":       args.get("query", ""),
        "top_k":       int(args.get("top_k") or 5),
        "repo":        args.get("repo", ""),
        "symbol_kind": args.get("symbol_kind", ""),
        "mode":        args.get("mode", "hybrid"),
    })
    results = hits.get("results") or []
    if not results:
        return {"hit": None, "callers": [], "callees": [], "alternatives": []}
    top = results[0]
    callers = _call("GET", f"/v1/symbol/{int(top['id'])}/callers")
    callees = _call("GET", f"/v1/symbol/{int(top['id'])}/callees")
    return {
        "hit":          top,
        "callers":      callers.get("results") or [],
        "callees":      callees.get("results") or [],
        "alternatives": results[1:],
    }


TOOLS = [
    {
        "name": "fabric__health",
        "description": "Fabric service health: DB status, version, embedding model.",
        "inputSchema": {"type": "object", "properties": {}, "additionalProperties": False},
        "fn": tool_health,
    },
    {
        "name": "fabric__search",
        "description": "Hybrid name+semantic search over Fabric memos. Returns ranked excerpts.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "query": {"type": "string"},
                "top_k": {"type": "integer", "minimum": 1, "maximum": 50},
                "type":  {"type": "string", "description": "filter by memo type"},
                "mode":  {"type": "string", "enum": ["hybrid", "tsvector", "semantic"]},
            },
            "required": ["query"],
        },
        "fn": tool_search,
    },
    {
        "name": "fabric__remember",
        "description": "Persist a memo to Fabric (deduplicates on sha256).",
        "inputSchema": {
            "type": "object",
            "properties": {
                "title":   {"type": "string"},
                "content": {"type": "string"},
                "type":    {"type": "string"},
                "tags":    {"type": "array", "items": {"type": "string"}},
            },
            "required": ["content"],
        },
        "fn": tool_remember,
    },
    {
        "name": "fabric__symbol_search",
        "description": "Hybrid name+semantic search over indexed code symbols. Returns top N matches with file_path, line_start/end, signature, kind.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "query":       {"type": "string"},
                "top_k":       {"type": "integer", "minimum": 1, "maximum": 50},
                "repo":        {"type": "string"},
                "symbol_kind": {"type": "string", "description": "filter by kind: function|method|struct|interface|class|const|var|type"},
                "mode":        {"type": "string", "enum": ["hybrid", "semantic", "name"]},
            },
            "required": ["query"],
        },
        "fn": tool_symbol_search,
    },
    {
        "name": "fabric__symbol_callers",
        "description": "Who calls this symbol? Returns incoming edges.",
        "inputSchema": {
            "type": "object",
            "properties": {"symbol_id": {"type": "integer"}},
            "required": ["symbol_id"],
        },
        "fn": tool_symbol_callers,
    },
    {
        "name": "fabric__symbol_callees",
        "description": "What does this symbol call? Returns outgoing edges.",
        "inputSchema": {
            "type": "object",
            "properties": {"symbol_id": {"type": "integer"}},
            "required": ["symbol_id"],
        },
        "fn": tool_symbol_callees,
    },
    {
        "name": "fabric__symbol_context",
        "description": "One-call code context: searches for the symbol, returns top hit + callers + callees grouped + runner-up matches as alternatives.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "query":       {"type": "string"},
                "repo":        {"type": "string"},
                "symbol_kind": {"type": "string"},
                "top_k":       {"type": "integer", "minimum": 1, "maximum": 20},
                "mode":        {"type": "string", "enum": ["hybrid", "semantic", "name"]},
            },
            "required": ["query"],
        },
        "fn": tool_symbol_context,
    },
]
TOOL_INDEX = {t["name"]: t for t in TOOLS}


def send(msg: dict) -> None:
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()


def respond_ok(req_id, result):
    send({"jsonrpc": "2.0", "id": req_id, "result": result})


def respond_err(req_id, code, message):
    send({"jsonrpc": "2.0", "id": req_id, "error": {"code": code, "message": message}})


def handle(msg: dict) -> None:
    method = msg.get("method", "")
    req_id = msg.get("id")
    if method == "initialize":
        respond_ok(req_id, {
            "protocolVersion": "2024-11-05",
            "serverInfo": {"name": "kx-fabric-mcp", "version": "0.9.0"},
            "capabilities": {"tools": {"listChanged": False}},
        })
        return
    if method in ("notifications/initialized", "initialized"):
        return
    if method == "tools/list":
        respond_ok(req_id, {"tools": [
            {"name": t["name"], "description": t["description"], "inputSchema": t["inputSchema"]}
            for t in TOOLS
        ]})
        return
    if method == "tools/call":
        params = msg.get("params") or {}
        name = params.get("name", "")
        args = params.get("arguments") or {}
        tool = TOOL_INDEX.get(name)
        if not tool:
            respond_err(req_id, -32602, f"unknown tool: {name}")
            return
        try:
            result = tool["fn"](args)
            respond_ok(req_id, {"content": [{"type": "text", "text": json.dumps(result, default=str)}]})
        except Exception as e:
            respond_err(req_id, -32000, f"{name}: {e}")
        return
    if req_id is not None:
        respond_err(req_id, -32601, f"method not found: {method}")


def main() -> int:
    if not FABRIC_KEY:
        sys.stderr.write("kx-fabric-mcp: FABRIC_KEY env var required\n")
        return 2
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue
        try:
            handle(msg)
        except Exception:
            traceback.print_exc(file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
