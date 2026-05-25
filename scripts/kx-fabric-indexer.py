#!/usr/bin/env python3
"""Kronaxis Fabric code-graph indexer (v0.5).

Walks a repo with tree-sitter, extracts symbols (Go + Python tier-1),
upserts them via /v1/symbol, and records call edges via /v1/symbol/edge.

Usage:
    FABRIC_KEY=... ./kx-fabric-indexer.py --repo kronaxis-fabric --root /home/jason/projects/kronaxis-fabric

Requires: tree-sitter, tree-sitter-go, tree-sitter-python, requests.
Install:  pip install tree-sitter tree-sitter-go tree-sitter-python requests
"""

from __future__ import annotations

import argparse
import os
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable

import requests
from tree_sitter import Language, Node, Parser
import tree_sitter_go
import tree_sitter_python

GO_LANGUAGE = Language(tree_sitter_go.language())
PY_LANGUAGE = Language(tree_sitter_python.language())


@dataclass
class Symbol:
    repo: str
    file_path: str
    symbol_name: str
    symbol_kind: str
    language: str
    line_start: int
    line_end: int
    signature: str
    docstring: str


@dataclass
class Edge:
    src_name: str  # we resolve src_name → id after upserting all symbols
    dst_name: str
    edge_kind: str
    file_path: str  # for disambiguation within a single file


def text(node: Node, source: bytes) -> str:
    return source[node.start_byte:node.end_byte].decode("utf-8", errors="replace")


def first_line(s: str) -> str:
    s = s.strip()
    if "\n" in s:
        return s.split("\n", 1)[0].strip()
    return s


# ---------- Go ----------

def parse_go_file(repo: str, rel: str, source: bytes) -> tuple[list[Symbol], list[Edge]]:
    parser = Parser(GO_LANGUAGE)
    tree = parser.parse(source)
    root = tree.root_node
    symbols: list[Symbol] = []
    edges: list[Edge] = []

    def walk_top(node: Node, enclosing: str | None) -> None:
        if node.type == "function_declaration":
            name_node = node.child_by_field_name("name")
            if not name_node:
                return
            name = text(name_node, source)
            sig = first_line(text(node, source))
            symbols.append(Symbol(repo, rel, name, "function", "go",
                                  node.start_point[0] + 1, node.end_point[0] + 1,
                                  sig[:300], ""))
            collect_go_calls(node, name, edges, rel, source)
        elif node.type == "method_declaration":
            name_node = node.child_by_field_name("name")
            if not name_node:
                return
            name = text(name_node, source)
            sig = first_line(text(node, source))
            symbols.append(Symbol(repo, rel, name, "method", "go",
                                  node.start_point[0] + 1, node.end_point[0] + 1,
                                  sig[:300], ""))
            collect_go_calls(node, name, edges, rel, source)
        elif node.type == "type_declaration":
            for spec in node.children:
                if spec.type == "type_spec":
                    name_node = spec.child_by_field_name("name")
                    type_node = spec.child_by_field_name("type")
                    if not name_node:
                        continue
                    kind = "type"
                    if type_node is not None:
                        if type_node.type == "struct_type":
                            kind = "struct"
                        elif type_node.type == "interface_type":
                            kind = "interface"
                    sig = first_line(text(spec, source))
                    symbols.append(Symbol(repo, rel, text(name_node, source), kind, "go",
                                          spec.start_point[0] + 1, spec.end_point[0] + 1,
                                          sig[:300], ""))
        elif node.type == "const_declaration":
            for spec in node.children:
                if spec.type == "const_spec":
                    for nm in spec.children_by_field_name("name"):
                        symbols.append(Symbol(repo, rel, text(nm, source), "const", "go",
                                              spec.start_point[0] + 1, spec.end_point[0] + 1,
                                              first_line(text(spec, source))[:300], ""))
        elif node.type == "var_declaration":
            for spec in node.children:
                if spec.type == "var_spec":
                    for nm in spec.children_by_field_name("name"):
                        symbols.append(Symbol(repo, rel, text(nm, source), "var", "go",
                                              spec.start_point[0] + 1, spec.end_point[0] + 1,
                                              first_line(text(spec, source))[:300], ""))

    for c in root.children:
        walk_top(c, None)
    return symbols, edges


