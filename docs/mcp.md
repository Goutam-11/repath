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

The adapter is **dual-era**:

```
modern client (2026-07-28)
  client → server/discover {_meta: {protocolVersion, clientInfo, …}}
           ← supportedVersions ["2026-07-28"], capabilities, serverInfo
  client → tools/list      {…_meta}   ← resultType "complete", ttlMs, cacheScope
  client → tools/call      {…_meta}   ← resultType "complete", content
  client → ping             {…_meta}   ← resultType "complete"

legacy client (2024-11-05 and earlier)
  client → initialize                   ← protocolVersion 2024-11-05, tools capability
  client → notifications/initialized    (no response)
  client → tools/list                   ← exactly six tools
  client → tools/call {name, arguments} ← {content:[{type:"text",text:"<envelope>"}]}
```

Modern requests carry
`params._meta["io.modelcontextprotocol/protocolVersion"]`. An unsupported
version returns JSON-RPC `-32022` with `data.supported` and `data.requested`, as
the specification requires. `server/discover` returns `resultType:"complete"`,
`supportedVersions:["2026-07-28"]`, `capabilities.tools`, `_meta.serverInfo`,
`ttlMs`, and `cacheScope`. A request without `_meta` is treated as legacy and
keeps the `initialize` path byte-for-byte.

Unknown methods return JSON-RPC `-32601`; malformed JSON returns `-32700`;
invalid tool-call params return `-32602`. Tool failures (not found, ambiguous,
bad argument, internal) are **tool results** with `isError: true`, so the client
sees the answer envelope instead of losing it to a protocol error. Modern tool
results also carry `resultType:"complete"`.

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

`status` includes `unresolved_rate` (unresolved / (resolved edges + unresolved)),
`stale` (indexed commit differs from HEAD), `entrypoints`/`entrypoint_rules`
(the flow dimension), and `adapters[]` (compiler-adapter coverage, skips, and
conflicts; see `docs/adapters.md` and `docs/flows.md`).

## Client configuration

A client that launches servers from a config file needs one entry. The exact
format is client-specific; the command and arguments are the same for both
eras:

```json
{
  "command": "/absolute/path/to/repath",
  "args": ["mcp", "--root", "/absolute/path/to/repo"]
}
```

`--root` defaults to the current working directory (or the git work-tree root).

## Verification

The pipe round-trip test (`internal/mcp/server_test.go`) drives initialize,
tools/list, every tool, and the error paths through real `io.Pipe`s.
`TestModernProtocolNegotiation` covers `server/discover`, modern tools/list and
tools/call, modern tool errors, the unsupported-version error, legacy
initialize, and legacy tools/list.

SDK probes (protocol eras, repath **a160280**):

| Probe | Exact version | Transport | Era observed | `tools/list` | `impact` |
| --- | --- | --- | --- | --- | --- |
| modern | `@modelcontextprotocol/client` **2.4.0**, `mode: {pin: "2026-07-28"}` | stdio | modern (`getProtocolEra() === "modern"`) | 6 | expected dependents |
| legacy | `@modelcontextprotocol/sdk` **1.32.2** | stdio | legacy `initialize` (`2024-11-05`) | 6 | expected dependents |
| legacy (M5) | `@modelcontextprotocol/client` 2.2.0 | stdio | legacy | 6 | worked |

Real clients (this machine, same binary):

| Client | Exact version | Transport | Era | `tools/list` | `impact` |
| --- | --- | --- | --- | --- | --- |
| MCP Inspector | `@modelcontextprotocol/inspector` **2.15.0** (`--cli`) | stdio | **legacy**: client offered `2025-11-25`, repath answered `2024-11-05` | 6 tools | 3 dependents: `checkout`, `cartTest`, `checkoutTest` |
| Codex CLI | `codex-cli` **0.164.0** (`codex exec --json`) | stdio | **legacy**: client offered `2025-06-18`, repath answered `2024-11-05` | 6 tools | same 3 dependents |
| Claude Desktop / IDE panel | — | — | — | **not verified** | not available on this Linux sandbox |
| Inspector web/TUI | 2.15.0 | stdio | legacy | **not driven** (headless run used the official `--cli` client; the UI was not automated) | — |

No **GUI** client could be exercised on this machine: Claude Desktop is not
installed, and the Inspector web/TUI was not automated. Both real clients that
were available are CLI/TUI tools. Neither Inspector 2.15.0 nor Codex 0.164.0
sends the `2026-07-28` `_meta`/`server/discover` path, so **modern era with a
real or GUI client remains unverified**; the modern evidence is the
`@modelcontextprotocol/client` 2.4.0 probe above. The six-tool list and the
envelope were unchanged in every probe, and no MCP SDK dependency was added to
the repository.
