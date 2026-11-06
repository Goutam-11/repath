# Why

<!-- The problem, in one or two sentences, in behaviour-facing terms. -->

# What changed

-

<!-- Call out anything intentionally left unchanged, and name the commits. -->

# Visuals

<!-- Keep whichever diagram explains this change; delete the rest. Replace the placeholders. -->

```mermaid
flowchart LR
  A[agent or user] --> B[repath CLI]
  B --> C{index exists?}
  C -->|no| D[build .repath/index.db]
  C -->|yes| E[query graph]
  D --> E
  E --> F[text report or JSON answer]
```

```mermaid
sequenceDiagram
  participant U as Caller
  participant C as CLI
  participant S as Store (SQLite)
  participant G as Graph
  U->>C: repath impact cart.ts:42
  C->>S: load nodes and edges
  S-->>C: rows
  C->>G: traverse callers, cap depth
  G-->>C: ranked dependents
  C-->>U: text report / JSON envelope
```

<!-- Schema or migration change? Add an ER diagram. Lifecycle change? Add a stateDiagram-v2. Layout change? A before/after tree works too. -->

# Validation and proof

| Check | Command | Result |
| --- | --- | --- |
| format | `gofmt -l .` | |
| vet | `go vet ./...` | |
| tests | `go test ./...` | |
| build | `make build` | |
| manual | `./bin/repath status --json` | |

<!-- Name the evidence: test names, counts, command output. Say plainly what was NOT verified. -->

# CLI proof

<!-- Terminal output showing the visible result. Delete this section when the change has no observable surface. -->

| Before | After |
| --- | --- |
|  |  |

# Risk and rollback

-

---

- [ ] `make check` passes
- [ ] the new behaviour is covered by a test, or the PR says why not
- [ ] the new check fails without this change
- [ ] no new dependency, or the reason is stated above
- [ ] README or `PLAN.md` updated when behaviour or a locked decision changed
- [ ] exit codes unchanged, or documented in the PR