def collect_go_calls(fn_node: Node, fn_name: str, edges: list[Edge], rel: str, source: bytes) -> None:
    # Walk all call_expression nodes inside fn_node.
    stack = [fn_node]
    while stack:
        n = stack.pop()
        if n.type == "call_expression":
            callee = n.child_by_field_name("function")
            if callee is not None:
                callee_name = extract_callee_name(callee, source)
                if callee_name and callee_name != fn_name:
                    edges.append(Edge(fn_name, callee_name, "calls", rel))
        for child in n.children:
            stack.append(child)


def extract_callee_name(node: Node, source: bytes) -> str | None:
    if node.type == "identifier":
        return text(node, source)
    if node.type == "selector_expression":
        field = node.child_by_field_name("field")
        if field:
            return text(field, source)
    return None


# ---------- Python ----------

def parse_python_file(repo: str, rel: str, source: bytes) -> tuple[list[Symbol], list[Edge]]:
    parser = Parser(PY_LANGUAGE)
    tree = parser.parse(source)
    root = tree.root_node
    symbols: list[Symbol] = []
    edges: list[Edge] = []

    def walk(node: Node, class_name: str | None) -> None:
        if node.type == "function_definition":
            name_node = node.child_by_field_name("name")
            if name_node:
                name = text(name_node, source)
                if class_name:
                    name = f"{class_name}.{name}"
                sig = first_line(text(node, source))
                doc = extract_py_docstring(node, source)
                kind = "method" if class_name else "function"
                symbols.append(Symbol(repo, rel, name, kind, "python",
                                      node.start_point[0] + 1, node.end_point[0] + 1,
                                      sig[:300], doc[:500]))
                collect_py_calls(node, name, edges, rel, source)
        elif node.type == "class_definition":
            name_node = node.child_by_field_name("name")
            if name_node:
                cname = text(name_node, source)
                sig = first_line(text(node, source))
                doc = extract_py_docstring(node, source)
                symbols.append(Symbol(repo, rel, cname, "class", "python",
                                      node.start_point[0] + 1, node.end_point[0] + 1,
                                      sig[:300], doc[:500]))
                body = node.child_by_field_name("body")
                if body:
                    for c in body.children:
                        walk(c, cname)
                return
        for child in node.children:
            walk(child, class_name)

    walk(root, None)
    return symbols, edges


def extract_py_docstring(node: Node, source: bytes) -> str:
    body = node.child_by_field_name("body")
    if not body:
        return ""
    for c in body.children:
        if c.type == "expression_statement":
            if c.children and c.children[0].type == "string":
                raw = text(c.children[0], source).strip()
                # strip surrounding triple-quotes
                for q in ('"""', "'''", '"', "'"):
                    if raw.startswith(q) and raw.endswith(q):
                        return raw[len(q):-len(q)].strip()
                return raw
            return ""
        if c.type != "comment":
            return ""
    return ""


def collect_py_calls(fn_node: Node, fn_name: str, edges: list[Edge], rel: str, source: bytes) -> None:
    stack = [fn_node]
    while stack:
        n = stack.pop()
        if n.type == "call":
            callee = n.child_by_field_name("function")
            if callee is not None:
                name = extract_py_callee_name(callee, source)
                if name and name != fn_name:
                    edges.append(Edge(fn_name, name, "calls", rel))
        for child in n.children:
            stack.append(child)


def extract_py_callee_name(node: Node, source: bytes) -> str | None:
    if node.type == "identifier":
        return text(node, source)
    if node.type == "attribute":
        attr = node.child_by_field_name("attribute")
        if attr:
            return text(attr, source)
    return None


# ---------- IO + upload ----------

EXTENSIONS = {".go": parse_go_file, ".py": parse_python_file}
IGNORE_DIRS = {".git", "node_modules", "vendor", "__pycache__", ".venv", "venv", "dist", "build"}


def walk_repo(root: Path) -> Iterable[tuple[Path, str]]:
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in IGNORE_DIRS]
        for fn in filenames:
            p = Path(dirpath) / fn
            ext = p.suffix.lower()
            if ext in EXTENSIONS:
                yield p, ext


