# Runtime trace fixture

Two source files and a committed V8 CPU profile. `src/dynamic.js` dispatches
through an object map, so tree-sitter cannot resolve `dispatch → greet`;
`src/other.js` has a same-named `greet` to prove the frame is not attached by
name. The static graph is `run → dispatch` (`ts.local`) plus one unresolved
`toUpperCase` member call.

The profile was generated on 2026-11-18 with Node **v24.18.0**:

```sh
node --cpu-prof --cpu-prof-interval=100 \
  --cpu-prof-dir=.cache/prof testdata/fixtures/trace/cpu/src/dynamic.js
```

Only the `file://` URLs were rewritten to repo-relative paths (`src/dynamic.js`)
for the committed copy; the importer also accepts absolute `file://` URLs and
skips `node:` internals.

Expected report: **3 mapped frames, 2 observed edges** — `run → dispatch`
confirmed by the static graph, `dispatch → greet` observed only — and no edge to
`other.js`. Stale-frame behavior is tested by shifting `src/dynamic.js` in test
code, not by committing a second profile.
