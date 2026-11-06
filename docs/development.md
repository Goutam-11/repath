# Development

Short version: **main is always buildable, every change lands through a PR, and
the PR body is the design record.**

## Flow

Trunk-based ([GitHub Flow](https://docs.github.com/en/get-started/using-github/github-flow)):

1. branch from `main` — `<type>/<short-scope>`, e.g. `feat/ts-import-resolution`,
   `fix/impact-cycle-guard`, `chore/ci`, `docs/sharing`
2. small commits, conventional style (below)
3. open a PR early, keep it under ~400 changed lines when possible
4. CI green → squash merge → delete the branch

`main` is protected: no direct pushes, PR required, `ci / check` must pass.

## Commits

[Conventional Commits](https://www.conventionalcommits.org/): `feat`, `fix`,
`chore`, `docs`, `test`, `refactor`, `perf`, `ci`.

```
feat(ts): resolve aliased imports through tsconfig paths

Imports written as `@/lib/x` resolved to a bare name and produced 0.6-confidence
edges. The resolver now reads compilerOptions.paths once per repo.
```

Subject in the imperative, ≤ 72 characters, no trailing period. The body explains
*why*; the diff already shows *what*. The PR title becomes the squash commit, so
it follows the same rules.

## Pull requests

- one logical change per PR; a refactor plus a feature is two PRs
- body follows [`.github/pull_request_template.md`](../.github/pull_request_template.md) —
  delete sections that do not apply rather than filling them with "n/a"
- include the diagram that explains the change (flow, sequence, ER, state)
- name the evidence: test names, counts, command output; say what was **not**
  verified
- CLI output changes need a before/after block

## Definition of done

- `make check` passes locally (gofmt, vet, tests)
- the behaviour is covered by a test, or the PR says why it is not
- exit codes (`0 ok, 1 internal, 2 usage, 3 not found, 4 ambiguous`) are
  unchanged, or the change is documented
- `PLAN.md` is updated when a **locked decision** (§3) changes — the plan is the
  contract, so change it deliberately and mention it in the PR

## Commands

```sh
make build   # bin/repath
make check   # gofmt + vet + tests
make test    # tests only
make fmt     # gofmt -w
```

Local Go lives at `/usr/local/go/bin` if `go` is not on your PATH.

## Releasing

Tag `v0.x.y` once M1 lands; binaries via goreleaser stay deferred (PLAN.md §16).
