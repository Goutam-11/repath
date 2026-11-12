# repath

Structural answers about a codebase — *what breaks if I change this*, *how does
A reach B*, *where is the important code* — from one deterministic graph, for
humans and for coding agents.

**Status: pre-alpha (M5 + v1.1 `check`).** TypeScript/JS, Python, and Rust
definitions and relations are indexed, and `impact`, `path`, `find`, `callers`,
and `map` answer structural questions from SQLite. `repath mcp` serves the same
six answers over stdio JSON-RPC in both the legacy and 2026-07-28 eras, and [`skills/repath/SKILL.md`](skills/repath/SKILL.md)
teaches shell-capable agents the CLI. `index --artifact` exports a commit-keyed snapshot (with opt-in `--compress`), `pull` seeds it locally, parallel git worktrees warm from a shared
immutable cache, `repath check` adds graph-native CI diagnostics, and
`repath adapter --scip` merges compiler facts into unresolved references without
overwriting tree-sitter edges, and `repath flows` walks forward from detected
entrypoints (Express/Fastify/Next.js, FastAPI/Flask, pytest, `main`, `#[test]`),
and `repath tests` maps a change to the test files that structurally reach it.
[`PLAN.md`](PLAN.md) is the specification.

## Build

Requires Go 1.27+ and a C toolchain: tree-sitter runs through cgo. Python
(`.py`) resolves absolute and relative imports, package re-exports, class bases,
decorators, and `self`/`classmethod`/typed-receiver calls. Rust (`.rs`) resolves
Cargo module trees, use trees and `pub use` re-exports, impl/trait methods,
associated functions, and typed receivers; macro bodies are a documented blind
spot. `status` reports `ambiguous` edges and their rate.

```sh
make build
./bin/repath status --json             # builds .repath/index.db on first run
./bin/repath impact <symbol>           # transitive blast radius
./bin/repath path <from> <to>          # how one symbol reaches another
./bin/repath map                       # ranked overview: modules, cycles, hubs
./bin/repath callers <symbol>          # who calls this
./bin/repath find <symbol>             # symbol card: callers, callees
./bin/repath mcp                       # stdio JSON-RPC for MCP clients
./bin/repath index --artifact out/     # export a snapshot (--compress for zstd)
./bin/repath pull out/                 # seed the local index from a snapshot
./bin/repath check --fail-on warning   # cycles, dead code, boundaries, hotspots
./bin/repath adapter --scip index.scip # merge SCIP facts into pending refs
./bin/repath flows <entrypoint>        # forward execution paths
./bin/repath tests <symbol|file:line>  # affected test files (structural)
```

An agent with a shell should prefer the CLI; [`skills/repath/SKILL.md`](skills/repath/SKILL.md)
is the ~300-token cheat sheet. Shell-less clients use the MCP adapter; see
[`docs/mcp.md`](docs/mcp.md) for the transport, six tools, and envelope.
Sharing, the manifest, and the schema gate are in
[`docs/sharing.md`](docs/sharing.md); worktree seeding, locks, and LRU are in
[`docs/worktrees.md`](docs/worktrees.md); the five checks, severities, and the
check-only exit code `5` are in [`docs/check.md`](docs/check.md). The MCP
adapter's dual-era negotiation and client verification are in
[`docs/mcp.md`](docs/mcp.md). The optional SCIP adapter, its merge rules,
and the measured unresolved drop are in
[`docs/adapters.md`](docs/adapters.md), and entrypoint detection/flow limits are
in [`docs/flows.md`](docs/flows.md), and test-impact provenance/limits are in
[`docs/tests.md`](docs/tests.md).

The 50k artifact compresses from **19.2 MiB to 4.8 MiB** (25.2%) with
`--compress`; the 10k tree goes from **3.8 MiB to 991 KiB** (25.3%). The
measured 10k-file synthetic tree (reproducible with `make perf-10k`): cold
index **5.0 s**, 10-file incremental **237 ms**, `impact` p95 **24.5 ms**, a
second worktree warm seed **12.9 ms**, and a cold build **21.8 s** — README
quickstart to first answer **~27 s**, inside the 60 s budget. The 50k tree
(`make perf-50k`): cold index **14.3 s**, peak RSS **298 MiB**, `impact` p95
**64.0 ms**, `map` p95 **228 ms**, `check` **490 ms** — every PLAN §10 budget
met. The full table is in `PLAN.md` §12.

Add `.repath/` to your own `.gitignore`. Languages today:
TypeScript/JavaScript (`.ts`, `.tsx`, `.js`, `.jsx`, `.mjs`, `.cjs`), Python
(`.py`), and Rust (`.rs`).

## Shape

```
cmd/repath/     entrypoint
internal/       cli, index, lang, resolve, graph, store, check, adapter, artifact, mcp, output
docs/           development.md (workflow), design notes
PLAN.md         the specification
```

## Development

See [`docs/development.md`](docs/development.md): trunk-based flow, conventional
commits, PR template, `make check` gate.

## License

MIT — see [`LICENSE`](LICENSE).
