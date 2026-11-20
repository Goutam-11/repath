#!/usr/bin/env python3
"""Runtime audit of the Python wildcard-re-export rung (`py.import.star`).

This is opt-in evidence for issue #105, not a build or CI dependency. It answers
a question the SCIP calibration cannot on `scip-python` 0.6.8: for every edge the
wildcard walk produced, does Python's own import machinery actually expose that
name on that module, and does it resolve to the file repath named?

`scip-python` 0.6.8 mis-resolves module attributes behind a wildcard package
(`models.CharField` is emitted as `django.db.models.fields/DateTimeField#` for
every `models.X`), so a Python member slice cannot be compiler-calibrated. The
identifier slice (`from pkg import Name`) is attributable; this script measures
both slices against the interpreter.

Usage:

    # index once (opt-in, not CI)
    repath index --root .cache/django-60
    python3 scripts/audit-python-star.py \
        --root .cache/django-60 --db .cache/django-60/.repath/index.db \
        --json .cache/audit-star.json

It imports the modules the edges name, so it runs their import-time side effects
and needs a Python interpreter that can import the corpus (django needs its
dependencies for some modules; those land in `import-error` and are counted as
unverifiable, never as false positives). A verdict is one of:

  tp       the interpreter exposes the name and its defining file is the edge's
           target file
  fp       it exposes the name but from another file (or does not expose it)
  error    importing the module failed (unverifiable)
  unknown  no import binding at the referring line matched the edge
  ambiguous  several candidate modules disagreed
"""

import argparse
import collections
import contextlib
import datetime
import importlib
import inspect
import io
import json
import os
import posixpath
import re
import sqlite3
import sys


def parse_args(argv):
    parser = argparse.ArgumentParser(
        description="Audit py.import.star edges against Python's import machinery."
    )
    parser.add_argument("--root", required=True, help="indexed repository root")
    parser.add_argument("--db", help="index database (default: <root>/.repath/index.db)")
    parser.add_argument("--rule", default="py.import.star", help="rule id to audit")
    parser.add_argument("--json", help="write the evidence JSON here")
    parser.add_argument("--max-examples", type=int, default=5, help="examples per verdict")
    return parser.parse_args(argv)


