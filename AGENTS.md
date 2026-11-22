# AGENTS.md

repath turns a repository into a confidence-tagged symbol graph and answers
structural questions about it (`impact`, `path`, `map`, `find`, `callers`) for
humans and coding agents. One static binary, SQLite, no LLM, no daemon; the
optional read-only `repath serve` is the only HTTP surface.

**Current state: M5 + v1.1 + the v1.2 wave (SARIF, Python and Rust SCIP adapters, test `--diff`, real-repo evidence across TypeScript, Python, and Rust, real-client MCP verification, read-only HTTP serve, and GoReleaser distribution) plus the v1.3 LSP facts seam, cross-language HTTP links, and read-only runtime trace evidence.**
TypeScript/JS, Python, Rust, and Go are indexed; the MCP adapter + skill, artifact
export/pull, and worktree seeding are in, `repath check` adds graph-native CI
diagnostics, `repath check --sarif` attaches external compiler/linter SARIF 2.1.0
findings to enclosing symbols (it never runs the compiler), `repath adapter --scip` merges TypeScript/Python/Rust compiler facts into unresolved
references (`repath adapter --lsp-facts` ingests the same kind of facts from an
external LSP client), `repath flows` walks forward from detected entrypoints,
`repath tests` maps a change to structurally reachable test files, and
`repath mcp` speaks both the legacy handshake and the 2026-07-28 discovery era.
The measured 10k and 50k budgets live in PLAN §12 and are reproducible with
`make perf`.

## Read first

| File | Why |
| --- | --- |
| [`PLAN.md`](PLAN.md) | The contract: 25 locked decisions (§3), schema (§4), CLI/MCP contracts (§6–7), milestones and gates (§12) |
| [`docs/resolution.md`](docs/resolution.md) | The confidence ladder and the resolution gaps |
| [`docs/development.md`](docs/development.md) | Branch, commit, PR, definition of done |

If a change contradicts `PLAN.md` §3, either don't make it or amend §3 in the
same PR with the reason. Silent drift is the one thing this repo does not allow.

## Commands

```sh
make build      # bin/repath
make check      # gofmt + vet + tests — the PR gate
make test
make hooks      # install the commit-msg identity guard (once per clone)
make perf       # opt-in 10k + 50k synthetic budgets (outside make check)

./bin/repath status --json                 # indexes lazily, prints counts
./bin/repath index --full                  # rebuild
./bin/repath index --scope packages/web    # discovery limited to a monorepo subtree (repeatable)
./bin/repath impact <symbol|file:line>     # reverse closure, depth 8, limit 50
./bin/repath path <from> <to>              # shortest chain + alternates
./bin/repath map --max-tokens 1200         # PageRank, modules, cycles
./bin/repath map --md --out docs/repath-map # committable markdown bundle (index.md + one page per module)
./bin/repath export --graphml --out g.graphml  # full symbol graph for Gephi/yEd/Obsidian
./bin/repath export --dot --artifact out/     # read a local artifact without pulling it
./bin/repath export --dot --modules         # module-level graph for Graphviz
./bin/repath find <symbol>                 # card: callers/callees
./bin/repath callers <symbol>              # one-hop reverse edges
./bin/repath impact x --min-confidence 0.9 # import/this/local edges only
./bin/repath impact --diff origin/main     # union blast radius for a git change set
./bin/repath mcp                           # stdio JSON-RPC, six tools
./bin/repath serve --addr 127.0.0.1:0      # read-only HTTP, six answers
./bin/repath index --artifact out/         # export an artifact + manifest
./bin/repath index --artifact out/ --compress # zstd artifact (opt-in)
./bin/repath pull out/                     # seed the local index from one
./bin/repath check --fail-on warning       # graph-native diagnostics for CI
./bin/repath check --sarif out.ts.sarif    # attach external SARIF 2.1.0 findings
./bin/repath adapter --scip index.scip     # merge compiler facts, no overwrite
./bin/repath adapter --lsp-facts facts.json # merge external LSP facts, same policy
./bin/repath trace --file profile.cpuprofile # observed calls vs the static graph (read-only)
./bin/repath flows <entrypoint>            # forward execution paths
./bin/repath tests <symbol|file:line>      # structurally affected test files
./bin/repath tests --diff origin/main      # affected tests for a git change set
./bin/repath install --all                 # copy the embedded agent skill into user config dirs
./bin/repath uninstall --platform claude   # remove only the file we wrote
make bench-real REPATH_BENCH_ROOT=...       # opt-in real-repo evidence (not CI)
make bench-real-langs REPATH_BENCH_PY_ROOT=... REPATH_BENCH_RS_ROOT=...  # Python/Rust evidence
make bench-corpora                          # reproducible corpus matrix + accuracy (opt-in)
make bench-tokens REPATH_TOKENS_CORPORA=...  # answer tokens vs reading the named files (opt-in)
make release-snapshot                       # goreleaser current-platform snapshot
```

Sandbox note (this machine): Go lives at `/usr/local/go/bin` (not on `PATH`),
`$HOME` is read-only, so redirect caches into the workspace and disable ccache:

```sh
export PATH=/usr/local/go/bin:$PATH
export GOCACHE=$PWD/.cache/go-build GOPATH=$PWD/.cache/gopath \
       GOMODCACHE=$PWD/.cache/gomod CCACHE_DISABLE=1
```

## Layout and dependency direction

