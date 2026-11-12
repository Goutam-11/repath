# repath

Structural answers about a codebase — *what breaks if I change this*, *how does
A reach B*, *where is the important code* — from one deterministic graph, for
humans and for coding agents.

**Status: pre-alpha (M5 in progress).** TypeScript/JS, Python, and Rust
definitions and relations are indexed, and `impact`, `path`, `find`, `callers`,
and `map` answer structural questions from SQLite. `repath mcp` serves the same
six answers over stdio JSON-RPC, and [`skills/repath/SKILL.md`](skills/repath/SKILL.md)
teaches shell-capable agents the CLI. `index --artifact` exports a commit-keyed
snapshot and `pull` seeds it locally; worktree seeding is next.
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
./bin/repath index --artifact out/     # export a shareable snapshot
./bin/repath pull out/                 # seed the local index from a snapshot
```

An agent with a shell should prefer the CLI; [`skills/repath/SKILL.md`](skills/repath/SKILL.md)
is the ~300-token cheat sheet. Shell-less clients use the MCP adapter; see
[`docs/mcp.md`](docs/mcp.md) for the transport, six tools, and envelope.
Sharing, the manifest, and the schema gate are in
[`docs/sharing.md`](docs/sharing.md).

Add `.repath/` to your own `.gitignore`. Languages today:
TypeScript/JavaScript (`.ts`, `.tsx`, `.js`, `.jsx`, `.mjs`, `.cjs`), Python
(`.py`), and Rust (`.rs`).

## Shape

```
cmd/repath/     entrypoint
internal/       cli, index, lang, resolve, graph, store, artifact, mcp, output
docs/           development.md (workflow), design notes
PLAN.md         the specification
```

## Development

See [`docs/development.md`](docs/development.md): trunk-based flow, conventional
commits, PR template, `make check` gate.

## License

MIT — see [`LICENSE`](LICENSE).
