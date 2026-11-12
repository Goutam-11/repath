# Test-impact mapping

`repath tests <target>` answers "which tests should I run?" from the symbol graph.
It is **structural reachability, not runtime coverage**: a test is reported when
the graph can show a path from that test to the target, not when the test
actually executes the changed code.

```sh
repath tests cart.total
repath tests src/cart.ts:42 --json
repath tests cart.total --min-confidence 0.9 --limit 20
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
        "call_line": 4
      }]
    }
  ],
  "depth": 8,
  "min_confidence": 0.5,
  "truncated": false
}
```

`truncated` is set when the reverse scan (bounded at 500 dependents) or the
test-file limit was reached. A target with no reachable tests returns an empty
`tests` array and exits 0; an unknown/ambiguous target follows the normal exit
codes (`3` / `4`).

## Limits

- **Not coverage.** Dynamic dispatch, fixtures, e2e suites, snapshot tests, and
  tests that exercise the code through an unmodelled framework are invisible.
- Path conventions are a heuristic and can produce a test file that does not
  exercise the target; the rule id and 0.50 confidence make that visible.
- Import fallback resolves relative TS/JS imports and simple Python dotted
  imports. Package aliases and wildcard re-exports are not followed here.
- There is no `--diff` mode yet. It would reuse the existing git discovery
  before inventing a second diff engine.
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