```
cmd/repath        entrypoint only
skills            embedded Agent Skill (skills/repath/SKILL.md)
internal/agentskill user-scope skill install/uninstall (pure file copy)
internal/cli      commands, text renderers, JSON, exit codes
internal/index    discovery, hashing, batching, module detection, orchestration, xlang pass
internal/lang     path → dialect; lang/{tsx,py,rs,golang} extract symbols + refs + imports
internal/resolve  global name index, confidence ladder, language-specific module rules
internal/store    SQLite schema and queries only
internal/graph    impact / path / map / flows walkers over store queries
internal/query    shared answer layer: open, resolve, six answers, status
internal/output   the MCP envelope and provenance cap
internal/mcp      hand-rolled stdio JSON-RPC adapter
internal/serve    read-only HTTP handlers over query/output
internal/artifact export/pull/merge inputs, manifest, schema gate
internal/merge    cross-repo snapshot: namespaced identity, xrepo links
internal/check    cycles, dead code, boundaries, hotspots, unresolved rate, SARIF
internal/adapter  SCIP and LSP-facts ingestion, group-guarded edge merge
internal/trace   read-only V8 CPU profile evidence (observed calls, no writes)
internal/testpath shared test-path classifier for check and tests
internal/eval     fixture gate (test-only)
```

`cli → index → {lang, resolve, store}`, `cli → query → {index, graph, store}`,
`cli → check → {graph, store}`, `cli → adapter → {store, index}`,
`cli → trace → store`,
`mcp → query`, `cli → serve → {query, output, store}`,
`artifact → {index, store}`.
Nothing in `store` knows about languages or parsing; nothing in `lang` touches
the database.

## Data model

- `.repath/index.db`, WAL, schema version in `internal/store/store.go`.
- **Nodes are symbols only.** Files and modules are dimensions
  (`files.id`, `files.module_id`); impact/map roll them up at read time. This is
  decision 8, amended in M2 — do not add file/module nodes without amending it.
- Edges are symbol→symbol: `CALLS`, `REFERENCES`, `EXTENDS`, `IMPLEMENTS`.
  Every edge carries `confidence` and a stable `rule` id; queries filter with
  `--min-confidence` (default 0.5).
- Unresolved references go to `pending`. Each run prunes dangling edges and
  retries pending identifier names that now exist (members are never guessed).
- Cross-language links are `REFERENCES` at **0.45** with `xlang.<protocol>.*`
  rules, derived from the `xlang_anchors` dimension (schema v6) and rebuilt by
  the xlang pass. They are not ladder rows and never a new edge kind (decision
  23). Default queries exclude them; `--min-confidence 0.4` opts in.
- `xlang_anchors` is an **input dimension** (like `imports`), not a node or edge
  table: literal protocol facts anchored to the enclosing symbol, replaced per
  file. The pass in `internal/index` owns matching; `store` only reads/writes.

## Repo rules

- `main` is protected: branch `type/scope` (`feat/m3-python`), conventional
  commits, PR required, CI green, squash merge.
- PR body follows [`.github/pull_request_template.md`](.github/pull_request_template.md):
  why, what changed, the one diagram that explains it, a validation table with
  real numbers, and an honest "not verified" line.
