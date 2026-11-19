#!/usr/bin/env python3
"""Measure a repath graph and a Graphify graph against SCIP compiler truth.

Opt-in evidence, in the spirit of `make bench-*`: it needs a SCIP index that an
external producer already made (repath never generates one), and it is not part
of `make check`.

    # 1. one-time protobuf bindings (no repo dependency; generated locally)
    curl -sSLO https://raw.githubusercontent.com/sourcegraph/scip/main/scip.proto
    python -m grpc_tools.protoc -I. --python_out=. scip.proto   # needs grpcio-tools

    # 2. run it against an indexed repo
    python scripts/scip-accuracy.py \
        --root .cache/requests \
        --scip .cache/requests-full.scip \
        --graphify .cache/gfy-requests/graphify-out/graph.json \
        --repath-db .cache/requests/.repath/index.db \
        --scope src/requests/ --targets 40

Method: a SCIP reference occurrence is a *call* when the character after the
symbol is `(`; its caller is the innermost `def`/`class` on that line, computed
with `ast` so no tool's scope model is used as truth. Targets are joined to a
tool by definition location (`file:line`, +/-2 lines for decorators), never by
name. Both tools are scope-matched to the SCIP document set. Two metrics:

  calls  the caller called the target
  refs   the caller also merely referenced it (annotation, callback, class base)

repath contributes `CALLS`/`REFERENCES`; Graphify contributes
`calls`/`indirect_call`/`references`/`uses`. `Cls()` construction is a `calls`
edge for Graphify and a `REFERENCES` edge for repath, which is why both metrics
are printed.
"""
from __future__ import annotations

import argparse
import ast
import collections
import json
import sqlite3
import sys
from pathlib import Path

try:
    import scip_pb2  # generated from scip.proto; see the header
except ImportError:  # pragma: no cover - documented setup step
    sys.exit("scip_pb2 is missing: generate it from scip.proto (see the docstring)")


def enclosing_map(path: Path):
    """line -> (def start line, name, ast node type) for the innermost definition."""
    tree = ast.parse(path.read_text(errors="replace"))
    out = {}

    def walk(node, prefix):
        for child in ast.iter_child_nodes(node):
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                for line in range(child.lineno, getattr(child, "end_lineno", child.lineno) + 1):
                    out.setdefault(line, []).append((child.lineno, prefix + child.name, type(child).__name__))
                walk(child, f"{prefix}{child.name}.")
            else:
                walk(child, prefix)

    walk(tree, "")
    return {line: max(vals, key=lambda v: v[0]) for line, vals in out.items() if vals}


def load_truth(root: Path, scip: Path):
    idx = scip_pb2.Index()
    idx.ParseFromString(scip.read_bytes())
    defs, refs = {}, collections.defaultdict(list)
    for doc in idx.documents:
        path = doc.relative_path
        src_path = root / path
        if not src_path.exists():
            continue
        lines = src_path.read_text(errors="replace").splitlines()
        try:
            enc = enclosing_map(src_path)
        except SyntaxError:
            enc = {}
        for occ in doc.occurrences:
            if occ.symbol_roles & 1:  # Definition
                line1 = occ.range[0] + 1
                e = enc.get(line1)
                defs.setdefault((path, line1), {
                    "symbol": occ.symbol,
                    "starts": bool(e) and e[0] == line1,
                    "kind": e[2] if e else None,
                })
        for occ in doc.occurrences:
            if occ.symbol_roles & 1 or occ.symbol_roles & 2:  # definition or import
                continue
            line0, col1 = occ.range[0], occ.range[2]
            src = lines[line0] if line0 < len(lines) else ""
            tail = src[col1:col1 + 4].lstrip() if col1 <= len(src) else ""
            caller = enc.get(line0 + 1)
            refs[occ.symbol].append({
                "path": path,
                "line": line0 + 1,
                "call": tail.startswith("("),
                "caller": (path, caller[0]) if caller else None,
            })
    return defs, refs


def load_repath(db: Path):
    con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    nodes, by_loc, edges = {}, collections.defaultdict(list), collections.defaultdict(list)
    for nid, qname, kind, path, start in con.execute(
            "SELECT n.id,n.qname,n.kind,f.path,n.start_line FROM nodes n "
            "JOIN files f ON f.id=n.file_id"):
        nodes[nid] = {"qname": qname, "kind": kind, "file": path, "line": start}
        by_loc[(path, start)].append(nid)
    for src, dst, kind, conf in con.execute(
            "SELECT src,dst,kind,confidence FROM edges WHERE confidence>=0.5"):
        edges[dst].append({"src": src, "kind": kind, "conf": conf})
    return nodes, by_loc, edges


def load_graphify(graph: Path):
    g = json.loads(graph.read_text())
    nodes, by_loc, incoming = {}, collections.defaultdict(list), collections.defaultdict(list)
    for n in g["nodes"]:
        nodes[n["id"]] = n
        loc = n.get("source_location") or ""
        if loc.startswith("L"):
            by_loc[(n.get("source_file"), int(loc[1:]))].append(n["id"])
    for link in g["links"]:
        incoming[link["target"]].append(link)
    return nodes, by_loc, incoming


