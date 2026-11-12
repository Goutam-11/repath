# repath — plan v1

> **Structural answers, not graph edges.**
> One static binary, zero config, no LLM, no server. Ask what breaks, how two
> things connect, or where the important code is — and get a finished answer,
> confidence-tagged, as text for a human or JSON for an agent.

Status: **M0–M5 landed; v1.1 `check`, the SCIP adapter, entrypoint flows, test-impact mapping, dual-era MCP, and opt-in artifact compression landed.** This document is the contract for v1 and its first v1.1 slice.
Scope changes require editing the decision table, not just the code.

---

## 1. Why this exists

Codebases are graphs, but the tools people and agents use are linear: grep,
file reads, jump-to-definition. An agent that needs to know what a change
touches currently spends turns chaining `grep → read → grep`. A human
onboarding to a repo has the same problem with worse tools.

repath builds a deterministic graph of a repository (symbols, files, modules;
calls, references, imports, inheritance) and answers **structural questions
about it** — with the answer already traversed, ranked, and bounded, because
that is the shape both consumers can actually use.

## 2. Positioning against prior art

| System | Category | Approach | What we take / leave |
|---|---|---|---|
| **Serena** (~31k★) | Agent code intelligence | LSP client, no graph | Take: small tool surface. Leave: per-language servers, no cross-language or whole-repo view. |
| **code-graph-rag** (~5.3k★) | Code graph + RAG | tree-sitter → Memgraph, LLM→Cypher | Take: language-agnostic parse. Leave: LLM in the query path, server DB, non-determinism. |
| **Potpie** (~5.8k★) | Code graph + agents | Neo4j, agent workflows | Confirms the demand; confirms the infra weight we refuse. |
| **Sourcetrail** (~16.5k★, archived 2021) | Human comprehension | Own parsers, GUI | Take: the "explain my codebase" need. Leave: GUI and per-language frontends. |
| **Joern** (~3.6k★) | Code Property Graph | AST+CFG+PDG | Take: schema rigor. Leave: CFG/PDG cost for v1. |
| **Kythe / Glean** | Infrastructure indexing | Typed facts, Datalog | Take: node/edge thinking. Leave: compiler-grade frontends per language. |
| **Stack Graphs** (archived Sep 2025) | Name resolution | File-local facts, resolve at query time | Take: the incremental architecture — our core design borrows it. |
| **ctags / cscope / GNU global** | Classic index | Regex/heuristic | The baseline we must beat on *relations*, not on symbol listing. |
| **RepoGraph / LocAgent** | Research | Line-level graphs for LLM localization | Take: evidence that graph context helps. Leave: research harnesses. |
| **~10 new Rust "code graph MCP" repos** | Commodity | tree-sitter + SQLite + MCP | 0–17★ each — the idea is trivially reproducible. The moat is answer quality. |

Two hard constraints inherited from the field:

1. **Kuzu was archived (Oct 2025)** after an Apple acqui-hire. No embedded graph
   DB dependency; SQLite it is.
2. **Agents don't chain hops.** A 2026 SWE-bench study (arXiv 2610.33958, ~79k
   episodes) found graph tools roughly double small-model localization accuracy,
   but the gain is name lookup at hop 0 plus *one* expansion at hop 1; agents
   chain expansions in only 12–14% of episodes, and each extra hop cuts the odds
   of success by 0.5–0.67×. Therefore: **the tool performs the traversal and
   returns the closure.** Never return raw edges and hope.

## 3. Locked decisions

| # | Area | Decision | Why |
|---|---|---|---|
| 1 | User | Agent-first; humans read the same answers | One query engine, two renderers; no divergent logic |
| 2 | Relation core | Symbols, imports, calls, inheritance; transitive | Answers "what breaks" without runtime data |
| 3 | Flagship | `path <A> <B>` — how two things connect | Pure graph, no conventions, works on day one |
| 4 | Commands | `index/status, find, callers, impact, path, map` | Every verb answerable from the core graph |
| 5 | Agent API | 6 answer-shaped MCP tools + JSON CLI | Small tool surface = reliable tool selection |
| 6 | Human output | Structured text on stdout + `--json` | Pipeable, diffable, zero dependencies |
| 7 | Proof | Golden fixtures with P/R gates in CI | Regression protection without an LLM harness |
| 8 | Nodes | Symbols are the only nodes; file and module are dimensions (amended in M2) | Same rollup power, no 10k no-op nodes, no drift after incremental |
| 9 | Modules | Manifest-first, directory fallback | Zero-config that survives monorepos |
| 10 | Identity | Scoped qname + kind + arity; lines for display | Survives edits, disambiguates overloads |
| 11 | Fidelity | tree-sitter + scope heuristics + confidence; optional SCIP adapter merge (v1.1) | Zero setup; compiler facts are opt-in and never required |
| 12 | Ambiguity | Emit at low confidence with candidates + rule id | Complete answers; precision is the user's choice |
| 13 | Languages | TypeScript/JS, Python, Rust | Modern web + AI + systems coverage |
| 14 | Storage | Single SQLite file, recursive CTEs | Zero infra, incremental writes, one shippable file |
| 15 | Stack | Go; cgo only for tree-sitter (amended in M0) | No maintained pure-Go tree-sitter exists; cgo is confined to `internal/lang/*` |
| 16 | Freshness | Hash + git-diff incremental; <1s/10 files; 50k in <3 min | Monorepo-viable without a daemon |
| 17 | Cold start | Lazy auto-index at `.repath/index.db` | An agent never has to know `index` exists |
| 18 | v1 cut | Exactly this spec — no UI, no embeddings, no entrypoints; v1.1 adds entrypoint detection as a symbol dimension (no new node kind) | Ship something trustworthy, then add the "how it works" story without changing identity |
| 19 | Transport | CLI primary; `repath mcp` stdio adapter; SKILL.md | 49-tool MCP measured at 88.9× schema overhead; ~300 on-demand tokens beat ~1.5k always-on |
| 20 | Worktrees | Per-worktree DB seeded by copying a commit-keyed cache | Parallel agents own separate trees; sub-second seed, zero lock contention |
| 21 | Sharing | Commit-keyed CI artifact + local dirty overlay | Snapshot distribution like SCIP→Sourcegraph; never a network-FS database |
| 22 | Diagnostics | Structural `check`; opt-in SARIF 2.1.0 ingestion attaches external diagnostics and never runs the compiler (amended in v1.2) | The compiler already did the work; reading its log is a bridge, not a language frontend |

