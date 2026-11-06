# repath

Structural answers about a codebase — *what breaks if I change this*, *how does
A reach B*, *where is the important code* — from one deterministic graph, for
humans and for coding agents.

**Status: pre-alpha.** TypeScript/JS definitions and relations are indexed:
`repath find` and `repath callers` answer structural questions from SQLite.
`impact`, `path`, and `map` are not built yet; [`PLAN.md`](PLAN.md) is the
specification.

## Build

Requires Go 1.27+.

```sh
make build
./bin/repath status --json             # builds .repath/index.db on first run
./bin/repath callers <symbol>          # who calls this
./bin/repath find <symbol>             # symbol card: callers, callees
./bin/repath callers <symbol> --min-confidence 0.9
```

Add `.repath/` to your own `.gitignore`.

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
