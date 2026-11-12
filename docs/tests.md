# Test-impact mapping

`repath tests <target>` answers "which tests should I run?" from the symbol graph.
It is **structural reachability, not runtime coverage**: a test is reported when
the graph can show a path from that test to the target, not when the test
actually executes the changed code.

```sh
repath tests cart.total
repath tests src/cart.ts:42 --json
repath tests cart.total --min-confidence 0.9 --limit 20
repath tests --diff                 # changed files vs origin/main (fallback main, HEAD)
repath tests --diff origin/main --json
```

The command resolves the target like `impact` (`qname`, name, `file:line`, or a
file), walks the reverse closure at `--min-confidence` (default 0.5) and
`--depth` (default 8), and returns test files with one link per piece of
evidence:

| Rule | Confidence | Meaning |
| --- | --- | --- |
| the resolver rule (`ts.import.named`, `py.this.method`, …) | edge confidence | a test symbol reaches the target through graph edges |
| `testpath.convention` | 0.50 | a test file name matches the production file (`cart.ts` → `cart.test.ts`, `cart.spec.ts`, `test_cart.py`, `cart_test.py`, `__tests__/`, `tests/`) |
| `testpath.import` | 0.50 | a test file imports the affected file but has no graph edge to it |

Graph evidence wins; convention and import fallbacks only add a test file that
has no graph link. Every link names the affected symbol/file, the test symbol
when there is one, depth, confidence, rule, and call line.

## Diff mode

`repath tests --diff [base]` answers the CI question — "which tests for this
change set?" — by discovering changed files with git once and unioning the
existing per-file answer:

```sh
repath tests --diff                  # default base
repath tests --diff origin/main
repath tests --diff origin/main --json
```

- **Base resolution:** an explicit base wins; otherwise the first resolvable
  ref in `origin/main`, `main`, `HEAD` order is used. The report names the base
  it used. A bad ref, or a directory that is not a git work tree, exits `3`
  (never `5`).
- **Changed files** come from `git diff --name-status -z <base> --`, the single
  diff implementation in `internal/index`. Committed branch changes and
  uncommitted tracked changes are included. **Untracked files are not** (git
  diff cannot see them); `git add` a new file first. Renames report the new path
  as `renamed`; deletions report the removed path as `deleted`.
- **Changed test files are included directly** with rule `testpath.changed`,
  confidence `1.00`, depth `0` — a test that changed must run even if no
  production symbol reaches it.
- **Changed production files** go through the same `query.Tests` walker as
  `repath tests <file>`; each evidence link gains `changed_file`, so a test
  pulled in by two changed files shows two links.
- **Deleted and non-indexed files** (docs, binaries, generated files) are listed
  under `diff.skipped` instead of crashing or inventing a symbol.
- The index is refreshed first when the diff is non-empty, so the answer
  describes the current tree. A clean diff returns an explicit empty answer and
  exits `0` **without opening or building an index**.
- The union is bounded by `--limit` (test files) and the normal depth/scan caps;
  `truncated` is set when any per-file answer or the union was capped.

The structural ceiling is unchanged: this is reachability, not coverage, and it
is not CI-provider integration.

## JSON

```json
{
  "targets": [{"id": 2, "qname": "total", "kind": "function", "file": "src/cart.ts", "line": 1}],
  "tests": [
    {
      "file": "src/cart.test.ts",
      "links": [{
        "test_symbol": "cartTest",
        "test_file": "src/cart.test.ts",
        "affected_symbol": "total",
        "affected_file": "src/cart.ts",
        "depth": 1,
        "confidence": 1,
        "rule": "ts.import.named",
        "call_line": 4,
        "changed_file": "src/cart.ts"
      }]
    }
  ],
  "depth": 8,
  "min_confidence": 0.5,
  "truncated": false,
  "diff": {
    "base": "origin/main",
    "changed": [
      {"path": "src/cart.ts", "status": "modified"},
      {"path": "src/old.ts", "status": "deleted"}
    ],
    "skipped": [{"path": "src/old.ts", "status": "deleted"}]
  }
}
```

`diff` is present only in `--diff` mode; `changed_file` is present on each diff
link and omitted in the single-target mode. `truncated` is set when the reverse
scan (bounded at 500 dependents) or the test-file limit was reached. A target with no reachable tests returns an empty
`tests` array and exits 0; an unknown/ambiguous target follows the normal exit
codes (`3` / `4`).

## Limits

- **Not coverage.** Dynamic dispatch, fixtures, e2e suites, snapshot tests, and
  tests that exercise the code through an unmodelled framework are invisible.
- Path conventions are a heuristic and can produce a test file that does not
  exercise the target; the rule id and 0.50 confidence make that visible.
- Import fallback resolves relative TS/JS imports and simple Python dotted
  imports. Package aliases and wildcard re-exports are not followed here.
- Diff mode is git-only and untracked files are invisible until `git add`ed.
- A deleted production file cannot be symbol-resolved; it is reported under
  `diff.skipped`. A deleted test file is not run.
- The reverse scan is capped at 500 dependents; raise `--depth` only when the
  graph is known to be shallow.

## Shared classifier

`internal/testpath.IsTest` is the single test-path classifier. `repath check`
uses it to exclude test files from dead-code/boundary findings and
`repath tests` uses it to recognise test files, so the two commands cannot
drift. `internal/testpath/testpath_test.go` owns the path cases.

## Verification

`testdata/fixtures/tests/` has a direct test (`cart.test.ts`), a transitive test
(`checkout.test.ts`), an import-only test, and a convention-only `cart.spec.ts`.
`internal/cli/tests_test.go` asserts all four, the empty answer, truncation, and
the shared rule ids; `repath check`'s tests continue to use the same classifier.
Diff mode is covered by `internal/index/diff_test.go` (modified/added/deleted/
renamed discovery, sorted output, base fallback, non-repo and bad-ref errors)
and by `internal/cli/tests_test.go` (a changed production file, a changed test
file, an added file, a deleted file, an unrelated test, a clean diff, and the
`2`/`3` error paths).