def match(gt: set, tool: set, tol: int = 2):
    """Greedy (file, line) matching within `tol` lines; returns tp, fp, fn."""
    gt_by_file, tool_by_file = collections.defaultdict(list), collections.defaultdict(list)
    for f, l in gt:
        gt_by_file[f].append(l)
    for f, l in tool:
        tool_by_file[f].append(l)
    tp = 0
    for f, gt_lines in gt_by_file.items():
        candidates = sorted(tool_by_file.get(f, []))
        used = [False] * len(candidates)
        for gl in sorted(gt_lines):
            for i, tl in enumerate(candidates):
                if not used[i] and abs(tl - gl) <= tol:
                    used[i] = True
                    tp += 1
                    break
    return tp, len(tool) - tp, len(gt) - tp


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--root", required=True, type=Path, help="checked-out repo the SCIP index describes")
    ap.add_argument("--scip", required=True, type=Path)
    ap.add_argument("--graphify", required=True, type=Path, help="graphify-out/graph.json")
    ap.add_argument("--repath-db", required=True, type=Path, help=".repath/index.db")
    ap.add_argument("--scope", default="", help="path prefix to compare, e.g. src/pkg/")
    ap.add_argument("--targets", type=int, default=20)
    ap.add_argument("--json", type=Path, help="write the per-target rows here")
    args = ap.parse_args()

    defs, refs = load_truth(args.root, args.scip)
    rp_nodes, rp_loc, rp_edges = load_repath(args.repath_db)
    gy_nodes, gy_loc, gy_edges = load_graphify(args.graphify)
    scope = {path for (path, _line) in defs}

    targets = []
    for (path, line), meta in sorted(defs.items()):
        if not path.startswith(args.scope) or not (meta["starts"] and meta["kind"]):
            continue
        symbol = meta["symbol"]
        callers = {r["caller"] for r in refs.get(symbol, []) if r["caller"]}
        calls = {r["caller"] for r in refs.get(symbol, []) if r["caller"] and r["call"]}
        if callers:
            targets.append({"file": path, "line": line, "callers": callers, "calls": calls})

    step = max(1, len(targets) // args.targets)
    sample = targets[::step][:args.targets]
    print(f"targets in scope: {len(targets)}; sampled: {len(sample)}", file=sys.stderr)

    totals = {(t, m): [0, 0, 0] for t in ("repath", "graphify") for m in ("refs", "calls")}
    rows = []
    for t in sample:
        key = (t["file"], t["line"])
        rp_sets = {m: set() for m in ("refs", "calls")}
        for nid in rp_loc.get(key, []):
            for edge in rp_edges.get(nid, []):
                caller = rp_nodes.get(edge["src"])
                if not caller:
                    continue
                loc = (caller["file"], caller["line"])
                if caller["file"] not in scope or loc == key:
                    continue
                if edge["kind"] in ("CALLS", "REFERENCES"):
                    rp_sets["refs"].add(loc)
                if edge["kind"] == "CALLS":
                    rp_sets["calls"].add(loc)
        gy_sets = {m: set() for m in ("refs", "calls")}
        for nid in gy_loc.get(key, []):
            for link in gy_edges.get(nid, []):
                source = gy_nodes.get(link["source"]) or {}
                loc_text = source.get("source_location") or ""
                if not loc_text.startswith("L"):
                    continue
                loc = (source.get("source_file"), int(loc_text[1:]))
                if loc[0] not in scope or loc == key:
                    continue
                relation = link.get("relation")
                if relation in ("calls", "indirect_call", "references", "uses"):
                    gy_sets["refs"].add(loc)
                if relation in ("calls", "indirect_call"):
                    gy_sets["calls"].add(loc)

        row = {"target": f"{t['file']}:{t['line']}",
               "qname": rp_nodes[rp_loc[key][0]]["qname"] if rp_loc.get(key) else None}
        for metric, gt in (("refs", t["callers"]), ("calls", t["calls"])):
            for tool, sets in (("repath", rp_sets), ("graphify", gy_sets)):
                tp, fp, fn = match(gt, sets[metric])
                for i, value in enumerate((tp, fp, fn)):
                    totals[(tool, metric)][i] += value
                row[f"{tool}_{metric}"] = {"tp": tp, "fp": fp, "fn": fn}
        rows.append(row)
        print(f"{row['target']:48} {(row['qname'] or '-')[:30]:30} "
              f"gt_calls={len(t['calls']):2} repath={row['repath_calls']['tp']}/"
              f"{row['repath_calls']['tp'] + row['repath_calls']['fp']} "
              f"graphify={row['graphify_calls']['tp']}/"
              f"{row['graphify_calls']['tp'] + row['graphify_calls']['fp']}")

    print()
    out = {"targets": rows, "totals": {}}
    for metric in ("refs", "calls"):
        for tool in ("repath", "graphify"):
            tp, fp, fn = totals[(tool, metric)]
            precision = tp / (tp + fp) if tp + fp else 0.0
            recall = tp / (tp + fn) if tp + fn else 0.0
            f1 = 2 * precision * recall / (precision + recall) if precision + recall else 0.0
            print(f"{metric:5} {tool:9} tp={tp:3} fp={fp:3} fn={fn:3} "
                  f"precision={precision:.3f} recall={recall:.3f} f1={f1:.3f}")
            out["totals"][f"{tool}_{metric}"] = {
                "tp": tp, "fp": fp, "fn": fn,
                "precision": round(precision, 4), "recall": round(recall, 4), "f1": round(f1, 4)}

    if args.json:
        args.json.write_text(json.dumps(out, indent=1))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
