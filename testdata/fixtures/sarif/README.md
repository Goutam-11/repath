# SARIF fixtures

These files are pinned, realistic SARIF 2.1.0 outputs used by
`internal/check/sarif_test.go` and `internal/cli/check_test.go`. They are not
generated at test time: `tsc`/`ruff` are external producers, exactly like
`scip-typescript` in `testdata/fixtures/ts-adapter`.

| Fixture | Producer | Results | Attached | Skipped | Duplicates |
| --- | --- | --- | --- | --- | --- |
| `ts/tsc.sarif` | `tsc` (simulated 2.1.0 log) | 5 | 3 | 1 | 1 |
| `py/ruff.sarif` | `ruff` (simulated 2.1.0 log) | 3 | 2 | 1 | 0 |

The skipped result is a top-level import line, which has no enclosing symbol;
repath counts it instead of dropping it silently. `ts/tsc.sarif` also repeats
one result verbatim so the `(rule, file, line, message)` dedupe is pinned.
