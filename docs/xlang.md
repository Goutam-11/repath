# Cross-language links

repath's graph is per language: a TS `fetch("/api/users")` and a Python
`@app.get("/api/users")` are separate islands. The cross-language pass adds one
kind of link between them, so a mixed-language `impact`/`path` can answer "what
breaks" without a runtime. It is deliberately low-confidence and derived, never
a replacement for the static graph.

## Edge shape

- Kind: **`REFERENCES`** — no new edge kind.
- Rule: **`xlang.http.route`**; confidence **0.45**.
- 0.45 is below the default `--min-confidence 0.5`, so default `impact`,
  `path`, `map`, `callers`, and `check` answers are unchanged. Lower the dial to
  opt in:

```sh
repath impact <handler> --min-confidence 0.4
repath path <client> <handler> --min-confidence 0.4
```

- Direction: client symbol → server handler. Reverse impact from the handler
  reaches the client; `path` from the client to the handler is one hop.
- Only symbols in **different languages** are linked: a TS client and a TS
  Express route in the same repo produce no cross-language edge.

## Inputs: `xlang_anchors`

The pass never parses at query time. Each file contributes literal anchor facts
into the `xlang_anchors` dimension (schema v6), replaced per file like
`entrypoints` (not nodes, not edges):

- `protocol` (`http`), `role` (`client` | `server`), `method`, `path`
- the enclosing symbol (`node_id`), line, and language
- `confidence` 0.90 for a literal fact

Server anchors: FastAPI `@app.get` / `@router.post`, Flask `@app.route` (GET
default, literal `methods=[...]`), and Express/Fastify `app.get` /
`fastify.post`; `app.all` is the `ALL` wildcard. Client anchors: TS/JS
`fetch("/x")` (GET unless a literal `method` option says otherwise) and
`axios.get/post/put/patch/delete("/x")`.

Matching is exact on `(protocol, method, path)` (a server `ALL` wildcard matches
any method), and every matching server anchor in another language gets an edge.
Paths are literal, start with `/`, lose the query/fragment, and collapse
duplicate slashes.

## Rebuild and parity

Whenever `index.Run` writes files, a bounded pass rebuilds every `xlang.*` edge
from the persisted anchors: delete only its own rule-prefixed rows, then insert
the matches. A cold, incremental, and second `--full` index therefore produce
identical cross-language edge rows (#49), and the pass never touches a static
edge (a same-key static edge wins). The cost is proportional to anchors, not to
files.

A schema upgrade needs no manual rebuild: a missing internal `xlang_indexed`
meta marker forces one full re-read of the existing files, so a database
migrated in place backfills its anchors. Git worktrees already discard a
stale-schema database before the indexer opens it.

## Limits (by design, not bugs)

- Literal same-origin paths only. Computed or template URLs
  (`` `/users/${id}` ``), full URLs with a host, and string concatenation
  produce no anchor.
- No path-parameter inference: FastAPI `/users/{id}` does not match a client
  `/users/123`. Trailing slashes are significant.
- Framework coverage is the short list above. Next.js file-system routes,
  Python `requests`/`httpx` clients, Rust clients, gRPC, FFI
  (pyo3/napi/wasm), and OpenAPI/shared schemas are not modelled yet (§16.7).
- A route with no client (or the reverse), a method mismatch, and a
  same-language pair produce no link.
- The link is a structural hint: "this call site names this route", not "this
  request was observed at runtime".

## Fixture evidence

`testdata/fixtures/xlang/http` is a TS client plus FastAPI, Flask, and Express
servers. The gate (`TestXlangHTTPBridge`, `TestXlangDefaultFloorHidesLinks`,
`TestXlangColdIncrementalFullParity`) pins:

- 11 anchors (6 client, 5 server);
- exactly 3 edges — `loadUsers → list_users` (GET), `createUser →
  create_user` (POST), `addItem → add_item` (Flask POST);
- no edge for the same-language Express pair, the missing handler, the dynamic
  URL, or the GET/POST mismatch;
- default `impact` (0.5) hides the links, 0.45 shows them, `path` is one hop;
- cold / incremental / full xlang row sets identical.

The fixture is source-only: no producer, no server, and no CI dependency.

## Adding a protocol

The anchor table is protocol-neutral. A new transport needs (1) extractor facts
emitting anchors with a `protocol`, `role`, `method`/`path` (or the protocol's
equivalent encoded in those fields), (2) a rule name `xlang.<protocol>.route`,
and (3) fixture positives/negatives. It does **not** need a new edge kind or a
resolver change. If a protocol cannot be expressed by the current anchor shape
(for example gRPC's service+method, or FFI's module+symbol), amend §4 with that
shape first.
