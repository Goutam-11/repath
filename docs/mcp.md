# MCP adapter

`repath mcp` is a stdio JSON-RPC server in the same binary. It exists for MCP
clients without a shell (desktop panes, IDE sidebars, remote setups). An agent
with bash should use the CLI instead: the CLI returns the same answers, carries
no always-on schema cost, and can be composed with normal shell tools
(PLAN.md §7.1).

The adapter is hand-rolled: no MCP SDK, no generated schemas. The transport is
newline-delimited JSON-RPC 2.0 over stdin/stdout; stdout is protocol only, logs
go to stderr.

## Lifecycle

```
client → initialize                       → protocolVersion 2024-11-05, tools capability
client → notifications/initialized        (no response)
client → tools/list                       → exactly six tools with JSON Schemas
client → tools/call {name, arguments}     → {content:[{type:"text",text:"<envelope>"}]}
client → ping                             → {}
```

Unknown methods return JSON-RPC `-32601`; malformed JSON returns `-32700`;
invalid tool-call params return `-32602`. Tool failures (not found, ambiguous,
bad argument, internal) are **tool results** with `isError: true`, so the client
sees the answer envelope instead of losing it to a protocol error.

The index is opened lazily on the first tool call and kept open for the process
lifetime. The server never indexes Go files (not a v1 language), so on repath
itself `status` reports zero files.

## The six tools

| Tool | Arguments | Answer |
| --- | --- | --- |
| `find_symbol` | `name`, `module?` | symbol card, location, direct caller/callee counts |
| `impact` | `symbol`, `depth?` (8), `min_confidence?` (0.5) | dependents by depth, affected files/modules |
| `path` | `from`, `to`, `max_depth?` (8), `min_confidence?` (0.5) | chain(s) with per-edge provenance, or the nearest frontier |
| `callers` | `symbol`, `depth?` (1) | direct reverse edges; depth > 1 is rejected with a pointer to `impact` |
| `repo_map` | `max_tokens?` (2000) | modules, module cycles, PageRank hubs |
| `status` | — | counts, unresolved rate, ambiguity rate, parse failures, staleness, dirty files |

The defaults live in `internal/query` and are shared with the CLI, so the two
surfaces cannot drift.

## Envelope

Every successful tool result is one JSON object in the MCP text content block:

```json
{
  "answer": { },
  "summary": "2 dependents across 2 files and 1 modules (deepest depth 2, min confidence 0.50)",
  "confidence": 1,
  "truncated": false,
  "provenance": [{"file": "b.ts", "line": 2, "rule": "ts.import.named"}]
}
```

- `confidence` is the weakest edge the answer stands on (so a path is only as
  confident as its weakest hop). Answers with no edges report `1.0`, except a
  path with no route, which reports `0`.
- `truncated` mirrors the depth/limit/token budget: `impact` caps dependents,
  `callers` caps at 200, `repo_map` trims symbols to `max_tokens`.
- `provenance` lists at most 50 `(file, line, rule)` entries, deduplicated. The
  rule id is the resolver rule (`ts.import.named`, `py.this.method`,
  `rs.self.method`, …), so a client can audit why an edge exists.

`status` includes `unresolved_rate` (unresolved / (resolved edges + unresolved))
and `stale` (indexed commit differs from HEAD) for freshness decisions.

## Client configuration

A client that launches servers from a config file needs one entry. The exact
format is client-specific; the command and arguments are:

```json
{
  "command": "/absolute/path/to/repath",
  "args": ["mcp", "--root", "/absolute/path/to/repo"]
}
```

`--root` defaults to the current working directory (or the git work-tree root).

## Verification

The pipe round-trip test (`internal/mcp/server_test.go`) drives initialize,
tools/list, every tool, and the error paths through real `io.Pipe`s. One
end-to-end call was also verified with the official TypeScript MCP client SDK
`@modelcontextprotocol/client` 2.2.0 (legacy handshake): `listTools` returned the
six tools and `impact` returned the expected two dependents for the fixture.
