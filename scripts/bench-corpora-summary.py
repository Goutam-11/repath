#!/usr/bin/env python3
"""Aggregate repath corpus runs into summary.json, deltas, and a baseline.

Called by scripts/bench-corpora.sh; not a build dependency and not run in CI.
The shell driver owns cloning, producer availability, and the go test
invocations. This script only reads <outdir>/runs/*.json and writes:

  <outdir>/summary.json          always (aggregated medians/ranges + host/tools)
  bench/baseline.json            only with --update-baseline
  docs/benchmarks-history.md     only with --update-baseline

It prints deltas against an existing baseline on every run. It never touches
tracked files unless --update-baseline is passed explicitly.
"""

import argparse
import json
import os
import platform
import re
import statistics
import subprocess
import sys
from datetime import datetime, timezone

SCHEMA = 1

# Flat metric name -> accessor over one run JSON. Keep this list in sync with
# internal/index/bench_corpora_test.go and docs/benchmarks.md.
METRIC_ACCESSORS = {
    "cold_index_ms": lambda r: r.get("cold_index_ms"),
    "peak_rss_bytes": lambda r: r.get("peak_rss_bytes"),
    "incremental_ms": lambda r: r.get("incremental_ms"),
    "impact_p50_ms": lambda r: r.get("queries", {}).get("impact", {}).get("p50_ms"),
    "impact_p95_ms": lambda r: r.get("queries", {}).get("impact", {}).get("p95_ms"),
    "callers_p50_ms": lambda r: r.get("queries", {}).get("callers", {}).get("p50_ms"),
    "callers_p95_ms": lambda r: r.get("queries", {}).get("callers", {}).get("p95_ms"),
    "path_p50_ms": lambda r: r.get("queries", {}).get("path", {}).get("p50_ms"),
    "path_p95_ms": lambda r: r.get("queries", {}).get("path", {}).get("p95_ms"),
    "map_p50_ms": lambda r: r.get("queries", {}).get("map", {}).get("p50_ms"),
    "map_p95_ms": lambda r: r.get("queries", {}).get("map", {}).get("p95_ms"),
    "check_ms": lambda r: r.get("check", {}).get("duration_ms"),
    "check_findings": lambda r: r.get("check", {}).get("findings"),
    "artifact_bytes": lambda r: r.get("artifact", {}).get("uncompressed_bytes"),
    "artifact_export_ms": lambda r: r.get("artifact", {}).get("export_ms"),
    "artifact_pull_ms": lambda r: r.get("artifact", {}).get("pull_ms"),
    "artifact_zstd_bytes": lambda r: r.get("artifact", {}).get("zstd_bytes"),
    "artifact_zstd_export_ms": lambda r: r.get("artifact", {}).get("zstd_export_ms"),
    "artifact_zstd_pull_ms": lambda r: r.get("artifact", {}).get("zstd_pull_ms"),
    "artifact_ratio": lambda r: r.get("artifact", {}).get("ratio"),
    "adapter_wall_ms": lambda r: (r.get("adapter") or {}).get("wall_ms"),
    "adapter_matched": lambda r: (r.get("adapter") or {}).get("matched"),
    "adapter_replaced": lambda r: (r.get("adapter") or {}).get("replaced"),
    "adapter_replaced_edges": lambda r: (r.get("adapter") or {}).get("replaced_edges"),
    "adapter_edges": lambda r: (r.get("adapter") or {}).get("edges"),
    "adapter_conflicts": lambda r: (r.get("adapter") or {}).get("conflicts"),
    "adapter_unresolved_before": lambda r: (r.get("adapter") or {}).get("unresolved_before"),
    "adapter_unresolved_after": lambda r: (r.get("adapter") or {}).get("unresolved_after"),
    "adapter_edges_before": lambda r: (r.get("adapter") or {}).get("edges_before"),
    "adapter_edges_after": lambda r: (r.get("adapter") or {}).get("edges_after"),
}

SAMPLE_ACCESSORS = {
    "impact_samples": lambda r: r.get("queries", {}).get("impact", {}).get("samples"),
    "callers_samples": lambda r: r.get("queries", {}).get("callers", {}).get("samples"),
    "path_samples": lambda r: r.get("queries", {}).get("path", {}).get("samples"),
    "map_samples": lambda r: r.get("queries", {}).get("map", {}).get("samples"),
}

