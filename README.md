# repath

Structural answers about a codebase — *what breaks if I change this*, *how does
A reach B*, *where is the important code* — from one deterministic graph, for
humans and for coding agents.

**Status: pre-alpha (M5 + v1.1 + the v1.2 wave).** TypeScript/JS, Python, and Rust
definitions and relations are indexed, and `impact`, `path`, `find`, `callers`,
and `map` answer structural questions from SQLite. `repath mcp` serves the same
six answers over stdio JSON-RPC in both the legacy and 2026-07-28 eras, and [`skills/repath/SKILL.md`](skills/repath/SKILL.md)
teaches shell-capable agents the CLI. `index --artifact` exports a commit-keyed snapshot (with opt-in `--compress`), `pull` seeds it locally, parallel git worktrees warm from a shared
immutable cache, `repath check` adds graph-native CI diagnostics, and
`repath adapter --scip` merges TypeScript, Python, or Rust compiler facts into unresolved references without
overwriting tree-sitter edges, and `repath flows` walks forward from detected
entrypoints (Express/Fastify/Next.js, FastAPI/Flask, pytest, `main`, `#[test]`),
and `repath tests` maps a change (or a git `--diff` set) to the test files that
structurally reach it.
`repath check --sarif` now attaches external compiler/linter diagnostics (SARIF
2.1.0) to the enclosing symbols without running those tools, and `repath serve`
answers the six structural questions over read-only HTTP for dashboards and
shell-less clients.
[`PLAN.md`](PLAN.md) is the specification.

## Install

Prebuilt Linux amd64/arm64 archives and SHA-256 checksums are attached to
[GitHub releases](https://github.com/Goutam-11/repath/releases); build-from-source,
cross-compile, and runtime notes are in [`docs/install.md`](docs/install.md).

```sh
# release binary
tar -xzf repath_X.Y.Z_linux_amd64.tar.gz && sudo install -m 0755 repath /usr/local/bin/
repath --version

# or from source
make build
./bin/repath --version
```

## Build

Requires Go 1.27+ and a C toolchain: tree-sitter runs through cgo. Python
(`.py`) resolves absolute and relative imports, package re-exports, class bases,
decorators, and `self`/`classmethod`/typed-receiver calls. Rust (`.rs`) resolves
Cargo module trees and local workspace members, uniform paths, use trees and
`pub use` re-exports, `#[path]` and inline `mod` import targets, generic bounds
(`T: Trait`) to local trait methods, trait default methods, impl/trait methods,
associated functions, and typed receivers (including same-file return and
associated-type bindings); macro bodies are a documented blind spot. `status`
reports `ambiguous` edges and their rate.

```sh
make build
./bin/repath status --json             # builds .repath/index.db on first run
./bin/repath impact <symbol>           # transitive blast radius
./bin/repath path <from> <to>          # how one symbol reaches another
./bin/repath map                       # ranked overview: modules, cycles, hubs
./bin/repath callers <symbol>          # who calls this
./bin/repath find <symbol>             # symbol card: callers, callees
./bin/repath mcp                       # stdio JSON-RPC for MCP clients
./bin/repath serve --addr 127.0.0.1:0  # read-only HTTP answers
./bin/repath index --artifact out/     # export a snapshot (--compress for zstd)
./bin/repath pull out/                 # seed the local index from a snapshot
./bin/repath check --fail-on warning   # cycles, dead code, boundaries, hotspots
./bin/repath check --sarif out.ts.sarif # attach SARIF diagnostics to symbols
./bin/repath adapter --scip index.scip # merge TS/Python SCIP facts into pending refs
./bin/repath flows <entrypoint>        # forward execution paths
./bin/repath tests <symbol|file:line>  # affected test files (structural)
./bin/repath tests --diff origin/main  # affected test files for a change set
```

An agent with a shell should prefer the CLI; [`skills/repath/SKILL.md`](skills/repath/SKILL.md)
is the ~300-token cheat sheet. Shell-less clients use the MCP adapter; see
[`docs/mcp.md`](docs/mcp.md) for the transport, six tools, and envelope.
Sharing, the manifest, and the schema gate are in
[`docs/sharing.md`](docs/sharing.md); worktree seeding, locks, and LRU are in
[`docs/worktrees.md`](docs/worktrees.md); the five checks, severities, and the
check-only exit code `5` are in [`docs/check.md`](docs/check.md); that page also
covers opt-in SARIF ingestion, the `diagnostic` kind, and the clean `2`/`3`
SARIF error paths. The MCP
adapter's dual-era negotiation and client verification are in
[`docs/mcp.md`](docs/mcp.md). The optional SCIP adapter (TypeScript and Python),
its merge rules, and the measured unresolved drops are in
[`docs/adapters.md`](docs/adapters.md), and entrypoint detection/flow limits are
in [`docs/flows.md`](docs/flows.md), and test-impact provenance, diff mode, and
limits are in [`docs/tests.md`](docs/tests.md); real-repo measurements and misses are in
[`docs/benchmarks.md`](docs/benchmarks.md), and the read-only HTTP surface is in
[`docs/serve.md`](docs/serve.md).

The 50k artifact compresses from **19.2 MiB to 4.8 MiB** (25.2%) with
`--compress`; the 10k tree goes from **3.8 MiB to 992 KiB** (25.4%). The
measured 10k-file synthetic tree (reproducible with `make perf-10k`): cold
index **1.74 s**, 10-file incremental **111 ms**, `impact` p95 **11.0 ms**, a
second worktree warm seed **12.9 ms**, and a cold build **21.8 s** — README
quickstart to first answer **~24 s**, inside the 60 s budget. The 50k tree
(`make perf-50k`): cold index **9.34 s**, peak RSS **290 MiB**, `impact` p95
**54.2 ms**, `map` p95 **148 ms** — every PLAN §10 budget met. The full table
is in `PLAN.md` §12.

A pinned real-repo run (`make bench-real`, Kibana `x-pack/solutions` at
`0551196`, 38,548 TS files) is in [`docs/benchmarks.md`](docs/benchmarks.md):
cold **19.2 s**, incremental **582 ms**, `map` p95 **223 ms**, `impact`/
`callers`/`path` p95 **60.8 / 38.3 / 71.6 ms**, peak RSS **920 MiB** cold, and
artifact **65.6 MiB → 17.2 MiB** (26.2%). Every measured target is met; the
synthetic budgets above remain the gate.

A Python corpus (`psf/requests`, 37 files) and a Rust corpus (`rayon`, 191
files) run with `make bench-real-langs`: the `scip-python` adapter takes
requests unresolved **543 → 523**; on rayon the static ladder leaves **1,394**
unresolved after #55 (2,048 before it) and the pinned `rust-analyzer` file takes
that to **1,156**. The full tables, producer versions, and the warm/cold caveat
are in [`docs/benchmarks.md`](docs/benchmarks.md).

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
