# Read-only HTTP answers (`repath serve`)

`repath serve` exposes the same six structural answers over HTTP for
dashboards, shell-less clients, and remote editors that cannot speak MCP over
stdio. It is the PLAN §16.8 surface: read-only, one existing index, no new
answer layer.

```sh
repath index                                   # build once (or run status)
repath serve --addr 127.0.0.1:0
# repath serve listening on http://127.0.0.1:41234 (read-only)

curl 'http://127.0.0.1:41234/status'
curl 'http://127.0.0.1:41234/impact?symbol=total'
```

`--addr` defaults to `127.0.0.1:0` (loopback, ephemeral port). The bound address
is printed to stdout as one line; logs and warnings go to stderr. `--root`
selects the repository whose `.repath/index.db` is served.

## Endpoints

All endpoints are `GET` and return the MCP/CLI envelope (§7), not a new format.

| Endpoint | Query parameters | Defaults | Answer |
| --- | --- | --- | --- |
| `/find` | `name` (required), `module?` | — | symbol card, location, direct caller/callee counts |
| `/impact` | `symbol` (required), `depth?`, `min_confidence?` | 8, 0.5 | dependents by depth, affected files/modules |
| `/path` | `from`, `to` (required), `max_depth?`, `min_confidence?` | 8, 0.5 | chain(s) with per-edge provenance, or the nearest frontier |
| `/callers` | `symbol` (required), `depth?` | 1 | direct reverse edges; depth > 1 is a `400` pointing at `/impact` |
| `/map` | `max_tokens?` | 2000 | modules, module cycles, PageRank hubs |
| `/status` | — | — | counts, unresolved/ambiguity rates, parse failures, staleness, dirty files |

`symbol` and `from`/`to` accept the same forms as the CLI: `qname`, `name`,
`file:line`, or `file:name`. `module` narrows `/find` exactly like the CLI and
MCP tool.

## Envelope

```json
{
  "answer": {
    "targets": [{"id": 2, "qname": "total", "kind": "function", "file": "src/cart.ts", "line": 1}],
    "dependents": [
      {"qname": "checkout", "depth": 1, "confidence": 1, "rule": "ts.import.named", "call_line": 4}
    ],
    "min_confidence": 0.5
  },
  "summary": "1 dependents across 1 files and 1 modules (deepest depth 8, min confidence 0.50)",
  "confidence": 1,
  "truncated": false,
  "provenance": [{"file": "src/checkout.ts", "line": 4, "rule": "ts.import.named"}]
}
```

`confidence` is the weakest edge the answer stands on, `truncated` mirrors the
depth/limit/token budget, and `provenance` is capped at 50 deduplicated
`(file, line, rule)` entries. These values come from `internal/output` and
`internal/query.ImpactEnvelope` and friends — the same functions the MCP
adapter uses, so the two transports cannot drift.

## HTTP status mapping

| Status | Envelope `answer.code` | Meaning |
| --- | --- | --- |
| `200` | — | answer |
| `400` | `2` | missing/invalid query parameter, or unsupported `callers` depth |
| `404` | `3` | unknown symbol/file, or unknown path |
| `409` | `4` | ambiguous symbol; `answer.candidates[]` lists the matches |
| `405` | `2` | anything other than `GET` |
| `500` | `1` | internal error |

An error still returns the envelope, so a client reads
`{"answer":{"error":"not found: nope","code":3}, …}` rather than a bare HTTP
body. `5` is never produced: only `repath check` owns it.

## Read-only guarantees

- The index is opened with SQLite `mode=ro` **and** `query_only`, through
  `store.OpenReadOnly`; the server cannot insert, update, migrate, or create a
  database.
- `serve` refuses to start without an existing index (exit `3`, with
  `run repath index or status first`) and refuses an unknown schema version. It
  never builds an index.
- `/status` calls the status query with `refresh=false`. A stale or dirty tree
  is reported in the answer (`stale`, `dirty_files`) instead of being patched.
  Run `repath status` or `repath index` separately to refresh.
- There are no artifact routes, no mutation routes, no config file, and no
  second SQLite writer. One read-only connection is shared by the handlers.
- Shutdown on `SIGINT`/`SIGTERM` is graceful (5 s), then the listener closes.

## Security

The default is loopback. `--addr` accepts any `host:port` because a dashboard on
a trusted host may need it, but binding a wider address prints a warning on
stderr and is **unauthenticated and unencrypted**. Do not expose it to an
untrusted network; use a reverse proxy with auth/TLS if that is required. Do
not point it at a database on a network filesystem (SQLite locking does not
survive that).

## Verification

`internal/serve/serve_test.go` covers all six endpoints and their envelope,
`/status` leaving the database mtime/size unchanged, `404`/`409`/`400`/`405`
mapping, 20 concurrent `/impact` reads, a missing index, and `Run` serving then
shutting down on context cancellation. `internal/store/store_test.go` covers
read-only open and rejects a write through `query_only`.
`internal/cli/serve_test.go` covers bad `--addr`/extra arguments and the
no-index exit `3` without creating `.repath/`.

## Limits

- Six answers only: `flows` and `tests` have no HTTP endpoint, just as they have
  no MCP tool. Adding one would be a new answer surface, not a transport.
- No streaming, auth, TLS, CORS configuration, or multi-repo serving.
- Structural answers, not coverage or a runtime trace.
- The server does not watch the index; restart it (or use the CLI/MCP) after a
  refresh if you need a new snapshot.
