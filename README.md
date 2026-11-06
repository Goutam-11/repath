# repath

Structural answers about a codebase — *what breaks if I change this*, *how does
A reach B*, *where is the important code* — from one deterministic graph, for
humans and for coding agents.

**Status: pre-alpha.** No graph engine exists yet; [`PLAN.md`](PLAN.md) is the
specification (22 locked decisions, schema, CLI and MCP contracts, milestones).

## Build

Requires Go 1.27+.

```sh
make build
./bin/repath version
./bin/repath status --json
```

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