Assumptions to veto if wrong: **MIT license**, **no config file** (flags only).

## 4. Data model

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;

CREATE TABLE meta    (key TEXT PRIMARY KEY, value TEXT);
-- schema_version, repath_version, git_head, indexed_at, root

CREATE TABLE modules (id INTEGER PRIMARY KEY, name TEXT, kind TEXT, root TEXT);
-- kind: workspace | package | crate | dir

CREATE TABLE files   (id INTEGER PRIMARY KEY, path TEXT UNIQUE, module_id INT,
                      lang TEXT, size INT, mtime INT, hash TEXT,
                      parse_ok INT, err TEXT);

CREATE TABLE nodes   (id INTEGER PRIMARY KEY, kind TEXT, name TEXT, qname TEXT,
                      file_id INT, module_id INT, lang TEXT, arity INT,
                      start_line INT, end_line INT, exported INT,
                      UNIQUE(lang, qname, file_id, arity));

CREATE TABLE edges   (src INT, dst INT, kind TEXT, confidence REAL, rule TEXT,
                      line INT, candidates TEXT,
                      PRIMARY KEY (src, dst, kind));

CREATE INDEX edges_rev ON edges (dst, kind, confidence);
CREATE INDEX nodes_qn  ON nodes (qname);
CREATE INDEX nodes_loc ON nodes (file_id, start_line);

-- unresolved names, re-linked when a new export appears nearby
CREATE TABLE pending (name TEXT, file_id INT, kind TEXT, form TEXT, line INT);

-- import bindings are resolution inputs, not edges: they let a later run
-- follow package/__init__.py and compat re-exports without re-parsing files
CREATE TABLE imports (file_id INT, local TEXT, imported TEXT, source TEXT, line INT,
                       public INT);

