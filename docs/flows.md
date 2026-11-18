# Entrypoints and flows

`repath flows <entrypoint>` answers "where does execution enter, and where does
it go next?" It walks **forward** from a symbol that was detected as an
entrypoint and returns bounded chains with per-edge rule and confidence. It is
structural: no runtime trace, dynamic dispatch, or HTTP routing is simulated.

Entrypoints are a **dimension over symbols**, not a new node kind. The
`entrypoints` table (schema v6) stores `(node_id, rule, name, confidence, line)`;
the node itself remains the same symbol, so identity and every existing query
are unchanged. One symbol may have several rules (for example a `main` that is
also a `#[test]`).

## Detection

| Language | Rule | Heuristic |
| --- | --- | --- |
| TypeScript/JS | `ts.route.express` | a named handler passed to `app.get/post/put/patch/delete/all/route` or the same on `router` |
| TypeScript/JS | `ts.route.fastify` | the same registration on `fastify` or `server` |
| TypeScript/JS | `ts.next.route` | an exported `GET`/`POST`/`PUT`/`PATCH`/`DELETE`/`HEAD`/`OPTIONS` (or `handler`) under `app/**/route.ts` or `pages/api/` |
| Python | `py.route.fastapi` | `@app.get/post/put/patch/delete/websocket` or `@router.…` on a function |
| Python | `py.route.flask` | `@app.route(...)` / `@bp.route(...)` / `@blueprint.route(...)` |
| Python | `py.test` | a function whose name starts with `test_` |
| Python | `py.main` | a module-level `def main()` in a file with an `if __name__ == "__main__"` guard |
| Rust | `rs.main` | a function named `main` |
| Rust | `rs.test` | a function with a `#[test]`-style attribute (including `#[tokio::test]`) |

Inline route handlers (`app.get("/x", async (req, res) => …)`) have no symbol to
anchor, so they are skipped rather than inventing a node. A handler passed by
name is anchored to that name; if it is local or imported, the index matches it
by qname and falls back to the file's unique name.

## The answer

```sh
repath flows start --json
```

```json
{
  "entrypoint": {"id": 2, "qname": "start", "kind": "function", "file": "src/server.ts", "line": 5},
  "entrypoint_rule": "ts.route.express",
  "paths": [[
    {"qname": "step", "file": "src/server.ts", "line": 9, "depth": 1, "edge_kind": "CALLS",
     "confidence": 0.85, "rule": "ts.local", "call_line": 6},
    {"qname": "finish", "file": "src/server.ts", "line": 13, "depth": 2, "edge_kind": "CALLS",
     "confidence": 0.85, "rule": "ts.local", "call_line": 10}
  ]],
  "frontier": [{"qname": "finish", "kind": "function", "file": "src/server.ts", "line": 13}],
  "depth": 6,
  "min_confidence": 0.5,
  "truncated": false
}
```

- `paths` are leaf chains. A branching handler gets one path per leaf, capped by
  `--limit` (default 20).
- `frontier` is the set of leaves that did not continue at the confidence floor
  or depth budget. When there are no outgoing edges, it contains the entrypoint
  itself and `paths` is empty.
- `--depth` defaults to 6, `--min-confidence` to 0.5. `truncated` is set when a
  depth or path cap was reached.
- Exit codes follow the normal contract: `0` ok, `2` usage, `3` not an
  entrypoint/not found, `4` several entrypoints share the name.

`status --json` and `map --json` expose `entrypoints` and `entrypoint_rules`
counts. The six MCP tools are unchanged; there is no `flows` tool.

## Limits and false positives

- Detection is syntactic. A `app.get(...)` on an object that is not an HTTP app
  can still be marked; a custom router wrapper is invisible.
- Flask and FastAPI are distinguished by decorator shape (`route` vs an HTTP
  verb), not by imported package. A project can use either style on either.
- Next.js detection is path-based (`route.ts` or `pages/api/`). A custom server
  or a renamed route file is not detected.
- Python `__main__` is only anchored when a module-level `main()` exists; a
  script that runs top-level statements has no containing symbol to start from.
- Rust macro-generated entrypoints (`#[tokio::main]` is still a function named
  `main`; a proc-macro-generated handler is not) are outside the static model.
- Tests are entrypoints for flow reading, not a test-coverage claim.
- The flow walker follows `CALLS`/`REFERENCES`/inheritance edges at the
  confidence floor. It does not model runtime dispatch, callbacks that are not
  graph edges, or network boundaries.

## Verification

`internal/index/entrypoints_test.go` covers detection for the three fixture
families and removal on re-index. `internal/cli/flows_test.go` covers the JSON
path (`start → step → finish`), text output, status/map counts, and the
not-an-entrypoint exit code. Fixtures live under `testdata/fixtures/flows/`.
