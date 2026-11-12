# AGENTS.md

repath turns a repository into a confidence-tagged symbol graph and answers
structural questions about it (`impact`, `path`, `map`, `find`, `callers`) for
humans and coding agents. One static binary, SQLite, no LLM, no server.

**Current state: M4 merged.** TypeScript/JS, Python, and Rust are indexed.

## Read first

| File | Why |
| --- | --- |
| [`PLAN.md`](PLAN.md) | The contract: 22 locked decisions (§3), schema (§4), CLI/MCP contracts (§6–7), milestones and gates (§12) |
| [`docs/resolution.md`](docs/resolution.md) | The confidence ladder and the resolution gaps |
| [`docs/development.md`](docs/development.md) | Branch, commit, PR, definition of done |

If a change contradicts `PLAN.md` §3, either don't make it or amend §3 in the
same PR with the reason. Silent drift is the one thing this repo does not allow.

## Commands

```sh
make build      # bin/repath
make check      # gofmt + vet + tests — the PR gate
make test

./bin/repath status --json                 # indexes lazily, prints counts
./bin/repath index --full                  # rebuild
./bin/repath impact <symbol|file:line>     # reverse closure, depth 8, limit 50
./bin/repath path <from> <to>              # shortest chain + alternates
./bin/repath map --max-tokens 1200         # PageRank, modules, cycles
./bin/repath find <symbol>                 # card: callers/callees
./bin/repath callers <symbol>              # one-hop reverse edges
./bin/repath impact x --min-confidence 0.9 # import/this/local edges only
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
internal/cli      commands, text renderers, JSON, exit codes
internal/index    discovery, hashing, batching, module detection, orchestration
internal/lang     path → dialect; lang/{tsx,py,rs} extract symbols + refs + imports
internal/resolve  global name index, confidence ladder, language-specific module rules
internal/store    SQLite schema and queries only
internal/graph    impact / path / map walkers over store queries
internal/eval     fixture gate (test-only)
```

`cli → index → {lang, resolve, store}`, `cli → graph → store`. Nothing in
`store` knows about languages or parsing; nothing in `lang` touches the database.

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

## Repo rules

- `main` is protected: branch `type/scope` (`feat/m3-python`), conventional
  commits, PR required, CI green, squash merge.
- PR body follows [`.github/pull_request_template.md`](.github/pull_request_template.md):
  why, what changed, the one diagram that explains it, a validation table with
  real numbers, and an honest "not verified" line.
- No new dependency without a stated reason. Today's tree: tree-sitter grammar
  modules + `go-tree-sitter-bare` (cgo), and `modernc.org/sqlite` (pure Go).
- Exit codes are contract: `0` ok, `1` internal, `2` usage, `3` not found,
  `4` ambiguous. Never reuse them.
- `gofmt` scope is `./cmd ./internal` on purpose: the local tool cache under
  `.cache/` must stay out of the formatter.
- Fixture expectations (`testdata/fixtures/*/expected.json`) and the eval gate
  are part of any resolution change. Never loosen a gate to make a PR pass.

## Landmines

- **cgo is required.** There is no maintained pure-Go tree-sitter (decision 15,
  amended in M0). Builds need a C toolchain; cross-compiles need a target one.
- **Grammar versions are pinned per module and they differ** (typescript
  `v1.9.4`, tsx and javascript `v1.9.2`, python `v1.9.10`, rust `v1.9.13`).
  Bumping one with
  `@latest` can drag the binding API and break the others; change them
  deliberately and rebuild.
- **The parser rejects some valid TypeScript**, e.g.
  `call<typeof import("x")>()`. Those files are stored with `parse_ok=0` and
  partial symbols. Report the count; never hide it.
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
- **Python inheritance is not walked for `self`.** `self.m()` resolves against
  the enclosing class only; a method inherited from a base stays unresolved
  (bases are still `EXTENDS` edges). Untyped locals stay unresolved too.
- **Python re-exports are followed through the imports table.** `py.import.reexport`
  covers `__init__.py` and ordinary modules such as `requests.compat`; a chain
  ending at the standard library is external, not pending.
- **Node ids are stable across re-index.** `ReplaceNodes` upserts by
  `(qname, arity)` so a file edit does not orphan incoming edges; keep that
  property when touching the indexer.
- **Pending retries only re-link identifiers.** `pending.form` exists so an
  untyped `obj.get()` is never retried as a repo-wide name guess.
- **Rust module resolution is convention-based.** Crate roots come from
  Cargo packages (`src/lib.rs` or `src/main.rs`); `src/foo.rs` is `crate::foo`.
  `#[path]`, inline `mod` blocks as import targets, and workspace cross-crate
  imports are not resolved. Do not add a "best guess" fallback for them.
- **Rust macro bodies are blind.** `macro_rules!` token trees and macro
  invocations create no symbols or edges. Typed receiver methods with no local
  definition are classified external (dependency or macro-generated trait impl)
  rather than guessed; keep that trade-off visible in docs.
- **Rust generic bounds are not modelled.** `T: Trait` calls stay unresolved,
  and `IMPLEMENTS` is emitted only when the implementing type is defined in the
  same file. These are fixture-gated gaps, not bugs to paper over.
- **Rust ambiguity is real.** Inherent and trait impls share `Type.method`;
  2–5 candidates become 0.40 edges, and `status` exposes `ambiguous` and
  `ambiguity_rate`. Never collapse them by picking one.
- **Rust enum variants are a heuristic.** A CamelCase associated call on a known
  enum becomes REFERENCES at 0.90; an associated constant with the same shape
  can be misclassified.
- **Module cycles** are computed on cross-module edges at ≥0.5; one noisy edge
  can merge modules into one SCC. Treat cycles as hints.
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
./bin/repath status --json                   # on this repo, sanity
./bin/repath impact <symbol> --json | head   # smoke on a fixture copy
go test ./internal/eval -v                   # logs precision/recall
```

PR checklists that matter: a test (or a stated reason), no new dependency
without justification, `PLAN.md` updated when behaviour or a decision changes,
and the "not verified" line filled in truthfully.