def main(argv):
    args = parse_args(argv)
    root = os.path.abspath(args.root)
    db_path = args.db or os.path.join(root, ".repath", "index.db")
    if not os.path.isdir(root):
        print(f"audit-python-star: root {root} is not a directory", file=sys.stderr)
        return 2
    if not os.path.isfile(db_path):
        print(f"audit-python-star: database {db_path} does not exist (index the root first)",
              file=sys.stderr)
        return 2

    sys.path.insert(0, root)

    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    con.row_factory = sqlite3.Row
    files = {row["id"]: row["path"] for row in con.execute("select id, path from files")}
    nodes = {row["id"]: row for row in con.execute("select id, file_id, qname, name from nodes")}
    imports = collections.defaultdict(list)
    for row in con.execute("select file_id, local, imported, source from imports"):
        imports[files[row["file_id"]]].append(
            (row["local"], row["imported"], row["source"])
        )
    paths = set(files.values())

    def module_path(base):
        base = base.rstrip("/")
        if base == "":
            base = "."
        if base == "." and "__init__.py" in paths:
            return "__init__.py"
        for candidate in (base + ".py", base + "/__init__.py"):
            if candidate in paths:
                return candidate
        return None

    def resolve_module(from_path, spec):
        if not spec:
            return None
        dots = 0
        while dots < len(spec) and spec[dots] == ".":
            dots += 1
        rest = spec[dots:]
        module = rest.replace(".", "/")
        if dots > 0:
            directory = posixpath.dirname(from_path)
            for _ in range(dots - 1):
                directory = posixpath.dirname(directory)
            if module:
                directory = posixpath.join(directory, module)
            return module_path(directory)
        for base in ("", "src", "lib"):
            joined = posixpath.join(base, module) if base else module
            found = module_path(joined)
            if found:
                return found
        return None

    def join_spec(source, name):
        if not source:
            return name
        if source.strip(".") == "":
            return source + name
        return source + "." + name

    def dotted(path):
        name = path[: -len(".py")]
        if name.endswith("/__init__"):
            name = name[: -len("/__init__")]
        return name.replace("/", ".")

    def referring_bindings(src_path, name, line_text):
        """Return ([(local, module_file)], form) for the edge's reference."""
        binds = imports.get(src_path, [])
        candidates = []
        # Member form: `module.name` on the source line.
        for local, imported, source in binds:
            if local == "*":
                continue
            if re.search(r"\b" + re.escape(local) + r"\." + re.escape(name) + r"\b", line_text):
                spec = local if source.split(".")[0] == local else source
                if imported != "*":
                    spec = join_spec(source, imported)
                found = resolve_module(src_path, spec)
                if found:
                    candidates.append((local, found))
        if candidates:
            return candidates, "member"
        # Identifier form: the name itself is the binding.
        for local, imported, source in binds:
            if local == name:
                found = resolve_module(src_path, source)
                if found:
                    return [(local, found)], "ident"
        return [], "unknown"

    namespace_cache = {}
    import_errors = {}
    line_cache = {}

    def namespace(module_file):
        if module_file in namespace_cache:
            return namespace_cache[module_file]
        if module_file in import_errors:
            return None
        try:
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                module = importlib.import_module(dotted(module_file))
        except Exception as exc:  # noqa: BLE001 - any import failure is unverifiable
            import_errors[module_file] = f"{type(exc).__name__}: {exc}"[:120]
            namespace_cache[module_file] = None
            return None
        namespace_cache[module_file] = vars(module)
        return namespace_cache[module_file]

    def defining_file(obj):
        try:
            found = inspect.getsourcefile(obj) or inspect.getfile(obj)
        except Exception:  # noqa: BLE001 - builtins have no source
            return None
        if not found:
            return None
        return os.path.relpath(os.path.abspath(found), root)

    def source_line(path, number):
        key = (path, number)
        if key in line_cache:
            return line_cache[key]
        text = ""
        try:
            with open(os.path.join(root, path), encoding="utf-8", errors="replace") as handle:
                for index, line in enumerate(handle, 1):
                    if index == number:
                        text = line
                        break
        except OSError:
            pass
        line_cache[key] = text
        return text

    rows = list(con.execute(
        """select e.src, e.dst, e.line from edges e where e.rule = ?""", (args.rule,)
    ))
    counts = collections.Counter()
    slices = collections.Counter()
    examples = collections.defaultdict(list)
    for row in rows:
        source = nodes[row["src"]]
        target = nodes[row["dst"]]
        src_path = files[source["file_id"]]
        dst_path = files[target["file_id"]]
        name = target["name"]
        text = source_line(src_path, row["line"])
        modules, form = referring_bindings(src_path, name, text)
        if not modules:
            counts["unknown"] += 1
            slices[form + ":unknown"] += 1
            if len(examples["unknown"]) < args.max_examples:
                examples["unknown"].append(
                    {"file": src_path, "line": row["line"], "name": name,
                     "target": dst_path, "text": text.strip()[:100]}
                )
            continue
        verdicts = []
        for _, mod_file in modules:
            values = namespace(mod_file)
            if values is None:
                verdicts.append(("error", import_errors.get(mod_file, "")))
                continue
            marker = object()
            obj = values.get(name, marker)
            if obj is marker:
                verdicts.append(("missing", ""))
            else:
                verdicts.append(("tp" if defining_file(obj) == dst_path else "fp", ""))
        kinds = {verdict[0] for verdict in verdicts}
        if kinds <= {"tp"}:
            verdict = "tp"
        elif "tp" in kinds:
            verdict = "ambiguous"
        elif "error" in kinds and "fp" not in kinds and "missing" not in kinds:
            verdict = "error"
        elif "missing" in kinds:
            verdict = "missing"
        else:
            verdict = "fp"
        counts[verdict] += 1
        slices[form + ":" + verdict] += 1
        if verdict != "tp" and len(examples[verdict]) < args.max_examples:
            examples[verdict].append(
                {"file": src_path, "line": row["line"], "name": name,
                 "target": dst_path, "text": text.strip()[:100],
                 "modules": [entry[1] for entry in modules],
                 "verdicts": [list(verdict) for verdict in verdicts]}
            )

    total = len(rows)
    checked = counts["tp"] + counts["fp"] + counts["missing"] + counts["ambiguous"]
    payload = {
        "schema": 1,
        "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
        "root": root,
        "db": os.path.abspath(db_path),
        "rule": args.rule,
        "edges": total,
        "checked": checked,
        "counts": dict(sorted(counts.items())),
        "slices": dict(sorted(slices.items())),
        "import_errors": dict(sorted(import_errors.items())[:20]),
        "import_error_total": len(import_errors),
        "examples": dict(examples),
    }
    if args.json:
        with open(args.json, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, indent=2)
            handle.write("\n")

    print(f"rule {args.rule}: {total} edges")
    for key in ("tp", "fp", "missing", "ambiguous", "error", "unknown"):
        print(f"  {key:9s} {counts[key]:6d}")
    print("slices:")
    for key in sorted(slices):
        print(f"  {key:16s} {slices[key]:6d}")
    if import_errors:
        print(f"import errors: {len(import_errors)} module(s), e.g. {list(import_errors)[:3]}")
    if args.json:
        print(f"evidence: {args.json}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