def main() -> int:
    ap = argparse.ArgumentParser(description="Kronaxis Fabric code graph indexer")
    ap.add_argument("--repo", required=True, help="logical repo name (e.g. kronaxis-fabric)")
    ap.add_argument("--root", required=True, help="filesystem root to walk")
    ap.add_argument("--fabric-url", default=os.environ.get("FABRIC_URL", "http://localhost:8201"))
    ap.add_argument("--key", default=os.environ.get("FABRIC_KEY", ""))
    ap.add_argument("--no-edges", action="store_true", help="skip edge upload (symbols only)")
    ap.add_argument("--file", default="", help="index only this file (path relative to --root); used by fabric-watcher")
    args = ap.parse_args()

    if not args.key:
        sys.stderr.write("FABRIC_KEY env var or --key required\n")
        return 2

    root = Path(args.root).resolve()
    if not root.is_dir():
        sys.stderr.write(f"--root not a directory: {root}\n")
        return 2

    headers = {"Authorization": f"Bearer {args.key}", "Content-Type": "application/json"}
    session = requests.Session()
    session.headers.update(headers)

    # Decide what to walk: either a single file (watcher mode) or the whole tree.
    if args.file:
        target = (root / args.file).resolve()
        if not target.is_file():
            sys.stderr.write(f"--file not found: {target}\n")
            return 2
        ext = target.suffix.lower()
        if ext not in EXTENSIONS:
            sys.stderr.write(f"--file extension {ext} not indexed; skipping\n")
            return 0
        targets = [(target, ext)]
    else:
        targets = list(walk_repo(root))

    all_symbols: list[Symbol] = []
    all_edges: list[Edge] = []
    files_seen = 0
    for path, ext in targets:
        rel = str(path.relative_to(root))
        try:
            source = path.read_bytes()
        except OSError as e:
            sys.stderr.write(f"skip {rel}: {e}\n")
            continue
        # Clear previous symbols for this file so re-runs don't leak ghosts.
        session.post(f"{args.fabric_url}/v1/symbol/reindex",
                     json={"repo": args.repo, "file_path": rel}, timeout=10)
        syms, edges = EXTENSIONS[ext](args.repo, rel, source)
        all_symbols.extend(syms)
        all_edges.extend(edges)
        files_seen += 1

    print(f"parsed {files_seen} files → {len(all_symbols)} symbols, {len(all_edges)} edge candidates")

    # Upsert symbols and remember id by (file_path, symbol_name).
    id_map: dict[tuple[str, str], int] = {}
    for sym in all_symbols:
        body = {
            "repo": sym.repo,
            "file_path": sym.file_path,
            "symbol_name": sym.symbol_name,
            "symbol_kind": sym.symbol_kind,
            "language": sym.language,
            "line_start": sym.line_start,
            "line_end": sym.line_end,
            "signature": sym.signature,
            "docstring": sym.docstring,
        }
        r = session.post(f"{args.fabric_url}/v1/symbol", json=body, timeout=30)
        if r.status_code != 200:
            sys.stderr.write(f"upsert fail {sym.file_path}:{sym.symbol_name}: {r.status_code} {r.text}\n")
            continue
        id_map[(sym.file_path, sym.symbol_name)] = r.json()["id"]

    print(f"upserted {len(id_map)} symbols")

    if args.no_edges:
        return 0

    # Resolve edges. Prefer same-file resolution; fall back to any symbol with the matching name.
    name_index: dict[str, list[int]] = {}
    for (fp, name), sid in id_map.items():
        name_index.setdefault(name, []).append(sid)

    edges_posted = 0
    edges_skipped = 0
    for e in all_edges:
        src_id = id_map.get((e.file_path, e.src_name))
        if src_id is None:
            edges_skipped += 1
            continue
        # Resolve dst: first same-file, then global by name.
        dst_id = id_map.get((e.file_path, e.dst_name))
        if dst_id is None:
            candidates = name_index.get(e.dst_name, [])
            if len(candidates) == 1:
                dst_id = candidates[0]
        if dst_id is None or dst_id == src_id:
            edges_skipped += 1
            continue
        r = session.post(f"{args.fabric_url}/v1/symbol/edge",
                         json={"src_id": src_id, "dst_id": dst_id, "edge_kind": e.edge_kind},
                         timeout=10)
        if r.status_code == 200:
            edges_posted += 1
        else:
            edges_skipped += 1

    print(f"posted {edges_posted} edges, skipped {edges_skipped} unresolved")
    return 0


if __name__ == "__main__":
    sys.exit(main())
