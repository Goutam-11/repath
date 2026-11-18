# Runtime trace evidence

`repath trace` reads a CPU profile that an external runtime already produced and
reports the caller→callee calls it **observed**, classified against the static
graph. It is a read-only evidence bridge: nothing is written, no edge is added,
and default answers are unchanged. It answers a question the static graph
cannot: which calls actually happened, including dynamic dispatch, duck typing,
dependency injection, and framework magic.

## Command

```sh
node --cpu-prof --cpu-prof-interval=100 --cpu-prof-dir out my-program.js
repath trace --file out/CPU.*.cpuprofile [--root DIR] [--limit 50] [--json]
```

Exit codes: `0` ok, `2` usage or malformed profile, `3` profile not found; `5`
stays check-only. There is no MCP tool or HTTP answer for this command: the six
answers are unchanged.

## Input

A V8/Chrome CPU profile (`--cpu-prof`) JSON document: `nodes[]` with `callFrame
{functionName, url, lineNumber, hitCount}`, `children`, and an optional
`parent`. `file://` URLs are accepted and mapped to repo paths by exact path or
unique suffix; `node:` and other schemes are counted as `external-file`. A
profile with no nodes is a valid zero-fact report.

## Mapping and false-positive policy

A frame becomes an observed call only when **all** of these hold:

1. the URL maps to a file in the index;
2. `EnclosingNode(file, lineNumber+1)` finds a symbol;
3. that symbol's name equals the frame's `functionName`.

Otherwise the frame is skipped and counted with a reason (`no-function`,
`external-file`, `unmapped-file`, `no-line`, `no-enclosing`, `name-mismatch`).
A same-named function in another file is never a fallback, and a profile whose
lines have shifted since it was recorded is refused, not attributed. That is
the staleness check: the profile carries no commit, so file + line + name is
the only honest identity; regenerate it after changing the code.

## Report

- `observed_edges`, `confirmed` (a static edge exists at ≥0.5 from caller to
  callee), and `observed_only` (the static graph cannot prove it).
- Each edge carries `rule: "dyn.trace"`, `confidence: 0.60`, `kind: "CALLS"`, a
  sample `count`, and `static: true|false`. The rule id is visible only in this
  report; it is never persisted.
- `--limit` caps the listed edges (default 50); the totals cover all edges and
  `truncated` is set.
- Counts come from the profile's `hitCount`; they are sampling evidence, not
  invocation counts.

## What a trace can and cannot prove

- **Can:** that a caller→callee pair was entered in this run, including targets
  the resolver could not name; that a static edge is actually exercised.
- **Cannot:** that other targets are impossible (sampling, one run);
  exhaustiveness or coverage; anything about paths not run; Python/Rust behavior
  until those profile formats are supported; staleness across commits.
- Privacy: the profile is read locally and never written or uploaded.

## Fixture evidence

`testdata/fixtures/trace/cpu` is a small CommonJS program whose `dispatch`
routes through an object map (`handlers[kind](name)`). The committed profile was
generated with Node 24.18.0:

```sh
node --cpu-prof --cpu-prof-interval=100 \
  --cpu-prof-dir=.cache/prof testdata/fixtures/trace/cpu/src/dynamic.js
```

The `file://` URLs were rewritten to repo-relative paths for the committed
fixture only; the importer also accepts absolute `file://` URLs. The gates
(`internal/trace`, `internal/cli`) pin:

- **3 mapped frames / 2 observed edges**: `run → dispatch` is confirmed by the
  static `ts.local` edge, and `dispatch → greet` is observed only — the computed
  dispatch the static graph cannot see;
- `other.js` has a same-named `greet`; no edge is attached to it;
- shifting the source by two lines and re-indexing makes the old profile stale:
  no observed-only edge is produced and the skipped frames are counted;
- malformed/missing profiles, `--limit`, and determinism.

No producer is required by CI: the profile is committed and the source is
parsed by tree-sitter.

## Limits

- V8/Chrome CPU profile only. py-spy/speedscope, OTel spans, and Chrome tracing
  event streams are not parsed yet.
- Sampled profiles are approximate; inlining may attribute samples to a caller,
  and recursion (caller == callee) is not reported.
- Nothing is persisted: `impact`/`path`/`map` do not see observed edges, and a
  `--trace` query overlay is deferred. The report is the deliverable.