- No new dependency without a stated reason. Today's tree: tree-sitter grammar
  modules + `go-tree-sitter-bare` (cgo), `modernc.org/sqlite` (pure Go), and
  `github.com/klauspost/compress/zstd` (pure Go, opt-in artifact compression,
  approved for #18).
- Exit codes are contract: `0` ok, `1` internal, `2` usage, `3` not found,
  `4` ambiguous. `check` alone adds `5` for findings at or above `--fail-on`;
  never reuse it elsewhere.
- `gofmt` scope is `./cmd ./internal ./skills` on purpose: the local tool cache
  under `.cache/` must stay out of the formatter.
- Fixture expectations (`testdata/fixtures/*/expected.json`) and the eval gate
  are part of any resolution change. Never loosen a gate to make a PR pass.

## Landmines

- **cgo is required.** There is no maintained pure-Go tree-sitter (decision 15,
  amended in M0). Builds need a C toolchain; cross-compiles need a target one.
- **Grammar versions are pinned per module and they differ** (typescript
  `v1.9.4`, tsx and javascript `v1.9.2`, python `v1.9.10`, rust `v1.9.13`,
  go `v1.9.4`).
  Bumping one with
  `@latest` can drag the binding API and break the others; change them
  deliberately and rebuild.
- **The parser rejects some valid TypeScript**, e.g.
  `call<typeof import("x")>()`. Those files are stored with `parse_ok=0` and
  partial symbols. Report the count; never hide it. `export type *` (TS 5.0,
  rejected by the pinned grammar) is the one handled exception: the extractor
  retries a **same-length** copy with the `type` keyword blanked, accepts it
  only when that parse has no errors, and walks the copy, so offsets never
  move and the wildcard re-export is recorded (#122). Any other genuine error
  still reports `parse_ok=0`; do not generalize this into a source rewriter.
- **Member calls need a typed receiver** (`x.m()` resolves only when `x` is
  `new T()`, a parameter `x: T`, or a variable annotation). Without a type the
  reference stays unresolved by design — name guessing produced a false
  `BinaryHeap.push` hub with 219 callers and was removed.
- **Ambiguity policy:** 2–5 candidates become 0.4-confidence edges with the rule
  id visible; more than 5 are dropped. Do not "fix" an ambiguous reference by
  picking the first candidate.
- **Top-level references** (outside any function) have no source symbol and are
  skipped.
- **Python imports resolve at the repo root, `src/`, and `lib/` only.** There is
  no `sys.path`/`PYTHONPATH` reading and namespace packages without `__init__.py`
  are not resolved. That is a documented v1 gap, not a bug to paper over.
- **Python inheritance is walked in bounded MRO order.** `self.m()`/`cls.m()`,
  zero-argument `super().m()`, class receivers, and typed receivers follow the
  class's C3 linearization (cap 8, visited set, deterministic fallback for an
  inconsistent hierarchy) through same-file and imported bases. An external base
  (`unittest.TestCase`), an ambiguous base name, or the explicit
  `super(Child, self)` form ends the chain, so the call stays unresolved by
  design. A re-resolved file's base links replace its persisted ones
  (`AddInheritance`), and compiler (`scip.*`) base links never enter the MRO,
  which is what keeps cold/incremental/full graphs identical (#49). Receiver
  types are file-local: an attribute or factory return annotation declared in
  another module, and generic element types (`list[Service]`), stay untyped.
- **Python re-exports are followed through the imports table.** `py.import.reexport`
  covers `__init__.py` and ordinary modules such as `requests.compat`; a chain
  ending at the standard library is external, not pending. Explicit re-exports
  and **wildcard re-exports** (`from .impl import *` in `pkg/__init__.py`) are both
  followed, for `from pkg import Name` and for module members (`pkg.Name`): a
  declaration beats a named re-export beats a wildcard, wildcards apply in file
  order (last wins, Python's rule), the walk is capped at eight hops and
  cycle-safe, and a wildcard hop is `py.import.star` at 0.80 (#102; on django
  6,561 of 6,992 star edges were runtime-confirmed by Python's own import
  machinery, none contradicted). A literal `__all__` (a top-level list or tuple
  of string constants) bounds a wildcard hop: a name it omits stays pending, and
  the 0.60 recovery refuses to re-add it; a non-literal `__all__` is unknown and
  keeps the pre-#110 behaviour; Python's default underscore filter is not
  applied (#110). The pending retry
  still recovers a name the export walk cannot see (a deprecated `__getattr__`
  re-export, a chain past the cap) at 0.60 only when the referring file has an
  import binding for the name; without that provenance the `py.global.unique`
  guess is 0.40 (#97). Do not re-add a name-only guess above 0.5.
- **TypeScript barrels, workspaces, and dynamic imports are bounded inputs.**
  `export * from` and `export { a } from` are persisted as public import
  bindings and followed at ≤8 hops with a visited set; a declaration beats a
  named re-export beats `export *`; `export *` never carries a default; a chain
  ending at a dependency is external. A re-export creates no local binding, so
  it is **not** in the local-name imports map (Python/Rust re-exports are).
  `@scope/pkg` resolves from the root workspace manifest: the
  `pnpm-workspace.yaml` `packages:` list when present, else package.json
  `workspaces`, to the package's source entry
  (`exports`/`source`/`types`/`typings`/`module`/`main`, then `src/index`).
  Patterns support `*`/`?`/`[...]` and a bounded `**` walk, `!` negations are
  honored, and only the top-level `packages:` block is parsed — keep the YAML
  reader dependency-free and never read lockfiles, registries, `lerna.json`, or
  per-package overrides. A negated or unmatched package stays external; do not
  add a same-name fallback. Static
  `require("./x")` / `import("./x")` bindings, including bundler magic comments
  in the argument list, reuse the namespace/named rules; computed specifiers
  create no binding. `export * as ns` and CommonJS `module.exports` shapes are
  not modelled. A name a local barrel re-exports from a dependency is now
  external instead of a 0.60 same-name guess — keep that trade-off, do not
  re-add the guess.
- **Node ids are stable across re-index.** `ReplaceNodes` upserts by
  `(qname, arity)` so a file edit does not orphan incoming edges; keep that
  property when touching the indexer.
- **Pending retries only re-link identifiers.** `pending.form` exists so an
  untyped `obj.get()` is never retried as a repo-wide name guess. A recovered
  edge attaches to the same enclosing symbol the forward walk used: the retry
  re-derives `ref.Enclosing` (the packed references of a file this run resolved,
  or a re-extraction of the unchanged file on an incremental run) rather than
  the smallest node span, which misattributes a reference when one qname has
  more than one definition in a file (#106). A row with no such reference stays
  pending.
- **Unresolved reasons are a heuristic, computed at read time.** `status` and
  `check --json` bucket every pending row exactly once: `parse-failure`
  (`files.parse_ok = 0`), `member-scope` (`form = 'member'` and the name exists
  somewhere), `missing-definition` (no node has the name), `scope-gap` (name
  exists but the import/scope did not resolve), in that precedence; the buckets
  must sum to `unresolved`. The one implementation is the pure
  `store.SummarizePending`, called by `store.UnresolvedSummary` for status and
  by `check.Run` with the pending rows it already loads — do not reimplement it
  in `query`, `check`, `mcp`, or `serve`, and do not persist the buckets (that
  would be a schema change).
  Treat the buckets as a pointer to a resolver gap, not a compiler diagnosis:
  inheritance, macro-generated symbols, `export * as ns`, and workspace imports
  declared outside the root manifest (`lerna.json`, per-package overrides)
  legitimately land in `member-scope`/`scope-gap`. TypeScript barrels, workspace
  imports (package.json or pnpm), and static dynamic imports no longer do
  (#56/#79, see `docs/resolution.md`).
- **Rust module resolution is convention-based, with two explicit layout
  features.** Crate roots come from Cargo packages (`src/lib.rs` or
  `src/main.rs`), and a member's crate name comes from its Cargo.toml
  (`[lib].name`, else `[package].name`, hyphens → underscores). An unqualified
  path is read like a Rust 2018 uniform path: current module, then crate root
  (2015 style), then a local workspace member crate; an unindexed first segment
  is external. `#[path = "..."]` and inline `mod` blocks are persisted as
  reserved `@mod:<module path>` import bindings (inline has an empty source;
  `#[path]` resolves relative to the declaring file's directory plus the inline
  module path), while plain `mod x;` stays convention-based from the parent
  module's directory. Supertraits are reserved `@trait:<Trait>#N` bindings. Do
  not add a "best guess" fallback for any other layout; a missing local module
  file is a lookaside miss, not external.
- **Rust macro bodies are blind.** `macro_rules!` token trees and macro
  invocations create no symbols or edges. Typed receiver methods with no local
  definition are classified external (dependency or macro-generated trait impl)
  rather than guessed; keep that trade-off visible in docs.
- **Rust generic bounds are modelled but not inferred.** `T: Trait`,
  `T: A + B`, and `where T: Trait` are recorded per function/impl/trait scope; a
  member call on a type parameter follows each bound to a local trait's method
  declaration (`rs.generic.bound` 0.85), and same-repo supertraits are followed
  through reserved `@trait:` bindings (cap 4, cycle-safe). Multiple bound traits
  declaring the method stay a 0.40 group. A non-local bound is external and an
  unbounded type parameter is external (it can only call dependency or
  macro-generated methods). Generic containers, `impl Trait` parameters, and
  projections through an unknown impl stay unresolved. `IMPLEMENTS` is emitted
  only when the implementing type is defined in the same file; its links are
  resolved in a pre-pass and persisted so a typed call can follow a trait's
  default method (`rs.trait.method` 0.80), and a warm run loads the links of
  unchanged types so cold/incremental/full graphs agree (#49). Return typing is
  same-file only (`rs.typed.method.return` 0.80), and `Self::Assoc` resolves only
  through the same file's impl binding. No inference engine: do not guess.
- **Rust ambiguity is real.** Inherent and trait impls share `Type.method`;
  cross-file candidates and two implemented traits with the same default method
  become 0.40 edges with every candidate visible (`rs.typed.method.ambiguous`,
  `rs.generic.bound.ambiguous`, `rs.trait.method.ambiguous`, `rs.assoc.*.ambiguous`),
  `status` exposes `ambiguous` and `ambiguity_rate`, and the SCIP adapter may
  replace a group only with one compiler-named target. Never collapse them by
  picking one. One residual: node identity is `(qname, file, arity)`, so an
  inherent method and a trait-impl method of the same name in the *same* file
  are one node before resolution — that same-file case has no ambiguity row.
  Do not "fix" it by changing node identity in a resolver PR.
- **Rust enum variants are a heuristic.** A CamelCase associated call on a known
  enum becomes REFERENCES at 0.90; an associated constant with the same shape
  can be misclassified.
- **Go resolution is package-directory based, and implicit interfaces are not
  inferred.** A package is a directory of `.go` files; `go.mod` supplies the
  module path and local `replace` targets, and every other import path is
  external. Package-level names resolve across the directory (`go.package`
  0.90) and an imported symbol is exact (`go.import.member` 0.95). A member
  call needs a receiver type inferred **in the declaring file** from a
  parameter, `var`, `:=` composite literal, `new(T)`, or a same-file
  constructor's single named result (`go.typed.method` 0.90); a cross-file or
  cross-package constructor result needs an explicit type annotation, like
  Rust's same-file return typing. An interface receiver resolves to the
  interface's own method (`go.interface.method` 0.85), which is the static
  target; which concrete types implement an interface is **not** inferred (the
  next wave, bounded by the Graphify contract, not a same-name guess). Struct
  embedding is a `REFERENCES` at 0.80, interface embedding `EXTENDS` at 0.90.
  Build tags are ignored — every `.go` file is indexed — so build-tagged
  duplicate declarations are a visible 0.40 group, and `go.typed.method`
  duplicates are the same. Go adds the `const` and `var` node kinds for
  package-level declarations; no edge kind, schema, or rule id changed shape.
  Do not add a receiver-less member guess or collapse an ambiguity group.
- **Go package lookups must use the global name index.** `byFile` is partial on
  an incremental run by design; a package map built from it loses unchanged
  files and breaks cold/incremental parity (#49). `goPackages` is built from
  `byQName` plus `paths`, and the gin parity check (2,336 edges / 877 pending,
  identical cold/incremental/full) is the regression gate.
- **A merged cross-repo index is a projection, not a resolver.** `repath merge`
  copies single-repo artifacts into one snapshot: every file path is prefixed
  with its repo name (identity stays per repo; identical qnames are never
  merged), and type-like symbols whose qname is unique inside each of two parts
  become `REFERENCES` at **0.45** with rule `xrepo.same_type_as` — below the
  default floor like xlang (decision 23), so single-repo answers are unchanged.
  Imports and anchors are not copied, nothing is re-resolved, entrypoints are
  re-anchored to the new node ids, and pending rows keep their new file ids.
  Cross-repo `calls` stay deferred because `pending` carries no enclosing
  symbol. Output must be byte-identical on a second run: parts, files, nodes,
  pending rows, and entrypoints are copied in sorted order and the manifest has
  no timestamp (`TestMergeIsByteIdentical` covers a repo with entrypoints). The
  output directory is never overwritten (exit 2), a missing part is exit 3, and
  a schema mismatch is exit 1. Do not add an implicit merged read to a command;
  `export`/`map --md --artifact` are the read-only seams.
- **Module cycles** are computed on cross-module edges at ≥0.5; one noisy edge
  can merge modules into one SCC. Treat cycles as hints.
- **Artifacts default to uncompressed SQLite; zstd is opt-in.** `--compress`
  writes `manifest.format: "sqlite+zstd"` and `pull` reads both forms. The
  schema gate runs for both. The only compression dependency is the approved
  `github.com/klauspost/compress/zstd`, imported by `internal/artifact`; keep
  the default path dependency-free at runtime and never change the container
  silently.
- **Worktree cache is immutable.** `$GIT_COMMON_DIR/repath/commits/<sha>.db`
  is chmod `0444` and only copied into a worktree's active
  `.repath/index.db`; never open it for writes and never put a live database on
  a network filesystem. Dirty trees patch the active DB and must not write the
  cache.
- **`.repath/` must not count as dirty.** The worktree clean check filters it;
  a user repo may not have added it to `.gitignore` yet.
- **The cache schema is sidecar-checked.** `<sha>.db` is trusted only when
  `<sha>.db.schema` matches `store.SchemaVersion`; bumping the schema rebuilds
  old caches instead of seeding them.
- **Release is tag-triggered and draft.** `v1.3.0` (2026-11-20) is the first
  tagged release; its notes live in `docs/releases/v1.3.0.md`. `.goreleaser.yaml` builds Linux
  amd64/arm64 with cgo and injects `internal/cli.version`; arm64 needs
  `CC=aarch64-linux-gnu-gcc`, never a pure-Go cross-compile. The
  `.github/workflows/release.yml` workflow runs only on a `v*` tag, installs
  the target C toolchain, and creates a **draft** GitHub release. Do not create
  a tag or publish a release without explicit approval. `goreleaser check` and
  `make release-snapshot` are local checks; no Go dependency is involved. See
  `docs/install.md`.
- **`make bench-real` is opt-in evidence, not a gate.** It pins
  `elastic/kibana` @ `05511966780217f7357b98253299f369dc40199b`, subtree
  `x-pack/solutions` (38,548 indexed TS files), in `docs/benchmarks.md`. It
  removes the clone's `.repath/`, briefly touches ten source files and restores
  them, and logs budget misses instead of failing. Issue #43 closed the three
  #31 misses: cold **19.2 s** (<3 min), 10-file incremental **582 ms** (<1 s),
  and `map` p95 **223 ms** (<300 ms); RSS, `impact`/`callers`/`path`, and the
  26.2% zstd ratio also pass. Issue #56 (TS barrels/workspaces/static dynamic
  imports) re-ran the same matrix on a private copy: edges **61,904 → 62,489**,
  pending **158,503 → 157,205** (`scope-gap` 4,486 → 3,535, no pending rows
  added), ambiguous **5,242 → 5,138**, cold **27.4 s**, 10-file incremental
  **737 ms**, `map` p95 **313 ms** on a load-4 host, with cold / incremental /
  second `--full` edge and pending **row sets identical**.
  Since #79 (2026-11-21) Kibana's `pnpm-workspace.yaml` is read: a cold run
  measured **64,797 edges / 157,253 pending** (2,156 `ts.import.workspace*`
  over 562 target files; the +48 pending rows are local package entries whose
  named export is invisible — anonymous defaults and local alias re-exports
  such as `export { X as Y }`), with cold / incremental / second-`--full` row
  sets identical. Since #122 the same-day `export type *` parse fix leaves
  Kibana's edges and pending unchanged (tree-sitter error recovery already
  recorded those bindings): parse failures drop 158 → 52 and only
  `status.parse_failures` and the `parse-failure` bucket move (355 → 353).
  Run-to-run variance under host load can push
  incremental to ~1.3 s and `impact` p95 past 150 ms; report medians and the
  load caveat, never turn it into CI, and do not weaken the synthetic
  `make perf` budgets because of it. `make bench-real-langs` is the #48
  companion: `psf/requests` @ `3c7baaa` (37 files) and `rayon-rs/rayon` @
  `599425d` (191 files), with pinned `scip-python` 0.6.8 and `rust-analyzer`
  1.97.0 files; it runs the same matrix, rebuilds `.repath/` for the adapter
  before/after, and splits the Rust gain into production, test, and
  `rayon-demo` edges. Large corpora: `django/django` @ `f24f38c` (3,051 indexed
  files, 906 production / 2,009 test `.py`) and `Goutam-11/hyusk` @ `06bdb2b`
  (88 indexed files, 49k Rust LOC, 750-crate workspace). `scip-python` 0.6.8
  OOMs on the full Django tree at Node's default heap; it succeeds with
  `NODE_OPTIONS=--max-old-space-size=4096` (2,941 `.py`, **2:08** wall,
  **3.74 GiB** max RSS, 100.3 MiB index), and the documented bounded fallback
  is `--target-only django` (909 files) at a 3 GB heap. `rust-analyzer` needs
  ~158 s / 7.1 MiB for `hyusk`. Both targets stay opt-in; CI needs no producer.
  `scip-python` 0.6.8 also mis-resolves wildcard package attributes
  (`models.CharField` becomes `DateTimeField#`), so a Python **member** slice
  (`pkg.Name`) is not SCIP-attributable — only the `from pkg import Name`
  identifier slice is. Audit a Python rung with the opt-in
  `scripts/audit-python-star.py` (a Python interpreter that can import the
  corpus; never CI) instead of scoring members from that producer (#105).
- **`make bench-corpora` is reproducible evidence, not a gate.** Issue #61's
  `scripts/bench-corpora.sh` pins `django` @ `f24f38c`, `hyusk` @ `06bdb2b`,
  `requests` @ `3c7baaa`, and `rayon` @ `599425d` (repo + commit + optional
  SCIP), clones missing clones only under `.cache/`, refuses a wrong HEAD or a
  dirty tracked tree, and runs `TestBenchRealLanguages` N=3 in separate
  processes. It writes `<outdir>/summary.json` (host/CPU/RAM/load before+after,
  Go/Rust/Python/Node/git versions, per-metric medians/ranges, graph structure
  including `unresolved_by_reason`, adapter stats) plus one JSON per run, a
  condensed `accuracy-<corpus>.json`, and `failure-modes.json`. The checked-in
  `bench/baseline.json` (text only, no DB/artifact) plus generated
  `docs/benchmarks-history.md` are the history: a plain run prints deltas, and
  only `--update-baseline` rewrites them. `TestSCIPAccuracy` is the opt-in
  compiler check on `requests`/`rayon`: it samples 20 targets deterministically,
  compares `callers` and `impact` depth 2 against SCIP definitions/references
  using the adapter's own `EnclosingNode` mapping, is scope-matched to the SCIP
  document set, and counts its exclusions. It is a sampled measurement with
  documented caveats, not a correctness guarantee; `path` has no ground truth.
  Missing producer input is `3`, malformed protobuf is `1` (not `2`), empty is a
  zero-fact `0`, a non-git `index` falls back to full discovery at `0`, and a
  read-only `.repath/` is `1`. Never make any of this a CI gate and never
  commit corpora, databases, artifacts, or `.cache/`.
- **Incremental change detection is git-aware.** With a matching
  `meta.git_head`, `git status -uno` plus one tagged `ls-files -t` pass decide
  what to re-read: clean tracked files skip both stat and hash, untracked files
  are always re-hashed, and skip-worktree paths are omitted until sparse
  checkout clears the bit. A missing git, a different HEAD, or `--full` falls
  back to full discovery. The pending retry is a `pending_name`-indexed query
  over the resolver's unique names and is skipped when no symbol was added or
  removed; a cold run completes it so cold and warmed graphs agree for the #43
  TypeScript tree. Warm Python/Rust/TypeScript resolution lazily loads the
  import bindings of a re-export chain's unchanged modules (`importsOf`, one
  `imports_file`-indexed read per file, one bulk read after 256 files).
  TypeScript barrel hops go through the same path (#56): the walk is bounded at
  eight hops, cycle-safe, and loads only the barrels a reference actually
  reaches, so the Kibana incremental budget stays met. A `--full` run also
  completes the pending retry, like a cold run (#49). Measured parity on the #48 corpora: `requests` stays
  **543 pending / 1,031 edges** and `rayon` **2,048 / 2,161** after a ten-file
  comment edit and after a second `index --full`, with identical pending and
  edge row sets and no edge+pending pair. Keep the lazy load error-checked and
  bounded; do not drop the retry from the cold/full path. Measured parity after
  #56 on the Kibana tree: a ten-file comment edit and a second `index --full`
  reproduce the cold graph's **62,489 edge rows and 157,435 pending rows**
  exactly (path and qname, rule, confidence, line, and ambiguous candidate ids).
  Edge and pending writes are multi-row statements, and the WAL
  is checkpointed after a run. Keep those properties: reverting to per-file
  pending deletes or hashing every file brings back the #43 cold/incremental
  regressions.
- **Cold-run memory is bounded by packed references.** A cold run parses the
  whole tree before it can resolve anything, so the indexer retains each file's
  references in the compact `facts.PackRefs` form (decoded one file at a time by
  `facts.UnpackRefs`) and drops symbols, imports, entrypoints, and anchors once
  they are persisted. That is what keeps the pinned Kibana tree at ~633 MiB
  cold RSS instead of 1.09 GiB (#75). A new `facts.Ref` field must be added to
  the codec and its round-trip test in the same change; do not return to
  retaining `[]facts.Ref` until resolution (1.17M refs ≈ 258 MiB on Kibana).
- **`make bench-tokens` is opt-in evidence, not a gate.** `TestAnswerTokens`
  (`REPATH_TOKENS=1`, `REPATH_TOKENS_CORPORA="name=path[,name=path|scope]"`)
  indexes each corpus into a temporary store and compares the JSON answer bytes
  with the bytes of the files the answer names, using one estimator on both
  sides (`ceil(bytes/4)`). It never writes into a corpus, never runs in CI, and
  skips when the corpora are absent. The documented bias: answers are location
  lists, the file side is whole-file text, so the ratio is "what an agent skips",
  not "what the answer explains"; the `map` row is the JSON payload while
  `--max-tokens` bounds the text renderer. See `docs/benchmarks.md`
  §Answer tokens vs reading the files.
- **`make perf` is opt-in.** `TestPerf10k` and `TestPerf50k` skip without
  `REPATH_PERF=1`; `make perf` runs each in its own process so the Linux VmHWM
  reading belongs to that tree. It is evidence, not a CI gate (timings are
  machine-dependent). The 50k target is <3 min and <1 GB RSS; the 10k budgets
  are unchanged and must stay green.
- **Test impact is structural reachability, not coverage.** `repath tests` uses
  graph reverse edges first, then path conventions and imports at 0.50; it must
  say so. The classifier is `internal/testpath.IsTest`, shared with `check`; do
  not copy it. `--diff [base]` discovers changed files once through
  `index.ChangedFiles` (one `git diff --name-status -z` implementation), then
  reuses the per-file `query.Tests` walker; `impact --diff` uses the same
  `index.ChangedFiles` finder with the per-file `query.Impact` walker and carries
  `changed_file`; do not add a second diff engine.
  Each link carries `changed_file`; a changed test file is included as
  `testpath.changed` at 1.00/depth 0; deleted/non-indexed paths go to
  `diff.skipped`. Base order is explicit, `origin/main`, `main`, `HEAD`; bad
  refs/non-git exit `3`, clean diffs exit `0` without opening an index, and
  untracked files require `git add`.
- **`--scope` is a discovery filter with rebuild semantics.** `index --scope DIR`
  (repeatable) filters discovery but keeps the repo root for module detection
  and git invalidation; the set is recorded in `meta.scope` (absent = whole
  repo), `status` reports `scope`/`scope_files`, and an artifact manifest records
  it. A plain `index` or a different scope set **wipes the graph and rebuilds**
  (`store.ResetGraph`) — never splice two scopes — while every read command
  preserves the recorded scope, including across a HEAD change (the scope is
  carried out of the stale database before it is replaced). The HEAD-keyed
  commit cache describes the whole tree: a scoped index is never seeded from it
  and never stored into it, so a fresh worktree or a cache seed starts
  whole-repo. Do not add `--scope` to read commands or a per-scope cache file
  without a plan change. See `docs/scope.md`.
- **Exports are read-only projections, not a renderer.** `map --md` and
  `export --graphml|--dot` read the same index and write no new nodes, edges,
  rules, schema, or pending rows; they add no dependency (`encoding/xml` plus
  text). Both are byte-identical across runs (sorted modules/symbols/edges), and
  `map --md` bounds the whole bundle with `--max-tokens` while admitting when the
  structural pages alone exceed it. Do not add HTML, SVG, a layout engine, or an
  Obsidian-vault writer. `export` and `map --md` read a **local artifact** in
  place with `--artifact DIR|FILE` (`store.OpenReadOnly`, no pull, no writes
  beside it; a stale manifest or database schema is exit 1, a missing artifact
  exit 3), and the output is byte-identical to the live index. The vendored
  GraphML 1.0 schema set under `testdata/export/graphml/` is **test-only**:
  `REPATH_XSD=1` opts into `xmllint` validation and `make check` needs neither
  `xmllint` nor a viewer. See `docs/export.md`.
- **Entrypoints are a dimension, not nodes.** The `entrypoints` table (schema
  v6) anchors rules such as `ts.route.express`, `py.route.fastapi`, and
  `rs.main` to existing symbols. Do not turn them into a node kind or let
  framework detection change node identity. `flows` follows forward edges only
  at the confidence floor; it is structural, not a runtime trace.
- **`serve` is read-only and six answers.** `repath serve` opens the existing
  index through `store.OpenReadOnly` (SQLite `mode=ro` + `query_only`), never
  indexes/migrates/writes, and refuses a missing index with exit `3`. It
  exposes only `/find`, `/impact`, `/path`, `/callers`, `/map`, `/status` — no
  `flows`/`tests` endpoint, no mutation, no artifact route. `/status` does not
  refresh; staleness is reported. Loopback is the default; a wider `--addr` is
  unauthenticated and warns. Do not add a UI, auth, or a seventh answer.
- **MCP is dual-era and stays six tools.** Legacy `initialize` is retained at
  2024-11-05. Modern requests carry `_meta` protocol version; `server/discover`
  advertises 2026-07-28, unsupported versions return `-32022`, and cacheable
  results carry `resultType`/`ttlMs`/`cacheScope`. Do not add a seventh tool,
  an SDK, or a schema generator.
- **Adapter edges never overwrite a resolved tree-sitter edge.** `repath
  adapter --scip` consumes `pending` references and may replace exactly one
  tree-sitter **ambiguous** candidate group (confidence 0.40, rule suffix
  `.ambiguous`, 2-5 same-name candidates). The narrow exception: when a SCIP
  reference occurrence maps to the group's source node and line and names one
  of its candidate targets, `store.ReplaceAmbiguousGroup` deletes only that
  group's candidate rows (re-checking source, line, kind, confidence < 0.5,
  rule suffix, and candidate membership in one transaction) and inserts the
  exact `scip.<lang>.*` edge at 0.99. Exactly one compiler occurrence must
  disambiguate the group; two same-line occurrences leave it alone and are
  counted, never merged. `store.MergeEdges` still reports conflicts instead of
  upserting, no row at or above 0.5 is ever touched, and every replacement is
  reported as `replaced` / `replaced_edges` in stats; there is no schema
  change (stats live in `meta` and `status.adapters` under `scip` /
  `scip-python` / `scip-rust`). A group the compiler cannot disambiguate stays
  ambiguous and visible. Rules are prefixed `scip.ts.*`, `scip.py.*`, or
  `scip.rs.*` at confidence 0.99. The Python and Rust adapters accept
  `scip-python` and `rust-analyzer scip` documents through the same hand-rolled
  parser and share the merge policy. Rust symbol strings have a narrow parser
  (`rustSymbolName`: crate/module and `local` skipped; types, terms, enum
  variants, fields, and `impl#[T][Trait]method()` reduced to display names);
  extend it with focused cases, not a general SCIP resolver. The pinned
  `testdata/fixtures/rs-adapter` SCIP file is produced externally by
  `rust-analyzer` 1.97.0 and CI must not need a Rust toolchain. Do not turn the
  adapter into a silent replacement for the resolver, and do not add a
  SCIP/Node/Python/Rust dependency to the build.
- **LSP facts are a file seam, not an embedded client.** `repath adapter
  --lsp-facts FILE` reads a versioned JSON file (`schema: 1`) written by an
  external client; repath never starts a language server and CI never needs one
  (the committed fixture is `testdata/fixtures/lsp-adapter`). It uses the same
  pending-first merge and the same #57 ambiguous-group replacement guard, with
  `lsp.ts.*` / `lsp.py.*` / `lsp.rs.*` rules at `0.99` and stats under
  `meta.adapter.lsp*`. The file's 1-based columns are used only to count and
  dedupe occurrences inside one run; they are never persisted (schema is 6).
  A pending key merges only when the compiler reports at least as many
  occurrences as there are pending rows, and several distinct targets merge
  only when occurrences pair one-to-one with rows of one shape; otherwise the
  key stays pending and is counted. A missing file is exit `3`, malformed input
  is `1`, both/neither adapter flag is `2`, and a valid empty file is `0`. See
  `docs/adapters.md` and keep the fixture gate (`TestLSPFactsAdapterGate`)
  meaningful.
- **Cross-language links are derived and opt-in.** The `xlang` pass in
  `index.Run` rebuilds every `xlang.*` edge from the `xlang_anchors` dimension
  whenever a run wrote files: it deletes only its own rule-prefixed rows and
  re-inserts `REFERENCES` at **0.45** (`xlang.http.route`) where a client anchor
  and a server anchor in a different language share protocol + method + path.
  The 0.45 confidence is below the default 0.5 query floor, so default answers
  are unchanged; `--min-confidence 0.4` opts in. Rebuilding every write is what
  keeps cold/incremental/`--full` edge row sets identical (#49) — do not make
  it incremental without proving parity. No node/edge kind change, no name
  guessing: literal paths only, no templates, full URLs, path-parameter
  inference, or same-language pairs. A missing internal `xlang_indexed` meta
  marker forces one full re-read after a schema upgrade so anchors backfill;
  git worktrees discard a stale-schema database anyway.
  `testdata/fixtures/xlang/http` is the gate
  (`TestXlangHTTPBridge`, `TestXlangDefaultFloorHidesLinks`,
  `TestXlangColdIncrementalFullParity`). See `docs/xlang.md`.
- **Trace evidence is read-only, never a graph edge.** `repath trace --file
  PROFILE` parses a V8/Chrome CPU profile, maps frames by repo file + line and
  requires the enclosing symbol's name to equal the frame's function name, then
  reports observed caller→callee pairs as `dyn.trace` at 0.60, split into
  static-confirmed (an edge exists at ≥0.5) and observed-only. Nothing is
  persisted: no rule row enters the DB, and no default answer changes. Skipped
  frames are counted by reason (`external-file`, `no-function`, `unmapped-file`,
  `no-enclosing`, `name-mismatch`); a same-named function elsewhere is never a
  fallback, and shifted/stale profiles are refused rather than attributed.
  Missing input is `3`, malformed input is `2`, `5` stays check-only, and the
  six MCP/HTTP answers are unchanged. The gates are `internal/trace` and
  `internal/cli`; see `docs/trace.md`.
- **`check` is graph-native, not a compiler.** Dead code only considers private
  functions/methods; exported API, test paths, parse failures, and special
  entrypoint names are excluded. Boundary checks are objective heuristics
  (non-test → test, cross-module private access), not an intended-layering
  policy. Module cycles stay warnings because one noisy edge can merge an SCC.
  Do not add a config file to make these "smarter" without a plan change.
- **SARIF is an opt-in bridge, not a compiler frontend.** `check --sarif` reads
  SARIF 2.1.0 that an external tool already produced; repath never runs `tsc`,
  `pyright`, `ruff`, or `rustc`. Results attach to the enclosing symbol at read
  time as `diagnostic` findings with `sarif.<tool>.<ruleId>` rules; they are not
  persisted. Duplicates collapse on `(rule, file, line, message)`; results
  outside a symbol are counted as skipped. Missing input is exit `3`,
  malformed/non-2.1.0 input is exit `2`, and `5` stays check-only. Do not add a
  language server or a new dependency for this.
- `.cache/` and `.repath/` are local artifacts. Never commit them.

## Adding a language

1. `internal/lang/<lang>/extract.go` returning the shared
   `facts.Result{Symbols, Refs, Imports}` shape, with its own dialect type.
2. Register extensions in `internal/lang/lang.go`.
3. Add resolution rules (relative imports, packages/`__init__` re-exports) in
   `internal/resolve`, with new rule ids and confidences in the ladder.
4. `testdata/fixtures/<lang>/` + `expected.json`; wire the per-language gate in
   `internal/eval` from PLAN §12.
5. Update `docs/resolution.md`, `PLAN.md` status, `README.md`.

Adding a command follows the same shape: `internal/graph` walker → `internal/cli`
command + renderer → tests → docs.

## Before opening a PR

```sh
make check                                   # must be clean
make perf                                    # when touching index worktree/query paths
./bin/repath check --fail-on none            # check the check (should be clean here)
./bin/repath status --json                   # on this repo, sanity
./bin/repath impact <symbol> --json | head   # smoke on a fixture copy
go test ./internal/eval -v                   # logs precision/recall
```

PR checklists that matter: a test (or a stated reason), no new dependency
without justification, `PLAN.md` updated when behaviour or a decision changes,
and the "not verified" line filled in truthfully.
