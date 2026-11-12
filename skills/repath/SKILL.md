---
name: repath
description: Answer structural questions about a codebase — what breaks, how A reaches B, where the important code is — with one deterministic, confidence-tagged graph. Use for impact analysis, callers, connection paths, and repo orientation.
---

# repath

`repath` is a single binary over SQLite. Use the **CLI** when you have a shell;
the MCP server is for clients without one. Never chain `grep → read → grep` for
a structural question: one call returns the traversed answer.

The index builds itself on first use at `.repath/index.db`. `status` refreshes
it; queries do not.

```sh
repath impact <symbol|file:line> --json   # reverse closure: what breaks
repath path <from> <to> --json            # shortest chain + alternates
repath callers <symbol> --json            # direct reverse edges
repath find <symbol> --json               # card: location, caller/callee counts
repath map --max-tokens 1200 --json       # modules, cycles, ranked hubs
repath status --json                      # counts, unresolved, ambiguity, parse failures
repath check --fail-on warning --json     # cycles, dead code, boundaries, hotspots
repath adapter --scip index.scip --json    # optional compiler facts for unresolved refs
```

`--min-confidence` (default 0.5) is the precision/completeness dial: 0.9 keeps
import/`this`/local edges, 0.4 adds ambiguous candidates with their rule id.
`--depth` defaults to 8 for impact/path.

Read the JSON: `dependents[]` carries `depth`, `confidence`, `rule`, `call_line`;
`path` returns `hops[]` with per-edge `rule`; `impact` and `map` report
`truncated`. Exit codes: `0` ok, `1` internal, `2` usage, `3` not found,
`4` ambiguous (candidates printed or in `error.candidates`); `check` alone adds
`5` for findings at or above `--fail-on`. Resolve `4` with a qualified name,
never by picking a candidate yourself.

Without a shell, run `repath mcp`: six tools with the same answers —
`find_symbol`, `impact`, `path`, `callers`, `repo_map`, `status`.
