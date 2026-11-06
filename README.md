# repath

Structural answers about a codebase — *what breaks if I change this*, *how does
A reach B*, *where is the important code* — from one deterministic graph, for
humans and for coding agents.

**Status: pre-alpha.** M0 works: `repath index` walks a repo, parses TypeScript
and JavaScript definitions into SQLite, and `status` reports what it found.
Relations (calls, imports, impact) are not built yet; [`PLAN.md`](PLAN.md) is the
specification.

## Build

Requires Go 1.27+.

```sh
make build
./bin/repath status --json        # builds .repath/index.db on first run
./bin/repath index --full         # rebuild from scratch
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