STRUCTURE_KEYS = [
    "files",
    "nodes",
    "edges_total",
    "unresolved",
    "parse_failures",
    "ambiguous",
    "entrypoints",
]


def die(message):
    print(f"bench-corpora: {message}", file=sys.stderr)
    raise SystemExit(1)


def read_matrix(path):
    rows = []
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            line = line.rstrip("\n")
            if not line:
                continue
            name, lang, repo, commit, root, scip, accuracy = line.split("\t")
            rows.append(
                {
                    "name": name,
                    "language": lang,
                    "repo": repo,
                    "commit": commit,
                    "root": root,
                    "scip": scip or None,
                    "accuracy": accuracy == "1",
                }
            )
    return rows


def tool_version(command):
    try:
        out = subprocess.run(
            command, capture_output=True, text=True, timeout=30, check=False
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    text = (out.stdout or out.stderr).strip().splitlines()
    return text[0] if text else None


def host_info():
    mem_bytes = None
    try:
        with open("/proc/meminfo", encoding="utf-8") as handle:
            for line in handle:
                if line.startswith("MemTotal:"):
                    mem_bytes = int(line.split()[1]) * 1024
                    break
    except OSError:
        pass
    cpu_model = None
    try:
        with open("/proc/cpuinfo", encoding="utf-8") as handle:
            for line in handle:
                if line.startswith("model name"):
                    cpu_model = line.split(":", 1)[1].strip()
                    break
    except OSError:
        pass
    try:
        loadavg = [float(v) for v in open("/proc/loadavg", encoding="utf-8").read().split()[:3]]
    except OSError:
        loadavg = None
    uname = platform.uname()

    def loadavg_env(name):
        raw = os.environ.get(name, "").strip()
        if not raw:
            return None
        try:
            return [float(value) for value in raw.split()[:3]]
        except ValueError:
            return None

    return {
        "hostname": platform.node(),
        "platform": f"{uname.system} {uname.machine}",
        "kernel": uname.release,
        "cpu_model": cpu_model,
        "cpu_logical": os.cpu_count(),
        "ram_bytes": mem_bytes,
        "loadavg_1_5_15": loadavg,
        "loadavg_before": loadavg_env("REPATH_BENCH_LOAD_BEFORE"),
        "loadavg_after": loadavg_env("REPATH_BENCH_LOAD_AFTER"),
        "python": platform.python_version(),
    }


def tools_info():
    return {
        "go": tool_version(["go", "version"]),
        "rustc": tool_version(["rustc", "--version"]),
        "python": tool_version(["python3", "--version"]),
        "node": tool_version(["node", "--version"]),
        "git": tool_version(["git", "--version"]),
    }


def median_range(values):
    if not values:
        return None
    return {
        "median": statistics.median(values),
        "min": min(values),
        "max": max(values),
        "runs": len(values),
        "values": values,
    }


def phase_summary(phase):
    if not phase:
        return None
    keys = [
        "phase",
        "samples",
        "expected_callers",
        "actual_callers",
        "true_positives",
        "precision",
        "recall",
        "out_of_scope_actual",
        "impact_expected",
        "impact_actual",
        "impact_true_positives",
        "impact_precision",
        "impact_recall",
        "impact_out_of_scope",
        "impact_truncated",
        "callers_calibration",
        "impact_calibration",
    ]
    return {key: phase.get(key) for key in keys if key in phase}


def accuracy_summary(path):
    if not os.path.exists(path):
        return None
    with open(path, encoding="utf-8") as handle:
        data = json.load(handle)
    return {
        "corpus": data.get("corpus"),
        "language": data.get("language"),
        "commit": data.get("commit"),
        "documents": data.get("documents"),
        "references": data.get("references"),
        "definitions": data.get("definitions"),
        "attributable_references": data.get("attributable_references"),
        "exclusions": data.get("exclusions"),
        "ambiguous_name_keys": data.get("ambiguous_name_keys"),
        "candidate_targets": data.get("candidate_targets"),
        "sampled_targets": data.get("sampled_targets"),
        "min_confidence": data.get("min_confidence"),
        "before": phase_summary(data.get("before")),
        "after": phase_summary(data.get("after")),
        "calibration_04": phase_summary(data.get("calibration_04")),
        "adapter_touched_targets": data.get("adapter_touched_targets"),
        "adapter_touched_before": phase_summary(data.get("adapter_touched_before")),
        "adapter_touched_after": phase_summary(data.get("adapter_touched_after")),
        "disagreements": (data.get("before") or {}).get("disagreements", [])[:10],
    }


def load_runs(outdir, corpus):
    pattern = re.compile(rf"^{re.escape(corpus)}-(\d+)\.json$")
    paths = []
    for name in os.listdir(os.path.join(outdir, "runs")):
        match = pattern.match(name)
        if match:
            paths.append((int(match.group(1)), name))
    paths.sort()
    runs = []
    for _, name in paths:
        with open(os.path.join(outdir, "runs", name), encoding="utf-8") as handle:
            runs.append(json.load(handle))
    return runs


def aggregate(outdir, matrix, runs_want):
    corpora = {}
    for entry in matrix:
        name = entry["name"]
        runs = load_runs(outdir, name)
        if len(runs) != runs_want:
            die(f"{name}: found {len(runs)} runs, want {runs_want}")
        for run in runs:
            if run.get("schema") != SCHEMA:
                die(f"{name}: run schema {run.get('schema')} != {SCHEMA}")
            if run.get("commit") != entry["commit"]:
                die(f"{name}: run commit {run.get('commit')} != pinned {entry['commit']}")

        metrics = {}
        for metric, accessor in METRIC_ACCESSORS.items():
            values = [accessor(run) for run in runs]
            present = [v for v in values if isinstance(v, (int, float))]
            if len(present) == len(runs):
                metrics[metric] = median_range(present)
        samples = {}
        for metric, accessor in SAMPLE_ACCESSORS.items():
            values = [accessor(run) for run in runs]
            present = [v for v in values if isinstance(v, int)]
            if present:
                samples[metric] = median_range(present)

        first = runs[0]["structure"]
        structure = dict(first)
        variance = {}
        for key in STRUCTURE_KEYS:
            observed = [run["structure"].get(key) for run in runs]
            if len(set(observed)) > 1:
                variance[key] = observed
        structure["runs_consistent"] = not variance
        structure["variance"] = variance

        adapter = None
        if runs[0].get("adapter"):
            base = runs[0]["adapter"]
            adapter = {
                "name": base.get("name"),
                "definitions": median_range([r["adapter"]["definitions"] for r in runs]),
                "references": median_range([r["adapter"]["references"] for r in runs]),
                "matched": metrics.get("adapter_matched"),
                "replaced": metrics.get("adapter_replaced"),
                "replaced_edges": metrics.get("adapter_replaced_edges"),
                "edges": metrics.get("adapter_edges"),
                "conflicts": metrics.get("adapter_conflicts"),
                "skipped": median_range([r["adapter"]["skipped"] for r in runs]),
                "unmatched": median_range([r["adapter"]["unmatched"] for r in runs]),
                "wall_ms": metrics.get("adapter_wall_ms"),
                "unresolved_before": metrics.get("adapter_unresolved_before"),
                "unresolved_after": metrics.get("adapter_unresolved_after"),
                "edges_before": metrics.get("adapter_edges_before"),
                "edges_after": metrics.get("adapter_edges_after"),
                "new_edges_by_source": base.get("new_edges_by_source"),
                "new_edges_by_rule": base.get("new_edges_by_rule"),
            }

        corpora[name] = {
            "repo": entry["repo"],
            "commit": entry["commit"],
            "language": entry["language"],
            "root": entry["root"],
            "scip_path": entry["scip"],
            "runs": len(runs),
            "metrics": metrics,
            "samples": samples,
            "structure": structure,
            "adapter": adapter,
            "accuracy": accuracy_summary(os.path.join(outdir, f"accuracy-{name}.json")),
        }
    return corpora


def load_failure_modes(outdir):
    path = os.path.join(outdir, "failure-modes.json")
    if not os.path.exists(path):
        return None
    with open(path, encoding="utf-8") as handle:
        return json.load(handle)


def write_summary(outdir, summary):
    path = os.path.join(outdir, "summary.json")
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(summary, handle, indent=2, sort_keys=False)
        handle.write("\n")
    return path


def print_deltas(summary, baseline):
    if not baseline:
        print("bench-corpora: no baseline at bench/baseline.json; skipping deltas")
        return
    print("bench-corpora: deltas vs baseline (median, baseline -> now)")
    base_corpora = baseline.get("corpora", {})
    for name, corpus in summary["corpora"].items():
        base = base_corpora.get(name)
        if not base:
            print(f"  {name}: not in baseline")
            continue
        if base.get("commit") != corpus.get("commit"):
            print(
                f"  {name}: commit changed {base.get('commit')} -> {corpus.get('commit')} (deltas not comparable)"
            )
            continue
        for metric, current in corpus["metrics"].items():
            base_metric = (base.get("metrics") or {}).get(metric)
            if not base_metric:
                continue
            old = base_metric.get("median")
            new = current.get("median")
            if old in (None, 0) or new is None:
                continue
            delta = 100.0 * (new - old) / old
            print(f"  {name} {metric}: {old:g} -> {new:g} ({delta:+.1f}%)")
        for key in ("files", "nodes", "edges_total", "unresolved", "parse_failures"):
            old = (base.get("structure") or {}).get(key)
            new = (corpus.get("structure") or {}).get(key)
            if old is None or new is None:
                continue
            delta = new - old
            print(f"  {name} structure.{key}: {old} -> {new} ({delta:+d})")
        for phase in ("before", "after"):
            old = (base.get("accuracy") or {}).get(phase) or {}
            new = (corpus.get("accuracy") or {}).get(phase) or {}
            if old.get("precision") is None or new.get("precision") is None:
                continue
            print(
                f"  {name} accuracy.{phase}: precision {old['precision']*100:.1f}% -> {new['precision']*100:.1f}%, "
                f"recall {old['recall']*100:.1f}% -> {new['recall']*100:.1f}%"
            )


def portable_path(path):
    """Keep the checked-in baseline free of machine-specific absolute paths."""
    if not path:
        return path
    try:
        relative = os.path.relpath(path, os.getcwd())
    except ValueError:
        return path
    return path if relative.startswith("..") else relative


def median_of(value):
    if isinstance(value, dict):
        return value.get("median")
    return value


def write_baseline(summary, baseline_path):
    baseline = {
        "schema": SCHEMA,
        "generated_at": summary["generated_at"],
        "generated_by": "scripts/bench-corpora.sh",
        "repath_commit": summary.get("repath_commit"),
        "repath_version": summary.get("repath_version"),
        "host": summary["host"],
        "tools": summary["tools"],
        "runs_per_corpus": summary["runs_per_corpus"],
        "corpora": {},
    }
    for name, corpus in summary["corpora"].items():
        metrics = {}
        for metric, value in corpus["metrics"].items():
            metrics[metric] = {
                "median": value["median"],
                "min": value["min"],
                "max": value["max"],
            }
        structure = dict(corpus["structure"])
        structure.pop("runs_consistent", None)
        structure.pop("variance", None)
        # The bounded top-N lists are in summary.json; the baseline keeps only
        # the counters that make a regression visible.
        structure.pop("unresolved_top_names", None)
        structure.pop("unresolved_top_modules", None)
        adapter = None
        if corpus.get("adapter"):
            base_adapter = corpus["adapter"]
            adapter = {
                "name": base_adapter.get("name"),
                "definitions": median_of(base_adapter.get("definitions")),
                "references": median_of(base_adapter.get("references")),
                "matched": median_of(base_adapter.get("matched")),
                "replaced": median_of(base_adapter.get("replaced")),
                "replaced_edges": median_of(base_adapter.get("replaced_edges")),
                "edges": median_of(base_adapter.get("edges")),
                "conflicts": median_of(base_adapter.get("conflicts")),
                "skipped": median_of(base_adapter.get("skipped")),
                "unmatched": median_of(base_adapter.get("unmatched")),
                "wall_ms": median_of(base_adapter.get("wall_ms")),
                "unresolved_before": median_of(base_adapter.get("unresolved_before")),
                "unresolved_after": median_of(base_adapter.get("unresolved_after")),
                "edges_before": median_of(base_adapter.get("edges_before")),
                "edges_after": median_of(base_adapter.get("edges_after")),
                "new_edges_by_source": base_adapter.get("new_edges_by_source"),
                "new_edges_by_rule": base_adapter.get("new_edges_by_rule"),
            }
        baseline["corpora"][name] = {
            "repo": corpus["repo"],
            "commit": corpus["commit"],
            "language": corpus["language"],
            "scip_path": portable_path(corpus["scip_path"]),
            "runs": corpus["runs"],
            "metrics": metrics,
            "structure": structure,
            "adapter": adapter,
            "accuracy": corpus.get("accuracy"),
        }
    os.makedirs(os.path.dirname(os.path.abspath(baseline_path)), exist_ok=True)
    with open(baseline_path, "w", encoding="utf-8") as handle:
        json.dump(baseline, handle, indent=2, sort_keys=False)
        handle.write("\n")
    return baseline


def format_bytes(value):
    if value is None:
        return "—"
    value = float(value)
    for unit in ("B", "KiB", "MiB", "GiB"):
        if value < 1024 or unit == "GiB":
            return f"{value:.1f} {unit}" if unit != "B" else f"{int(value)} B"
        value /= 1024
    return f"{value:.1f} GiB"


def markdown_history(baseline, history_path):
    lines = []
    lines.append("# Benchmark history")
    lines.append("")
    lines.append(
        "Generated by `scripts/bench-corpora.sh --update-baseline`; do not edit by "
        "hand. The pinned corpus matrix, the JSON schema, and the accuracy caveats "
        "are in [`benchmarks.md`](benchmarks.md). This page is the small checked-in "
        "baseline view: medians with the observed range, so a later run can print "
        "deltas without carrying any database or artifact."
    )
    lines.append("")
    lines.append(f"Generated: `{baseline['generated_at']}` · repath `{baseline.get('repath_commit')}` (`{baseline.get('repath_version')}`) · {baseline['runs_per_corpus']} runs per corpus")
    host = baseline.get("host") or {}
    tools = baseline.get("tools") or {}
    lines.append("")
    lines.append("| Host | Value |")
    lines.append("| --- | --- |")
    for key, value in (
        ("hostname", host.get("hostname")),
        ("platform", host.get("platform")),
        ("kernel", host.get("kernel")),
        ("cpu", f"{host.get('cpu_model')} ({host.get('cpu_logical')} logical)"),
        ("RAM", format_bytes(host.get("ram_bytes"))),
        ("load average before run (1/5/15)", host.get("loadavg_before")),
        ("load average after run (1/5/15)", host.get("loadavg_after")),
        ("Go", tools.get("go")),
        ("Rust", tools.get("rustc")),
        ("Python", tools.get("python")),
        ("Node", tools.get("node")),
        ("git", tools.get("git")),
    ):
        lines.append(f"| {key} | {value if value is not None else '—'} |")
    for name, corpus in baseline["corpora"].items():
        lines.append("")
        lines.append(f"## `{name}` ({corpus['language']})")
        lines.append("")
        lines.append(f"- Repository: `{corpus['repo']}` @ `{corpus['commit']}`")
        if corpus.get("scip_path"):
            lines.append(f"- SCIP: `{corpus['scip_path']}`")
        structure = corpus.get("structure") or {}
        lines.append(
            "- Graph: **{files}** files, **{nodes}** nodes, **{edges}** edges, "
            "**{unresolved}** unresolved, **{parse}** parse failures, **{ambiguous}** ambiguous".format(
                files=structure.get("files"),
                nodes=structure.get("nodes"),
                edges=structure.get("edges_total"),
                unresolved=structure.get("unresolved"),
                parse=structure.get("parse_failures"),
                ambiguous=structure.get("ambiguous"),
            )
        )
        by_reason = structure.get("unresolved_by_reason") or {}
        if by_reason:
            lines.append(
                "- Unresolved by reason: "
                + ", ".join(f"`{key}` {value}" for key, value in by_reason.items())
            )
        lines.append("")
        lines.append("| Metric | Median | Range |")
        lines.append("| --- | --- | --- |")
        for metric, value in corpus["metrics"].items():
            if metric in ("check_findings",):
                continue
            lines.append(
                f"| `{metric}` | **{value['median']:g}** | {value['min']:g}–{value['max']:g} |"
            )
        adapter = corpus.get("adapter")
        if adapter:
            lines.append("")
            lines.append(
                "Adapter: **{matched}** matched · **{replaced}** replaced groups ({replaced_edges} rows) · "
                "**{edges}** new edges · **{conflicts}** conflicts · "
                "unresolved **{before} → {after}** · {wall:g} ms".format(
                    matched=adapter.get("matched"),
                    replaced=adapter.get("replaced") or 0,
                    replaced_edges=adapter.get("replaced_edges") or 0,
                    edges=adapter.get("edges"),
                    conflicts=adapter.get("conflicts"),
                    before=adapter.get("unresolved_before"),
                    after=adapter.get("unresolved_after"),
                    wall=adapter.get("wall_ms") or 0,
                )
            )
        accuracy = corpus.get("accuracy")
        if accuracy:
            lines.append("")
            lines.append(
                f"Accuracy on **{accuracy.get('sampled_targets')}** deterministically sampled symbols "
                f"({accuracy.get('candidate_targets')} candidates, {accuracy.get('references')} SCIP references):"
            )
            lines.append("")
            lines.append("| Phase | Callers P | Callers R | Impact≤2 P | Impact≤2 R |")
            lines.append("| --- | --- | --- | --- | --- |")
            for key, label in (("before", "tree-sitter"), ("after", "after adapter")):
                phase = accuracy.get(key) or {}
                lines.append(
                    "| {label} | {p:.1f}% | {r:.1f}% | {ip:.1f}% | {ir:.1f}% |".format(
                        label=label,
                        p=(phase.get("precision") or 0) * 100,
                        r=(phase.get("recall") or 0) * 100,
                        ip=(phase.get("impact_precision") or 0) * 100,
                        ir=(phase.get("impact_recall") or 0) * 100,
                    )
                )
            touched_before = accuracy.get("adapter_touched_before") or {}
            touched_after = accuracy.get("adapter_touched_after") or {}
            if touched_before and touched_after:
                lines.append("")
                lines.append(
                    "| Adapter-repaired target cohort | Callers P | Callers R |")
                lines.append("| --- | --- | --- |")
                for label, phase in (("tree-sitter", touched_before), ("after adapter", touched_after)):
                    lines.append(
                        "| {label} | {p:.1f}% | {r:.1f}% |".format(
                            label=label,
                            p=(phase.get("precision") or 0) * 100,
                            r=(phase.get("recall") or 0) * 100,
                        )
                    )
    lines.append("")
    lines.append("## Reading this")
    lines.append("")
    lines.append(
        "The summary.json written by the same run carries the raw per-run values, "
        "the host load average at generation time, and every metric this baseline "
        "condenses. `scripts/bench-corpora.sh` prints deltas against "
        "`bench/baseline.json` on the next run; only `--update-baseline` rewrites "
        "this page. Numbers are evidence, not a CI gate: the host is shared and "
        "small-tree timings are process-noise dominated."
    )
    lines.append("")
    os.makedirs(os.path.dirname(os.path.abspath(history_path)), exist_ok=True)
    with open(history_path, "w", encoding="utf-8") as handle:
        handle.write("\n".join(lines))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--outdir", required=True)
    parser.add_argument("--runs", type=int, required=True)
    parser.add_argument("--repath-commit", default=None)
    parser.add_argument("--repath-version", default=None)
    parser.add_argument("--baseline", default="bench/baseline.json")
    parser.add_argument("--history", default="docs/benchmarks-history.md")
    parser.add_argument("--update-baseline", action="store_true")
    args = parser.parse_args()

    matrix = read_matrix(os.path.join(args.outdir, "matrix.tsv"))
    corpora = aggregate(args.outdir, matrix, args.runs)
    summary = {
        "schema": SCHEMA,
        "generated_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "repath_commit": args.repath_commit,
        "repath_version": args.repath_version,
        "runs_per_corpus": args.runs,
        "host": host_info(),
        "tools": tools_info(),
        "corpora": corpora,
        "failure_modes": load_failure_modes(args.outdir),
    }
    summary_path = write_summary(args.outdir, summary)
    print(f"bench-corpora: summary {summary_path}")

    baseline = None
    if os.path.exists(args.baseline):
        with open(args.baseline, encoding="utf-8") as handle:
            baseline = json.load(handle)
    print_deltas(summary, baseline)

    if args.update_baseline:
        new_baseline = write_baseline(summary, args.baseline)
        markdown_history(new_baseline, args.history)
        print(f"bench-corpora: baseline {args.baseline}")
        print(f"bench-corpora: history {args.history}")


if __name__ == "__main__":
    main()
