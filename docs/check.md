# `repath check`

`repath check` is the graph-native diagnostic command from PLAN §16.2. It reads
the symbol graph and reports five kinds of structural finding; it never invokes
a compiler and needs no config file. An opt-in `--sarif FILE` can attach
compiler/linter diagnostics that another tool already produced (PLAN §3
decision 22, amended in v1.2) — repath still never runs the compiler.

```sh
repath check                          # report; fails on error-level findings
repath check --fail-on warning        # stricter CI gate
repath check --fail-on none           # report only, always exit 0
repath check --json                   # machine-readable report
repath check --sarif tsc.sarif        # attach SARIF diagnostics to symbols
repath check --sarif out/sarif/       # every *.sarif under a directory
repath check --sarif tsc.sarif --sarif ruff.sarif   # repeatable
```

`check` refreshes the index first (like `status`), so its findings describe the
current tree. `--no-refresh` is available for a deliberately stale snapshot.

## Findings

| Kind | Rule | Severity | Meaning |
| --- | --- | --- | --- |
| `module-cycle` | `module.cycle` | warning | two or more modules form an SCC in the cross-module edge graph |
| `dead-code` | `dead.unreferenced` | warning | a private function/method with no incoming edge at the confidence floor |
| `module-boundary` | `boundary.test` | **error** | non-test code depends on a symbol defined in a test path |
| `module-boundary` | `boundary.private` | warning | a module references a private symbol from another module |
| `hotspot` | `hotspot.fan-in` / `hotspot.fan-out` | info | a non-test symbol has fan-in or fan-out ≥ 5 (top 5 each) |
| `unresolved-rate` | `unresolved.module-rate` | warning | a module has ≥ 3 unresolved refs and ≥ 50% unresolved |
| `diagnostic` | `sarif.<tool>.<ruleId>` | error / warning / info | an opt-in SARIF result attached to the symbol enclosing its line |

Cycles are warnings on purpose: module cycles are computed on cross-module edges
at the confidence floor, and one noisy edge can merge modules into a single SCC
(PLAN §14). Use `--fail-on warning` when a cycle should block CI.

## Dead code is deliberately conservative

Only **private** `function`/`method` symbols are candidates:

- TypeScript/JavaScript: not `export`ed; methods inherit the class's export flag.
- Rust: not `pub`.
- Python: the `_name` convention (module-level defs are all importable, so
  `Exported` alone cannot decide).

The check skips:

- exported/public API (it may be consumed outside the repository);
- test paths (`test/`, `tests/`, `__tests__/`, `testdata/` anywhere, or at the
  repo root; `*.test.ts|tsx|js|jsx`, `*.spec.ts|tsx|js|jsx`, `*.test.py`,
  `*.spec.py`, `test_*.py`, `conftest.py`);
- files with `parse_ok=0` (partial facts);
- framework/entrypoint names (`main`, `constructor`, `setUp`, `tearDown`,
  lowercase `setup`/`teardown`, `__init__`, and any other `__x__` dunder).

Top-level references have no source symbol, so a private function referenced
only from module top level can still be reported. That is the documented v1
graph limitation, not a hidden guess.

## Boundary checks need no config

Without a config file, repath cannot know an intended layering. The two checks
here are objective:

- **Test boundary:** production code depends on a test-file symbol. This is an
  error because shipping it couples the product to test scaffolding.
- **Private crossing:** a symbol in module A references a private symbol in
  module B. Python privacy is the underscore convention; TypeScript and Rust use
  their export markers. A convention can be intentional, so this is a warning.

## Hotspots and unresolved rate

Hotspots are informational: the top five symbols by fan-in and fan-out among
non-test code, with a minimum count of 5. They answer "where is the gravity",
not "this is wrong".

The unresolved rate is per module:
`pending / (resolved outgoing edges at the confidence floor + pending)`. It is
an approximation (external imports are not pending, and ambiguous edges below
the threshold are excluded), so it is reported only for modules with at least 3
pending references and a rate of at least 50%.

## SARIF diagnostics (opt-in)

`--sarif` is a **bridge, not a compiler frontend**. repath never shells out to
`tsc`, `pyright`, `ruff`, or `rustc`; it only reads the SARIF 2.1.0 log those
tools already emit:

```sh
# The producer is external; its version and flags are the producer's concern.
ruff check --output-format=sarif app/ > ruff.sarif
# tsc has no built-in SARIF output today; point --sarif at whatever wrapper or
# linter writes a 2.1.0 log.

repath check --sarif tsc.sarif --sarif ruff.sarif --fail-on warning
```

Each result is attached to the smallest symbol whose `start_line..end_line`
contains the result's first location, using the same `EnclosingNode` path as
`impact file:line`. The finding keeps its location and gains the symbol in
`qname`:

- Rule ids are `sarif.<tool>.<ruleId>`, where `<tool>` is
  `run.tool.driver.name` and `<ruleId>` prefers `result.ruleId` and falls back
  to `result.rule.index` into `driver.rules`. Whitespace and `/` collapse to
  `-`, and a missing rule becomes `unknown`.
- `level` maps `error → error`, `warning → warning`, `note`/`none → info`; a
  missing level is `warning`, the SARIF default.
- Duplicates are collapsed on `(rule, file, line, message)` and counted.
- A result whose file is not indexed, whose URI cannot be matched, or whose
  line is outside every symbol is **skipped and counted**, never silently
  dropped. The summary keeps `results = attached + skipped + duplicates`.
- URIs are repo-relative POSIX paths; `file://` URIs and absolute paths under
  `--root` are made relative. A unique subproject mapping is not guessed.
- Only SARIF **2.1.0** is accepted. Any other version is refused with a
  one-line message and exit `2`; a missing file or a directory with no
  `*.sarif`/`*.sarif.json` is exit `3`.

When `--sarif` is absent, the report and text output are byte-for-byte the old
structural report. Diagnostics count toward `--fail-on` like any other finding;
`check` is still the only command that returns `5`.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | no findings at or above `--fail-on` |
| `5` | findings at or above `--fail-on` (check only) |
| `1` | internal error |
| `2` | usage error, malformed/unsupported SARIF |
| `3` | missing SARIF input, or a directory with no SARIF files |

`--fail-on` defaults to `error`, so a fresh check fails only on a test-boundary
violation. Use `--fail-on warning` to gate on cycles, dead code, private
crossings, and unresolved-rate findings; `--fail-on info` includes hotspots.
`--fail-on none` always exits 0.

The other commands never return `5`; exit codes `0–4` are unchanged.

## JSON

```json
{
  "root": "/repo",
  "schema_version": 5,
  "min_confidence": 0.5,
  "max_findings": 200,
  "files": 17,
  "nodes": 18,
  "edges": 13,
  "counts": {"error": 1, "warning": 0, "info": 0},
  "findings": [
    {
      "kind": "module-boundary",
      "severity": "error",
      "rule": "boundary.test",
      "message": "module \"src\" depends on test symbol \"ping\"",
      "qname": "ping",
      "file": "src/app.ts",
      "line": 2,
      "module": "src",
      "modules": ["src", "test"],
      "count": 1
    }
  ],
  "truncated": false
}
```

`counts` counts every finding, including any omitted by `--max-findings`; the
list is severity-first, so truncation removes the least severe findings first.

With `--sarif`, the report gains one summary object and the diagnostic findings
carry their attached symbol:

```json
{
  "diagnostics": {"sources": 1, "results": 5, "attached": 3, "skipped": 1, "duplicates": 1},
  "findings": [
    {
      "kind": "diagnostic",
      "severity": "error",
      "rule": "sarif.tsc.TS2322",
      "message": "Type 'string' is not assignable to type 'number'.",
      "qname": "Cart.total",
      "file": "src/cart.ts",
      "line": 5,
      "count": 1
    }
  ]
}
```

The summary is omitted entirely when `--sarif` is absent, so existing JSON
consumers see no change.

## CI

```sh
repath check --fail-on warning --json > check.json
```

Exit `5` is the gate; the JSON report is the annotation payload.

## Verification

`internal/check/check_test.go` covers a two-module cycle, a dead private
function vs a used helper, production→test and cross-module private boundaries,
a Rust private function and private method crossing a module boundary, the
TypeScript rule that a non-exported class's methods inherit the class export
flag, Python dunder/entrypoint name exclusions, a 10-caller fan-in hotspot, a
module with 5 unresolved refs, truncation counts, and the full test-path
classifier (including `testdata/`, `__tests__/`, `*.test.*`, `*.spec.*`,
`test_*.py`, and `conftest.py`). The classifier is
`internal/testpath.IsTest`, shared with `repath tests`. `internal/cli/check_test.go`
covers the text report, JSON, `--fail-on` thresholds, and usage errors.

On the reproducible 50k tree from `make perf-50k`, `check` takes **464 ms**
(0 findings); the 10k tree takes **86 ms**. The 50k tree is synthetic and has
no violations, so this is runtime evidence, not a false-positive benchmark.

The SARIF path is pinned by two committed logs (not generated at test time):
`testdata/fixtures/sarif/ts/tsc.sarif` (5 results → 3 attached, 1 skipped,
1 duplicate) and `testdata/fixtures/sarif/py/ruff.sarif` (3 → 2 attached,
1 skipped). `internal/check/sarif_test.go` covers the 2.1.0 gate, rule-index
fallback, level mapping, path normalization, dedupe, skipped-result counts, and
both attachment fixtures; `internal/cli/check_test.go` covers repeatable
`--sarif`, directory input, text/JSON, `--fail-on`, and the clean `3`/`2` error
paths. No real `tsc`/`ruff` run is part of CI — the fixtures are the contract.
