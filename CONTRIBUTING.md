# Contributing to repath

Thanks for taking the time. repath is a small, opinionated tool: one binary, one
SQLite index, a documented contract. The fastest way to get a change merged is
to keep it small, measurable, and honest about what it does not prove.

This file is the short version; **[`docs/development.md`](docs/development.md)**
is the full workflow (trunk-based flow, commit rules, PR checklist, worktree
setup for parallel work).

## Set up

```sh
git clone https://github.com/Goutam-11/repath
cd repath
make hooks      # commit-identity guard, once per clone
make check      # gofmt + vet + tests + hook tests — the PR gate (~50 s)
```

Builds need Go 1.27+ **and a C toolchain** (cgo/tree-sitter; there is no pure-Go
build). `make perf` runs the opt-in synthetic budgets and is expected when you
touch indexing, resolution, or query paths.

## Flow

1. Open or pick an issue. For a bug, include the pinned corpus/commit, the exact
   command, and `repath status --json` — the graph structure is the evidence.
2. Branch from `main` as `<type>/<short-scope>` (`fix/impact-cycle-guard`,
   `feat/ts-import-resolution`, `docs/scope`).
3. Write conventional commits, and one logical change per PR.
4. Open a PR from the template: why, what changed, one diagram, a validation
   table with real numbers, and an honest "not verified" line.
5. CI green → squash merge → the branch is deleted.

`main` is protected: no direct pushes, no force pushes.

## The rules that actually matter

- **The contract is [`PLAN.md`](PLAN.md).** A change that contradicts a locked
  decision in §3 needs an amendment in the same PR and explicit approval.
- **Never guess a target to make a number look better.** A new resolver rule
  needs a fixture case and a row in the confidence ladder
  ([`docs/resolution.md`](docs/resolution.md)).
- **Fixture gates are never loosened** to make a PR pass
  (`testdata/fixtures/*/expected.json`).
- **No new dependency without a stated reason.** Today's tree: tree-sitter
  grammar modules + `go-tree-sitter-bare`, `modernc.org/sqlite`, and
  `klauspost/compress/zstd` (opt-in artifacts).
- **Exit codes are contract:** `0` ok, `1` internal, `2` usage, `3` not found,
  `4` ambiguous; `5` belongs to `check` only.
- **Keep it local.** No daemon, no LLM, no config file, no network calls in the
  index/answer path; `serve` stays read-only.

## Reporting a bug

Open an issue with:

- the command and its full output (or `--json`);
- `repath --version` and `repath status --json`;
- the repository (or a minimal fixture) and commit you indexed;
- what you expected vs what happened.

Security-sensitive reports: please describe the impact without a public
proof-of-concept first, and we will arrange a private channel.

## Proposing a feature

Say which structural question it answers, why the existing commands cannot, and
what it would write into the graph (ideally: nothing). Features that add a node
kind, an edge kind, a config file, or a seventh MCP tool need a plan change, so
lead with the plan amendment.

## Docs

- [`docs/resolution.md`](docs/resolution.md) — confidence ladder and gaps
- [`docs/benchmarks.md`](docs/benchmarks.md) — every measured number and caveat
- [`docs/install.md`](docs/install.md) — install, cross-compile, agent skill
- [`docs/releases/`](docs/releases/) — release notes

By contributing you agree your work is licensed under the repository's
[MIT license](LICENSE).
