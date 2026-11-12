# repath

Structural answers about a codebase — *what breaks if I change this*, *how does
A reach B*, *where is the important code* — from one deterministic graph, for
humans and for coding agents.

**Status: pre-alpha (M4).** TypeScript/JS, Python, and Rust definitions and
relations are indexed, and `impact`, `path`, `find`, `callers`, and `map` answer
structural questions from SQLite. MCP and sharing are not built yet;
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
```

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