-- entrypoints are a dimension over symbols, not nodes; one symbol can anchor
-- several rules (a main that is also a test).
CREATE TABLE entrypoints (
    id INTEGER PRIMARY KEY, node_id INT, file_id INT, rule TEXT,
    name TEXT, confidence REAL, line INT,
    UNIQUE (node_id, rule)
);
```

**Node kinds:** `function, method, class, struct, trait, interface, type, enum, const`.
**Edge kinds:** `CALLS, REFERENCES, EXTENDS, IMPLEMENTS`.

Invariants:

- Every edge is symbol→symbol. Files and modules are **dimensions** on rows
  (`files.id`, `files.module_id`), not nodes: rollups group by them at read time.
- Imports are resolution inputs, not edges; they live in `pending` semantics and
  the resolver, not in the graph.
- File- and module-level impact is **computed at read time** by grouping symbol
  edges. No rollup rows exist in the database.
- `confidence` ∈ `(0,1]`; `rule` is a stable id (`ts.import.named`,
  `rs.use.super`, `py.module.relative`, …) used by tests and by `status`.
- Adapter facts add edges only: rules are prefixed `scip.ts.*` or `scip.py.*`,
  stats live in `meta.adapter.<name>` (`scip`, `scip-python`), and no adapter
  row is a node or a schema change.
- SARIF diagnostics are ingested per `check` run and attached to the enclosing
  symbol at read time (`sarif.<tool>.<rule>`); they are never persisted as
  nodes, edges, or a schema change (decision 22, amended in v1.2).

### 4.1 Storage forms — one format, three lives

```
<repo>/.repath/index.db                        working copy (WAL, gitignored)
$GIT_COMMON_DIR/repath/commits/<sha>.db        immutable, shared across worktrees
out/repath-<sha>.sqlite + manifest.json       CI artifact, read-only
```

- Paths are stored **repo-relative POSIX only**, plus `meta.root` for validation.
  That is what lets an artifact move from CI to laptop to teammate.
- Measured size on a real 38,548-file TypeScript tree: **65.6 MiB
  uncompressed, 17.2 MiB zstd (26.3%)**. The earlier 250–400 MB estimate was
  wrong by roughly 4× because the artifact holds compact symbol rows and no
  source text; see `docs/benchmarks.md`.
- An artifact contains structure (names, paths, kinds, line refs), never source
  text. Still internal: symbol names leak intent, so no public caches.
- `manifest.json`: `schema_version, tool_version, commit, file_count,
  node_count, edge_count, unresolved_rate, indexed_at`. A binary refuses an
  artifact whose `schema_version` it does not know, with a one-line upgrade hint.

> **M5 amendment — artifact compression.** The sketch above named
> `.sqlite.zst`. M5 shipped an uncompressed `.sqlite` instead: zstd would add a
> dependency beyond the two the project allowed (§11), and the manifest schema
> gate does not depend on the container.
>
> **v1.1 amendment (#18).** Compression is now opt-in via
> `repath index --artifact out/ --compress`, using the approved third dependency
> `github.com/klauspost/compress/zstd`. `manifest.format` is authoritative
> (`sqlite` or `sqlite+zstd`), `pull` reads both, and the schema gate runs for
> both. Measured on the synthetic trees: 10k **3.8 MiB → 991 KiB** (25.3%),
> 50k **19.2 MiB → 4.8 MiB** (25.2%); 50k export **127 → 190 ms**, pull
> **11.8 → 30.9 ms**. The default remains uncompressed.

### 4.2 Worktrees — the primary multi-agent case

Worktrees share the object store but have their own working directory, HEAD, and
index. Parallel agents in worktrees therefore see different file trees from the
same commit.

1. Build/keep an immutable DB per commit in the **git common dir**, never in the
   worktree: `$GIT_COMMON_DIR/repath/commits/<sha>.db`.
2. On query in a worktree: if HEAD is cached and `git status --porcelain` is
   empty → **copy** the base DB. If dirty → copy, then patch only changed and
   untracked files through the normal incremental path.
3. Active DB lives at `<worktree>/.repath/index.db`, so two agents never contend
   for the same SQLite file.
4. `index.lock` via `O_EXCL` with pid + timestamp; stale locks are recoverable.
5. Eviction: keep the 5 most recent commit DBs, LRU.

File-copy seeding is why node IDs may stay autoincrement. Merging base + overlay
at query time (`ATTACH` + `UNION`) was rejected: it forces deterministic
content-hashed node IDs, and that is a schema decision that must be made before,
not after, M4.

### 4.3 Team sharing — distribute snapshots, not a database

- `repath index --artifact out/` writes the DB plus manifest.
- `repath pull <path|url>` seeds the local cache; dirty files overlay
  automatically, so a developer never waits for a cold index.
- CI runs `repath impact --diff origin/main --artifact out/ --json` → PR
  annotation, or an exit code for a gate.
- **Never** a shared mutable DB: SQLite locking over network filesystems
  corrupts, there is exactly one writer, and every developer's dirty state
  differs. A read-only `repath serve` (HTTP) is a v1.2 candidate, not v1.

## 5. Resolution and confidence

Facts are extracted per file in isolation (defs, refs with their scope chain,
imports, exports). Resolution then walks a ladder:

| # | Rule | Confidence | Example |
|---|---|---|---|
| 1 | Exact via explicit import / alias | 1.00 | `import { cart } from './cart'` |
| 2 | Relative path or `self::` / `super::` | 0.90 | `use super::store::{Store}` |
| 3 | Same-module unique match | 0.85 | sibling `def helper()` |
| 4 | Unique global name, no import | 0.60 | bare `parse()` |
| 5 | Ambiguous — several candidates | 0.40 | DI, `getattr`, macro-generated |
| 6 | No candidate | dropped | counted in `status`, never silent |

`--min-confidence` defaults to `0.5`. It is the completeness/precision dial and
the threshold used by the eval harness, so "accuracy" is a measured curve rather
than a claim.

Per-language rules that must exist before that language ships:

- **TS/JS:** named/default/namespace imports, `export *` re-exports, arrow
  functions assigned to `const`, class methods, interface/type declarations,
  `tsconfig` `paths` aliases (`@/*`), CommonJS `require`, dynamic `import()`.
- **Python:** absolute and relative imports (`from . import x`, `from ..pkg`),
  module and `__init__.py` re-exports (including stdlib shims such as
  `requests.compat`), decorators recorded as reference edges, class and
  classmethod receivers, `self`/`cls` calls, and `Constructor()` receiver typing.
  The `if __name__ == "__main__"` marker is emitted as `py.main` when a
  module-level `main()` exists (v1.1 entrypoints; see `docs/flows.md`).
- **Rust:** `mod` tree, `use` with braces/globs/aliases and `pub use`
  re-exports, inherent vs trait `impl` blocks (methods keyed by receiver type),
  `Self`/`self`, associated functions, `pub` visibility, enum-variant
  construction, trait method dispatch ambiguity, and explicit blind spots
  around macro token trees. Workspace cross-crate imports stay external.

## 6. CLI contract

```
repath index  [--full] [--json]
repath status [--json]
repath find   <name>            [--module M] [--json]
repath callers <sym>            [--depth 1] [--min-confidence 0.5] [--json]
repath impact <sym|file:line>   [--depth 8] [--limit 50] [--json] [--module M]
repath tests  <sym|file:line>   [--depth 8] [--limit 50] [--json]
repath tests  --diff [base]     [--depth 8] [--limit 50] [--json]
repath flows  <entrypoint>      [--depth 6] [--limit 20] [--json]
repath path   <A> <B>           [--max-depth 8] [--alternates 3] [--json]
repath map    [--max-tokens 2000] [--json]
repath check  [--min-confidence 0.5] [--fail-on error|warning|info|none]
              [--max-findings 200] [--sarif FILE|DIR ...] [--json]
repath adapter --scip FILE [--json]
```

Symbol references accept `qname`, `name`, `file:line`, or `file:name`.

`impact` output (text):

```
cart.total  function  src/cart.ts:42
  7 dependents · 4 files · 1 module · min-confidence 0.50

depth 1
  checkout.checkout        src/checkout.ts:88   0.95  calls
  api.postCheckout         src/api/routes.ts:31 0.90  calls
  cart.test.total          src/cart.test.ts:12  0.60  references
depth 2
  checkout.recalculate     src/checkout.ts:104  0.95  calls
  …

affected files
  src/checkout.ts     3 symbols
  src/api/routes.ts   1 symbol
```

Exit codes: `0` ok · `2` usage · `3` not found · `4` ambiguous (candidates
printed, or in `error.candidates` for JSON) · `5` check findings at or above
`--fail-on` · `1` internal. `5` is only produced by `check`; the other commands
never reuse it.

## 7. MCP contract

Six tools, all returning the same envelope, in two protocol eras:

```json
{
  "answer": { },
  "summary": "7 dependents across 4 files; highest-confidence path via checkout()",
  "confidence": 0.9,
  "truncated": false,
  "provenance": [{"file": "src/checkout.ts", "line": 88, "rule": "ts.import.named"}]
}
```

| Tool | Arguments | Returns |
|---|---|---|
| `find_symbol` | `name`, `module?` | symbol card, locations, counts |
| `impact` | `symbol`, `depth?`, `min_confidence?` | grouped dependents + affected files/modules |
| `path` | `from`, `to`, `max_depth?` | chain(s) with per-edge provenance, or nearest frontier |
| `callers` | `symbol`, `depth?` | one-hop (or N-hop) reverse edges |
| `repo_map` | `max_tokens?` | ranked symbols per module + module edges + cycles |
| `status` | — | counts, unresolved rate, staleness, parse failures |

No raw-edge CRUD tools, no `graph_query` DSL. Transport: stdio JSON-RPC,
hand-rolled (~300 LOC). Legacy era: `initialize`, `tools/list`, `tools/call`.
Modern era (`2026-07-28`): no handshake; every request carries
`params._meta.io.modelcontextprotocol/protocolVersion`, `server/discover`
advertises `supportedVersions`/capabilities/identity, and unsupported versions
return `-32022` with the supported list. Both eras keep exactly the six tools
and the envelope.

### 7.1 Transport strategy — CLI first, MCP second

Measured, not assumed:

- 49 configured MCP tools = **72,612 characters** of tool definitions vs 817
  characters of bare names — **88.9×** overhead, paid every turn because schemas
  load up front.
- Task-level runs put MCP at **4–32× more tokens per task**; like-for-like
  comparisons find **no inherent protocol advantage** — the difference is
  scaffolding and how much each call returns (GitHub MCP: 172 KB vs 8 KB for an
  equivalent CLI query).
- Therefore the payload discipline in §7 matters more than the transport, and
  repath's 6 tools (~1.5k tokens of schema) stay the maximum.

Ship:

1. **CLI is the implementation.** Everything works through it; JSON is the API.
2. **`repath mcp`** — stdio adapter in the same binary, 6 tools, M5.
3. **`skills/repath/SKILL.md`** (~300 tokens, loaded only when relevant) so
   shell-capable agents discover the CLI without carrying schemas. This is the
   highest-leverage 2026 pattern.
4. **`--help --json`** so agents can enumerate commands mechanically.

MCP exists for clients without a shell (desktop panes, IDE sidebars, remote
setups). An agent with bash should call the CLI. Costs of MCP accepted knowingly:
client config entry, process lifecycle, version skew, a second test surface.

## 8. File discovery

1. If the repo is a git work tree: `git ls-files -z --cached --others --exclude-standard`
   → tracked **and** new-but-not-ignored files, ignore rules for free.
2. Otherwise: directory walk with default excludes
   (`.git, node_modules, .venv, venv, __pycache__, target, dist, build, vendor, .next, .turbo`).
3. Skip binary/minified/generated files by sniff (NUL byte, `*.min.js`,
   `@generated`, size > 1 MB) and record the skip count in `status`.

## 9. Incremental indexing

```
discover → hash (xxhash) → changed = hash differs ∪ deleted ∪ renamed
changed → parse → replace facts for that file (single transaction)
        → resolve new refs
        → re-resolve `pending` rows whose name is now exported
        → invalidate closure cache entries for touched nodes
```

Invariants:

- Facts are file-local and immutable; only resolution is global.
- One writer goroutine owns the SQLite handle (single-writer DB), batches of
  500 rows per transaction.
- Re-resolution is bounded by `pending`, never a full repo scan.
- Deleting a file deletes its nodes, edges (both directions), and pending rows.

## 10. Performance budget

| Metric | Target |
|---|---|
| Cold index, 10k files | < 60 s |
| Cold index, 50k files | < 3 min |
| Incremental, 10 changed files | < 1 s |
| Warm `impact` / `callers`, p95 | < 150 ms |
| `path`, p95 (depth ≤ 8) | < 250 ms |
| `map --max-tokens 2000` | < 300 ms, ≤ 2k tokens |
| Resident memory, 50k files | < 1 GB |

## 11. Layout and dependencies

```
cmd/repath/main.go
internal/
  cli/        flag parsing, command dispatch, exit codes
  index/      discovery, hashing, parse pool, pipeline, transactions
  lang/       registry + tsx/ py/ rs/ extractors (defs, refs, imports)
  resolve/    scope chains, import maps, confidence rules
  graph/      queries: impact, path, map, callers, find
  store/      schema, migrations, upserts, checkpoint, pending queue
  query/      shared answer layer (open, resolve, six answers, status)
  check/      graph-native diagnostics (cycles, dead code, boundaries, hotspots,
              plus opt-in SARIF 2.1.0 ingestion attached to symbols)
  testpath/   shared test-path classifier for check and tests
  adapter/    compiler facts (SCIP) merged into pending references
  artifact/   pack/pull commit-keyed snapshots, manifest, schema gate
  mcp/        stdio JSON-RPC server, tool schemas
  output/     text renderer, JSON envelope, token budgeting
skills/repath/SKILL.md       agent instructions for the CLI
testdata/fixtures/{ts,py,rs}/{src,expected.json}
docs/         resolution.md, mcp.md, sharing.md, worktrees.md, check.md
```

Dependencies (deliberately three): the `go-sitter-forest` grammars with
`go-tree-sitter-bare` (cgo, confined to `internal/lang/`), `modernc.org/sqlite`
(pure Go), and `github.com/klauspost/compress/zstd` (pure Go, v1.20.4, only
imported by `internal/artifact` for opt-in export compression; approved for
#18). No cobra, no ORM, no MCP SDK — stdlib flags, handwritten SQL, handwritten
JSON-RPC.

> **M0 amendment.** There is no maintained pure-Go tree-sitter runtime; the
> grammars and binding are cgo. The cost is real: a C toolchain is required to
> build, and cross-compilation needs the target toolchain (musl for a static
> linux binary). The benefit is a real parser instead of a hand-rolled scanner,
> so the trade was taken deliberately rather than silently.

## 12. Milestones

| | Deliverable | Definition of done |
|---|---|---|
| **M0** | Walk + hash + TS definitions + `status` + `--json` | `repath status --json` on a real repo reports files/nodes/parse failures; fixture walk test passes |
| **M1** | TS/JS resolution, `find`, `callers`, eval harness | TS fixture CALLS P ≥ 0.90, R ≥ 0.75; unresolved ≤ 15%; `--min-confidence` changes results |
| **M2** | `impact`, `path`, `map`, text renderer | 10k-file repo: `impact` p95 < 200 ms; typical impact answer ≤ 40 lines; `map` respects budget |
| **M3** | Python | PY fixture P ≥ 0.88, R ≥ 0.80 |
| **M4** | Rust (impl/traits, mod tree, macro blind spots) | RS fixture R ≥ 0.70; ambiguity rate visible in `status` |
| **M5** | MCP adapter, skill, artifact export/pull, worktree seeding, incremental, perf, docs | Cold 10k < 60 s; 10-file delta < 1 s; artifact CI→laptop round-trip verified; second worktree warm in < 2 s; MCP round-trip in a real client; README quickstart → first answer < 60 s |

Order is deliberate: Rust (highest uncertainty) is staffed after the query
engine is proven, but before MCP polish, so its ambiguity informs the output
design rather than being discovered at the end.

**M0 landed** (PR #3): 710 files / 3,497 nodes in 538 ms on excalidraw, 20 ms to
re-verify an unchanged tree, 5 parse failures (0.7%) — every one the same
tree-sitter-typescript gap around `call<typeof import("...")>()`, reported in
`status` rather than dropped.

**M1 landed** (PR #4): import/alias-aware resolution with the confidence ladder,
`find` and `callers`, and a fixture gate at precision 1.00 / recall 1.00 (9/9
edges, 1 unresolved). On excalidraw: 6,447 `CALLS`, 2.1 s full index, 56 ms
incremental. 9,248 references stay unresolved, dominated by member calls without
typed receivers — the honest ceiling of tree-sitter resolution and the first
target for the v1.1 adapter seam.

**M2 landed** (PR #5): module detection from package.json workspaces (directory
fallback), `impact` (reverse closure, depth cap, cycle-safe), `path` (shortest
chain, alternates, nearest-frontier fallback), and `map` (PageRank, module graph,
SCC cycles, token budget). On a synthetic 10k-file chain: index 2.8 s, `impact`
p95 25 ms, `map` 120 ms. Member calls now use locally inferred receiver types
instead of name guessing: the false `BinaryHeap.push` hub (219 callers) vanished,
excalidraw's `CALLS` fell 6,447 → 3,976, and the real hubs (`t`,
`isArrowElement`) rose to the top.

**M3 landed** (Python): absolute/relative module resolution (root, `src/`,
`lib/`), `__init__.py` and ordinary-module re-exports through a persisted
`imports` table (schema v3, extended to v4 in M4, and to v5 for entrypoints), `self`/`cls` calls, class and classmethod receivers,
constructor-style receiver typing, decorator references, and Python class bases.
The fixture gate is 31/31 edges at precision 1.00 / recall 1.00 with 1 genuine
unresolved callback parameter, above the 0.88/0.80 milestone gate (not loosened).
On `psf/requests` @ `3c7baaa`: 37 files, 787 nodes, 0 parse failures, 707
`CALLS` + 283 `REFERENCES` + 41 `EXTENDS` in 81 ms; 543 references stay
unresolved, dominated by untyped member calls (`get`, `send`, `append`, `json`)
and inheritance (`Session.should_strip_auth`), the documented ceiling. A 1-file
incremental edit is 27 ms; node ids are now stable across re-index, so a no-op
edit leaves the edge set and unresolved count unchanged.

**M4 landed** (Rust): Cargo package/workspace modules, crate/self/super module
resolution, use trees (named, alias, glob, module, `pub use` re-exports),
`Self`/assoc/self/typed receivers, struct literals and enum variants as
`REFERENCES`, and `IMPLEMENTS` for same-file trait impls. Ambiguity is now
visible in `status` (`ambiguous`, `ambiguity_rate`). The fixture gate is 27/27
edges at precision 1.00 / recall 1.00 with 0 unresolved and 2 ambiguous edges,
above the 0.70 recall gate (not loosened). On `serde-rs/json` @ `8c535a2`:
72 files, 1,676 nodes, 136 ms, 844 `CALLS` + 147 `REFERENCES` + 9 `IMPLEMENTS`,
37 ambiguous edges (3.7%), 596 unresolved; the one parse failure is the
intentionally invalid UI fixture `tests/ui/parse_expr.rs`. A 1-file incremental
edit is 24 ms with unchanged counts. Remaining unresolved references are
dominated by generic trait parameters, macro-generated impls, and external
collection/formatting methods — the documented Rust ceiling.

**M5 landed** (product surface): `repath mcp` is a hand-rolled stdio JSON-RPC
adapter with exactly six tools and the §7 envelope; a pipe round-trip test and
the official TypeScript MCP client SDK 2.2.0 both verified it. Artifacts export
with `index --artifact out/` (consistent `VACUUM INTO` snapshot + manifest,
`0444`) and `pull` seeds a laptop, overlays a dirty checkout, and refuses an
unknown schema version with a one-line upgrade hint and no half-seeded index.
Worktrees seed from
`$GIT_COMMON_DIR/repath/commits/<sha>.db` under an `O_EXCL` lock, patch dirty
trees without touching the cache, and evict LRU beyond five commits.

The reproducible 10k-file synthetic tree (100 modules × 100 files, 9,900
`CALLS`) is `make perf-10k`: cold index **5.0 s**, 10-file incremental **237 ms**,
`impact` p50/p95 **21.7 / 24.5 ms**, `callers` **12.4 / 13.1 ms**, `path`
**15.9 / 17.1 ms**, `map` **183 / 197 ms**, MCP `impact` round-trip **38 ms**
(p50/p95 over 20 samples; `map` over 5). A second worktree warms from the clean
cache in **12.9 ms** (budget <2 s). A cold build of the binary (fresh Go build
cache, modules already downloaded) is **21.8 s**, so README quickstart → first
answer on the 10k tree is **~27 s** (budget <60 s). Every §10 budget is met; the
artifact fixture (14 TS files) exports to **73,728 B** and pulls in **0.05 s**.
M5 artifacts were uncompressed; v1.1 adds opt-in zstd (§4.1).

**v1.1 landed** (`repath check`): five graph-native checks with a shared
report and `--json`: module cycles (warning), dead private functions/methods
(warning), objective boundary violations (non-test → test is an error;
cross-module private access is a warning), fan-in/out hotspots (info), and
per-module unresolved rate (warning). `--fail-on none|error|warning|info` gates
CI; `check` is the only command that returns exit code `5`. Definitions and
limitations are in `docs/check.md`.

**v1.1 artifact compression landed** (issue #18): `repath index --artifact
out/ --compress` writes a zstd-compressed SQLite snapshot with
`manifest.format: "sqlite+zstd"`; `pull` reads both forms and the schema gate
runs before and after decompression. The dependency is the approved
`github.com/klauspost/compress/zstd` v1.20.4, imported only by
`internal/artifact`; the default artifact is still uncompressed. Measured
10k **3.8 MiB → 991 KiB**, 50k **19.2 MiB → 4.8 MiB**; 50k export
**127 → 190 ms**, pull **11.8 → 30.9 ms**. See `docs/sharing.md`.

**v1.1 MCP negotiation landed** (issue #17): the adapter is dual-era. It still
answers the legacy `initialize` handshake at 2024-11-05, and now implements the
2026-07-28 `server/discover` path with per-request `_meta`, `resultType`,
`ttlMs`/`cacheScope` on cacheable results, and `-32022` for an unsupported
version. Exactly six tools and the envelope are unchanged; `tools/list` and
`tools/call` were verified with `@modelcontextprotocol/client` 2.4.0 (modern,
pinned) and `@modelcontextprotocol/sdk` 1.32.2 (legacy). The hand-rolled JSON-RPC
server is still dependency-free. See `docs/mcp.md`.

**v1.1 test-impact mapping landed** (issue #15): `repath tests <target>`
returns the test files that structurally reach a changed symbol/line, with a
link per piece of evidence (test symbol, affected symbol/file, depth,
confidence, rule, call line). Graph reachability is primary; path conventions
(`testpath.convention`, 0.50) and test imports (`testpath.import`, 0.50) only
fill files with no graph link. The test-path classifier now lives in
`internal/testpath` and is shared with `check` (no duplicate copy). Output is
text + JSON, bounded by depth and `--limit` with `truncated`. It is explicitly
structural, not runtime coverage. There is no MCP tool and no `--diff` mode yet.
See `docs/tests.md`.

**v1.1 entrypoint flows landed** (issue #14): `repath flows <entrypoint>`
walks forward from a detected entrypoint and returns bounded leaf paths with
per-edge rule/confidence, or the nearest frontier when no outgoing edge exists.
Detection is a symbol dimension (`entrypoints` table, schema v5), not a new node
kind: Express/Fastify route handlers, Next.js route exports, FastAPI/Flask route
decorators, Python `test_` functions and `__main__` guards, Rust `main`, and
Rust `#[test]` functions. `status`/`map` expose counts and rule ids in JSON.
The heuristics and false positives are in `docs/flows.md`. No MCP tool was
added; the six-tool surface is unchanged.

**v1.1 adapter landed** (issue #13): `repath adapter --scip FILE` parses a
SCIP index in-process and merges compiler facts only into tree-sitter `pending`
references. Adapter edges carry `scip.ts.method`/`scip.ts.call`/`scip.ts.new`/
`scip.ts.reference` at confidence 0.99; `store.MergeEdges` never overwrites an
existing edge and reports a conflict instead. `status --json` exposes per-adapter
coverage, skips, and conflicts in `adapters[]`. No node kind, node identity, or
schema changed; adapter stats live in `meta`. On the pinned TypeScript fixture
unresolved goes **1 → 0**; on `microsoft/vscode-uri` @ `8b86482` (8 files) it
goes **91 → 69** (rate 81.25% → 63.89%) with **18** new edges and **0**
conflicts. The tree-sitter fixture gates are unchanged; the adapter-on gate is
`TestSCIPAdapterGate`. Setup and limits are in `docs/adapters.md`.

**v1.1 50k budget landed** (issue #16): the reproducible tree is 500 modules ×
100 files (49,900 `CALLS`), generated by `make perf-50k`; each budget run is a
separate process so Linux `VmHWM` is that tree's own peak. Measured here:

| Metric | 50k result | PLAN §10 target |
|---|---|---|
| Cold index | **13.4 s** | <3 min |
| Peak RSS | **278 MiB** | <1 GB |
| 10-file incremental | **699 ms** | <1 s |
| `impact` p50/p95 | **57.1 / 61.1 ms** | <150 ms p95 |
| `callers` p50/p95 | **33.5 / 35.6 ms** | <150 ms p95 |
| `path` p50/p95 | **42.1 / 45.3 ms** | <250 ms p95 |
| `map --max-tokens 2000` p50/p95 | **216.4 / 222.6 ms** | <300 ms p95 |
| `check` | **464 ms** | no target |
| artifact export / pull | **19.2 MiB · 127 ms / 11.8 ms** | no target |
| MCP `impact` round-trip | **88 ms** | no target |
| cold index + first query | **13.5 s** | — |

`map` first missed its p95 budget (432 ms) because `BuildMap` joined both
endpoint symbols for every edge and allocated a map per PageRank iteration. It
now reads edge skeletons (no joins) into dense slices: 50k p95 **217–228 ms**, and
the 10k p95 fell from 187 ms to **82 ms**. The 10k budgets were not loosened.
The synthetic files are one-line functions, so the 19.2 MiB artifact was not
evidence for the old 250–400 MB real-repo estimate; the real measurement is
**65.6 MiB** for 38,548 files (see `docs/benchmarks.md`), and the timings and
memory here remain the synthetic 50k claim.

**v1.2 SARIF diagnostics landed** (issue #28): `repath check --sarif FILE|DIR`
reads SARIF **2.1.0** only, attaches each result to the symbol enclosing its
first location, and adds `diagnostic` findings with `sarif.<tool>.<ruleId>`
rules. `error`/`warning`/`note` map to error/warning/info; duplicates are
collapsed on `(rule, file, line, message)`; results outside any symbol are
counted as skipped. Diagnostics count toward `--fail-on`, and `check` remains
the only command returning `5`. Missing input is exit `3`, malformed JSON or a
non-2.1.0 version is exit `2`. No schema, node kind, edge, or dependency
changed; the pinned fixtures are `testdata/fixtures/sarif/{ts,py}` and the
structural report is unchanged when `--sarif` is absent. See `docs/check.md`.

**v1.2 Python SCIP adapter landed** (issue #29): the shared SCIP parser now
accepts Python documents from `scip-python` 0.6.8 and merges them under the
distinct adapter name **`scip-python`**, with `scip.py.method`/`call`/`new`/
`reference` rules at confidence `0.99`. The merge policy is unchanged: only
`pending` rows are candidates, a conflict is reported instead of overwriting a
tree-sitter edge, and matched pending rows are removed. On the pinned Python
fixture unresolved goes **2 → 0**; on `psf/requests` @ `3c7baaa` it goes
**543 → 523** (rate **34.50% → 33.27%**) with **18** edges, **20** matched,
**0** conflicts, and a **0.27 s** adapter run. The tree-sitter fixture gates are
unchanged; the adapter-on gate is `TestPythonSCIPAdapterGate`. No Go dependency
was added. See `docs/adapters.md`.

**v1.2 test-impact diff landed** (issue #30): `repath tests --diff [base]`
discovers changed files with one git command (`git diff --relative
--name-status -z`), refreshes the current tree, and unions the existing
per-file `query.Tests` answer. Each link gains `changed_file`; a changed test
file is included directly as `testpath.changed` at 1.00/depth 0; deleted and
non-indexed paths are listed under `diff.skipped` instead of being guessed. The
base order is explicit, `origin/main`, `main`, `HEAD`; a bad ref or non-git
directory exits `3`, a clean diff exits `0` without opening an index. Untracked
files still require `git add`. See `docs/tests.md`.

**v1.2 real-repository validation landed** (issue #31): the pinned target is
`elastic/kibana` @ `05511966780217f7357b98253299f369dc40199b`, subtree
`x-pack/solutions` (42,491 files, 38,562 TS/JS; the full tree has 114,967 TS/JS
and is out of the 100k+ scope), measured with `make bench-real`. repath indexed
**38,548** files / 87,478 nodes / 61,894 edges; peak RSS **853 MiB** cold and
**987 MiB** overall. The synthetic §10 table above is **unchanged and still
green** (this run: 10k cold 6.0 s, 10k incremental 247 ms, 50k cold 15.2 s, 50k
incremental 709 ms, 50k map p95 245.7 ms, zstd 25.2%). The real tree honestly
**missed** three targets: cold **4 min 29 s** vs <3 min, 10-file incremental
**4.81 s** vs <1 s, and `map` p95 **455.6 ms** vs <300 ms; `impact`/`callers`/
`path` passed at p95 **118.9 / 73.0 / 130.1 ms**, and the artifact is **65.6 MiB
→ 17.2 MiB (26.3%)**, export **408 → 659 ms**, pull **41 → 126 ms**. No code
optimization was attempted in this validation issue; the numbers and the
not-verified list are in `docs/benchmarks.md`. `make bench-real` stays opt-in
and is not a CI gate.

## 13. Evaluation harness

- `testdata/fixtures/{ts,py,rs}` — one small but realistic repo per language,
  with `expected.json` hand-labeled by kind.
- `go test ./internal/eval` computes precision/recall at the default confidence
  plus the unresolved and ambiguous counts; CI fails on regression.
  Per-language gates live in the test table, so adding a language cannot
  silently lower an existing gate.
- A `make bench` target indexes a pinned mid-size OSS repo per language and
  prints the table from §10.
- Ambient check (not a gate): run `impact` on 20 random symbols in a real repo
  and confirm the top-3 dependents by hand.

## 14. Risks

| Risk | Signal | Mitigation |
|---|---|---|
| Rust macros hide/invent symbols | low recall on `rs` fixture | confidence ladder surfaces it; never claim exactness |
| Dynamic dispatch (Python/JS) | many 0.40 edges | keep candidates; `--min-confidence` dial; document the ceiling |
| Deep CTE queries | `impact --depth 8` slow | cycle guard via path array, bidirectional search for `path`, closure cache |
| Vendored/generated noise | long file lists | git-driven discovery + content sniff, reported in `status` |
| Confidence drift | P/R gates go red | gates per edge kind; `rule` ids never renamed silently |
| Artifact/binary skew | old snapshot, new binary | `manifest.schema_version` gate, explicit refusal, one-line upgrade hint |
| Worktree seed cost | 300 MB copy per new worktree | `copy_file_range`/reflink when available, LRU to 5 commit DBs |
| Scope creep (tests, entrypoints, UI) | new node kinds mid-milestone | non-goals list is binding until v1 ships |
| Grammar rejects valid TS | files with `parse_ok=0` | surfaced in `status`; partial symbols kept; upgrade grammar, never hide the count |
| Member-call precision | untyped receivers leave member calls unresolved (12k on excalidraw) | types come only from annotations, `new`, and parameters; `--min-confidence` filters the rest; a type-aware adapter is the v1.1 fix |

## 15. Non-goals for v1

Runtime/model edges (routes, DB tables, env, queues) · test coverage
collection · embeddings or any LLM in index or query · watch daemon · local HTTP server ·
multi-repo · FFI/cross-language edges · UI/TUI/HTML · C/C++/Java/Go parsers ·
config file. **Impact is structural, not test-aware**, and `repath tests` is structural
reachability rather than runtime coverage. That remains the known ceiling.

## 16. Deferred (revisit after v1)

1. File and module nodes with `DEFINES`/`CONTAINS` edges, arriving with M2
   impact rollups rather than as an empty shell in M1.
2. `repath check` (v1.1, landed): import cycles, dead code, module-boundary
   violations, fan-in/out hotspots, unresolved-rate per module — graph-native,
   CI exit codes. Defined in `docs/check.md`; cycles stay warnings because a
   single noisy edge can merge an SCC.
3. SARIF ingestion (v1.2, landed): opt-in `check --sarif FILE` reads a pinned
   SARIF 2.1.0 log, attaches each result to its enclosing symbol, and counts
   skipped/duplicate results. It is a bridge, not a compiler frontend; see
   `docs/check.md`. Decision 22 was amended in the same PR.
4. Entrypoint/flow detection (v1.1, landed): Express/Fastify/Next.js,
   FastAPI/Flask, pytest, Python `__main__`, Rust `main`/`#[test]`; see
   `docs/flows.md`. MCP still exposes the same six tools.
5. Test-impact mapping (v1.1, landed): `repath tests <target>` maps the
   structural reverse closure to test files; v1.2 adds `--diff [base]`, which
   unions the per-file answer for a git change set. See `docs/tests.md`.
6. Adapter seam (v1.1, landed): SCIP facts merged into pending references;
   Python landed in v1.2 under the separate `scip-python` adapter name. LSP and
   Rust adapters are still deferred. See `docs/adapters.md`.
7. Cross-language edges (TS `fetch` → route → Python handler, OpenAPI, gRPC).
8. `repath serve` read-only HTTP for dashboards and shell-less clients.
9. Watch mode / LSP server for editors.
10. Distribution: goreleaser, Homebrew, `go install`, GitHub Action.

## 17. Decision log

Four rounds of structured questions, all recommended options taken unless noted.
Round 1 — product identity, relation semantics, fidelity, storage, stack,
languages (Rust added by request). Round 2 — flagship explanation, command set,
agent surface, human output, freshness contract, ambiguity handling. Round 3 —
node taxonomy, module boundaries, symbol identity, cold start, cut line, proof
strategy. Round 4 — transport (CLI + MCP adapter + skill), worktree seeding,
team artifact sharing, diagnostics scope. Output: the 22 decisions in §3.

---

Next action: **v1.1 wave** — `check`, the 50k budget, the SCIP adapter,
entrypoint flows, and test-impact mapping have landed. The remaining candidates
are MCP protocol negotiation (#17) and opt-in artifact compression (#18); the
largest quality gap remains member calls without typed receivers, now partly
addressed by the opt-in adapter.
