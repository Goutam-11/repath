# Real-repository benchmark

Issue #31 asked for the 50k/zstd claims to be validated on a real repository,
not only the synthetic one-line-function tree. This page records the pinned
repositories, the reproducible commands, and the honest numbers. Issue #43
profiled and closed the three Kibana budgets that #31 missed; issue #48 then
extended the same matrix to Python and Rust and split the Rust adapter gain
into production and test code. It is **evidence, not a gate**: `make perf`
remains the opt-in synthetic budget run, and neither target is part of CI.

## Pinned shape

| | |
| --- | --- |
| Repository | [`elastic/kibana`](https://github.com/elastic/kibana) |
| Commit | `05511966780217f7357b98253299f369dc40199b` (2026-11-12) |
| Subtree | `x-pack/solutions` |
| Files present | 42,491 |
| TypeScript/JavaScript files | 38,562 |
| Indexed by repath | **38,548** files, 87,478 nodes (3,943 non-language/oversized files skipped) |
| Parse failures | 158 (0.41%) |
| Edges | 61,894 (57,912 `CALLS`, 1,385 `REFERENCES`, 196 `EXTENDS`, 59 `IMPLEMENTS`) |
| Unresolved | 161,425 (72.0%) |
| Ambiguous edges | 5,242 (8.8%) |

The full Kibana tree has **114,967** TypeScript/JavaScript files. That is beyond
the issue's 100k+ out-of-scope line, so the measurement pins the coherent
`x-pack/solutions` subtree (the commercial security/observability/search code)
rather than the whole monorepo. The repo is a shallow, sparse clone:

```sh
git clone --depth 1 --single-branch https://github.com/elastic/kibana /tmp/kibana
cd /tmp/kibana
git sparse-checkout init --cone
git sparse-checkout set x-pack/solutions
git rev-parse HEAD   # 05511966780217f7357b98253299f369dc40199b
```

## Command

```sh
cd /path/to/repath
make bench-real \
  REPATH_BENCH_ROOT=/tmp/kibana \
  REPATH_BENCH_COMMIT=05511966780217f7357b98253299f369dc40199b
```

`TestBenchReal` refuses a dirty clone, removes `<root>/.repath/` for the cold
run, appends a comment to ten discovered files for the incremental run and
restores them from memory, then measures queries, `check`, and artifacts. It
logs `BUDGET MISS` instead of failing, so a real-repo miss cannot turn into a
CI gate. The `REPATH_BENCH_COMMIT` check keeps the numbers pinned.

## Results on this machine

Linux, 12 logical CPUs, 7.2 GiB RAM (≈1.7 GiB free during the run), NVMe,
Go 1.27, repath after issue #43. Peak RSS is `/proc/self/status` `VmHWM`.
Three quiet-machine runs on 2026-11-13; the range is in parentheses.

| Metric | Result | PLAN §10 target | Status |
| --- | --- | --- | --- |
| Cold index (38,548 files) | **19.2 s** (19.02–19.30) | < 3 min | pass |
| Peak RSS (cold) | **920 MiB** (870–935) | < 1 GiB | pass |
| 10-file incremental | **582 ms** (576–614) | < 1 s | pass |
| `impact` p50 / p95 | **55.9 / 60.8 ms** | < 150 ms p95 | pass |
| `callers` p50 / p95 | **36.7 / 38.3 ms** | < 150 ms p95 | pass |
| `path` p50 / p95 | **49.8 / 71.6 ms** | < 250 ms p95 | pass |
| `map --max-tokens 2000` p50 / p95 | **215 / 223 ms** | < 300 ms p95 | pass |
| `check` | **725 ms** (200 findings, capped) | no target | — |
| Artifact uncompressed | **65.6 MiB** | no target | — |
| Artifact export / pull | **317 / 32 ms** | no target | — |
| Artifact zstd | **17.2 MiB** (26.2%) | no target | — |
| zstd export / pull | **531 / 88 ms** | no target | — |
| Peak RSS (all measurements) | **1.00–1.02 GiB** | no real-tree target | — |

Sample sizes: 20 deterministic `impact`/`callers` samples and 17 `path`
samples over symbols with both callers and callees; 5 `map` samples. The first
query of each kind is included, so these are warm-index, cold-process numbers
(no query cache is warmed deliberately).

**Run-to-run variance is real.** The host shared the machine with other work.
Under concurrent load, identical runs produced incremental **0.96–1.32 s** and
`impact` p95 **192–228 ms**; the three rows above are quiet-machine runs. The
budgets are met at the median; an isolated noisy run can still miss by a few
percent. `make bench-real` stays opt-in and is not a gate.

## Before and after (#43)

| Metric | #31 (docs) | Reproduced baseline on this machine | After #43 | Target |
| --- | --- | --- | --- | --- |
| Cold index | 4 min 29.1 s | 3 min 49 s – 4 min 41 s | **19.2 s** | < 3 min |
| 10-file incremental | 4.81 s | 2.35 – 3.04 s | **0.58 s** | < 1 s |
| `map` p95 | 455.6 ms | 308 – 385 ms | **223 ms** | < 300 ms |
| `impact` / `callers` / `path` p95 | 118.9 / 73.0 / 130.1 ms | 60–77 / 38–58 / 69–82 ms | **60.8 / 38.3 / 71.6 ms** | pass |
| Edges | 61,894 | 61,894 | **61,894** | unchanged |
| Unresolved | 158,806 (72.0%) | 158,806 (72.0%) | **158,806 (72.0%)** | unchanged |
| Artifact | 65.6 → 17.2 MiB (26.3%) | same | same | unchanged |

The graph totals are unchanged; the difference is *when* they appear. Before
#43 a cold index produced 59,552 edges and left 2,619 pending identifiers for
the next run's global retry; the first incremental then added 2,342
`ts.global.unique` edges. A cold run now completes that provable retry itself,
so cold and warmed indexes agree and the first incremental no longer changes the
edge set.

## What was slow, and what changed

**Cold: SQLite, not parsing.** A CPU profile with `-cpuprofile` attributed
**202.6 s of the 229 s** run to `store.ReplacePending`. `pending` has an index on
`name` but none on `file_id`, so the per-file `DELETE ... WHERE file_id = ?`
scanned the whole table once for each of 38,548 files. Parsing was only 42.6 s.
Pending replacement now deletes every changed file before inserting, edges and
pending deletes are multi-row statements, and node ids are read per changed file
only where resolution needs them. The WAL is checkpointed at the end of a run.
Cold: **229 → 19.2 s**.

**Incremental: 38.5k reads that changed nothing.** The old path read and
SHA-256-hashed every file (1.34 s CPU), walked the tree for Cargo crates
(150 ms), loaded all 305,808 import bindings (360 ms), and scanned 158,806
pending rows. It also statted 92,089 sparse-excluded paths because `ls-files`
lists all 134,638 tracked paths. Now `git status -uno` plus one tagged
`ls-files` pass drive the change set; clean tracked paths skip both stat and
read; skip-worktree paths are omitted; imports load only for changed files
(`imports_file`); Rust crate roots come from indexed paths; and the retry is a
single `pending_name`-indexed query over the resolver's unique names. A run
that adds or removes no symbol skips the retry, and the WAL checkpoint makes the
next process read the main file instead of replaying the log. Incremental:
**4.81 → 0.58 s**.

**`map`: ten columns for every symbol.** `store.Nodes` (10 columns × 87,478
rows) was 36% of `BuildMap`, and paths were loaded for all 38,548 files even
though only the returned ~162 symbols need them. `map` now reads five columns
(`MapNodes`) and resolves paths only for the head. `BuildMap` median fell
**342 → 215 ms**, p95 **223 ms**.

## Guards

- No schema change, no node kind, no node-identity change, no new Go dependency.
- Resolution rules and fixture gates are untouched: TS/Python/Rust fixture edges
  remain **9 / 31 / 27** at precision **1.00** and recall **1.00**.
- `make perf` 10k/50k stayed green and improved: 10k cold **1.74 s**, 10k
  incremental **111 ms**, 50k cold **9.34 s**, 50k incremental **571 ms**, 50k
  `map` p95 **148 ms**, 50k RSS **290 MiB**, zstd **25.4% / 25.2%**.
- Exit codes and every CLI/MCP/HTTP contract are unchanged.

## Not verified

- One Linux machine, warm page cache; the load-sensitive numbers above are not
  a statistical benchmark.
- Sparse-checkout changes were not exercised end to end. Skip-worktree paths are
  omitted while excluded; when `git sparse-checkout` brings them back the bit
  clears and the normal stat path picks them up, but that transition was not run
  on the pinned clone.
- Untracked indexed files are re-hashed on every run because git keeps no stat
  cache for them. A repo with many untracked source files will see the
  incremental cost grow; the pinned clone has essentially none.
- The Python corpus is small (37 files), so its timings are not evidence for
  the 10k/50k budgets and are not comparable to Kibana; they show that the
  Python path works and that no language-specific regression appears.
- `scip-python` indexed `src/` and not `tests/`, so the Python adapter run
  cannot resolve references in test files; that is the producer's scope.
- Warm/cold parity after #49 is measured on the two small Python/Rust corpora
  (identical pending and edge row sets), not on a large Python or Rust tree;
  TypeScript barrel re-exports remain the documented gap. The adapter
  before/after numbers still use the cold graph.
- `check` stopped at the 200-finding cap; the findings were not audited for
  true/false positives.
- Artifact pull into a dirty real checkout (overlay cost) was not measured.

## Artifact size correction

The old PLAN §4.1 estimate of 250–400 MB at 50k files was **wrong by roughly
4×**. The real 38,548-file TypeScript artifact is 65.6 MiB uncompressed and
17.2 MiB zstd (26.2%, essentially the synthetic 25.4%). The artifact holds
compact symbol rows with no source text, so file count alone overstated the
size. The estimate is corrected in PLAN §4.1 and `docs/sharing.md`.

## TypeScript barrels, workspaces, and static dynamic imports (#56)

Issue #56 closed the last three documented TypeScript resolution gaps: barrel
re-exports (`export * from`, `export { a } from`, `export { default as X } from`),
`package.json` workspace packages (`@scope/pkg`), and static `import("./x")` /
`require("./x")` bindings. The barrel walk is bounded at eight hops, cycle-safe,
and deterministic; [`docs/resolution.md`](resolution.md#typescript--javascript)
has the full ladder and the remaining gaps.

### Method

A private copy of the same pinned clone (`cp -a .cache/kibana .cache/kibana-56`,
1.1 GiB). `<root>/.repath/` and `<root>/.git/repath/` are removed before every
cold run: a `--full` run over a *seeded* manifest leaves edges the new rules no
longer produce, so a cache-seeded "cold" run is not a baseline. The baseline and
after binaries ran on the same host in the same session; the machine was shared,
load average 2.6–5.9, which is why ranges are given.

### Graph before / after

| Metric | Baseline (main) | After #56 | Delta |
| --- | --- | --- | --- |
| Indexed files / nodes | 38,548 / 87,478 | same | — |
| Parse failures | 158 | 158 | — |
| Edges | 61,904 | **62,489** | +585 |
| `CALLS` / `REFERENCES` / `IMPLEMENTS` / `EXTENDS` | 59,988 / 1,642 / 60 / 214 | 60,571 / 1,643 / 61 / 214 | +583 / +1 / +1 / — |
| Pending references | 158,503 (71.91%) | **157,205 (71.56%)** | −1,298 (−0.36 pp) |
| Ambiguous edges | 5,242 (8.47%) | **5,138 (8.22%)** | −104 |

Pending by reason:

| Reason | Baseline | After #56 | Delta |
| --- | --- | --- | --- |
| `member-scope` | 87,706 | 87,672 | −34 |
| `missing-definition` | 65,952 | 65,643 | −309 |
| `parse-failure` | 359 | 355 | −4 |
| `scope-gap` | **4,486** | **3,535** | **−951** |

The slope is the honest part: this work targets `scope-gap`, which is 2.8% of
the subtree's unresolved references. The pending rows that disappeared are
951 `scope-gap`, 309 `missing-definition`, 34 `member-scope`, and 4
`parse-failure`; **no pending row was added** (row-set diff: 1,298 removed, 0
added). The remaining 71.56% is the member-call ceiling that typed receivers,
SCIP, and the documented gaps still own.

Edges by rule (rules that moved; the Python rows are untouched):

| Rule | Baseline | After #56 | Delta |
| --- | --- | --- | --- |
| `ts.import.named` | 28,822 | 28,922 | +100 |
| `ts.import.namespace` | 528 | 529 | +1 |
| `ts.import.reexport` | — | **1,159** | +1,159 |
| `ts.import.star` | — | **1,738** | +1,738 |
| `ts.typed.method` | 567 | 568 | +1 |
| `ts.typed.method.reexport` | — | **120** | +120 |
| `ts.typed.method.global` | 168 | 53 | −115 |
| `ts.global.unique` | 5,094 | 2,779 | −2,315 |
| `ts.global.ambiguous` | 5,224 | 5,120 | −104 |
| `ts.local` / `ts.this.method` | 18,412 / 2,555 | same | — |

Two effects explain the shape:

1. **2,317 edges keep their symbol pair and change rule**, almost all upgrades:
   2,115 `ts.global.unique` (0.60, the pending retry's unique-name guess) become
   `ts.import.star` (1,159) or `ts.import.reexport` (956); 114
   `ts.typed.method.global` (0.70) become `ts.typed.method.reexport` (0.80); 37
   `ts.global.ambiguous` (0.40, several candidates) become one exact barrel edge.
2. **802 new symbol pairs**: 579 `ts.import.star`, 202 `ts.import.reexport`, 14
   static `require`/`import()` named bindings (stored as `ts.import.named`), 6
   `ts.typed.method.reexport`, 1 `ts.import.namespace`.

215 pairs disappeared, and they are the cost side: 148 were 0.60
`ts.global.unique` edges pointing at an unrelated same-named project symbol
whose real origin is a dependency the local barrel re-exports from (for example
`@kbn/apm-types` through a local `formatters/index.ts`) — the reference is now
correctly external, matching the Python `requests.compat` rule; 67 were the
wrong candidates of an ambiguity group the barrel now resolves exactly. Both are
precision improvements, but they remove impact/caller branches from those local
symbols, so they are recorded rather than celebrated.

`ts.import.workspace*` is 0 on Kibana: the root `package.json` has no
`workspaces` field (its workspace list lives in `pnpm-workspace.yaml`, which
`docs/resolution.md` lists as not read), so `@kbn/*` imports stay external.
Workspace resolution is fixture- and unit-tested instead
(`testdata/fixtures/ts/packages/*` and `TestTypeScriptWorkspacePackageResolves`).

### Cold / incremental / full parity

One lineage, compared as full row sets (file paths and qnames, rule,
confidence, line, and ambiguous candidate ids) rather than counts:

| Path | Edges | Pending rows | Row sets |
| --- | --- | --- | --- |
| Cold | 62,489 | 157,435 | reference |
| Ten-file comment edit, incremental | 62,489 | 157,435 | identical |
| Second `index --full` | 62,489 | 157,435 | identical |

Two independent cold runs also agree on every edge row once the node ids inside
`candidates` are excluded: ids are handed out in file-write order, which is not
stable across processes; the graph is.

### Budgets

| Metric | Baseline run | After #56 | PLAN §10 target |
| --- | --- | --- | --- |
| Cold index | 27.13 s | 26.9–27.8 s | < 3 min · pass |
| Peak RSS (cold) | 953.3 MiB | 932.1–1005.0 MiB, one run at 1.00 GiB | < 1 GiB · pass, one borderline miss |
| 10-file incremental | 759 ms | 737–814 ms | < 1 s · pass |
| `impact` p50 / p95 | 77.3 / 89.1 ms | 74.3 / 84.1 ms | < 150 ms p95 · pass |
| `callers` p50 / p95 | 51.5 / 65.1 ms | 48.8 / 64.8 ms | < 150 ms p95 · pass |
| `path` p50 / p95 | 69.3 / 97.3 ms | 67.7 / 107.9 ms | < 250 ms p95 · pass |
| `map` p50 / p95 | 306.1 / 323.7 ms | 296.8 / 312.6 ms | < 300 ms p95 · miss |
| `check` | 1.12 s | 1.04 s | no target |
| Artifact / zstd | 65.6 → 17.2 MiB (26.2%) | 66.4 → 17.4 MiB (26.3%) | no target |

The `map` miss is the documented host-load variance from #43 (a quiet machine
measured 223 ms there) and the baseline missed it in the same session. The
incremental path now lazily loads the import bindings of unchanged barrels
through the same `importsOf` path Python and Rust use; the ten files the harness
touches reach few barrels, so the budget holds. A cold run reads the root
`package.json` once on the first bare specifier (ties: one 5.5 KiB read; Kibana
has no workspaces) and persists 10,417 more `imports` rows (305,808 → 316,225),
which is ≈25 MiB of live heap — the reason cold RSS sits near the 1 GiB line.

### Not verified

- Workspace resolution is not measured on a real package.json-workspaces
  monorepo. Kibana uses `pnpm-workspace.yaml` and no other pinned corpus has
  local workspaces, so that part of #56 is fixture/unit evidence only.
- Every number above is one Linux host with a warm page cache and a shared
  load (2.6–5.9). The `map` and RSS rows are load-sensitive.
- Only the `x-pack/solutions` subtree is indexed, so no whole-Kibana claim.
- `export * as ns`, computed specifiers, subpath `exports` maps, and
  non-package.json workspace manifests are documented gaps, not measured.
- The 215 removed pairs were sampled and classified by hand; they were not
  audited one by one.

## Python and Rust corpora (#48)

Issue #48 extended the same matrix to a second and a third language and split
the Rust adapter gain into production and test code. Both corpora are opt-in
and are driven by `make bench-real-langs`; the Kibana run above is unchanged.
The SCIP producers are external and pinned, and CI never needs Python or Rust.

### Pinned shape

| | Python | Rust |
| --- | --- | --- |
| Repository | [`psf/requests`](https://github.com/psf/requests) | [`rayon-rs/rayon`](https://github.com/rayon-rs/rayon) |
| Commit | `3c7baaa05ab4aa49cc819ce3a819d260de19c157` | `599425d4a4c64714632817f48496dbc53f656a90` |
| Indexed files | 37 | 191 |
| Nodes | 787 (510 method, 181 function, 96 class) | 3,402 (1,600 method, 866 function, 422 type, 382 struct, 86 const, 38 trait, 8 enum) |
| Parse failures | 0 | 0 |
| Edges | 1,031 (707 `CALLS`, 283 `REFERENCES`, 41 `EXTENDS`) | 2,161 (1,356 `CALLS`, 472 `REFERENCES`, 333 `IMPLEMENTS`) |
| Unresolved | 543 (34.50%) | 2,048 (48.66%) |
| Ambiguous | 25 (2.42%) | 75 (3.47%) |
| Entrypoints | 348 (1 `py.main`, 347 `py.test`) | 467 (13 `rs.main`, 454 `rs.test`) |
| SCIP producer | `scip-python` 0.6.8, 817,113 B | `rust-analyzer` 1.97.0 (`2d8144b`), 7,395,613 B in 34.4 s |

`rayon` is a workspace: the table indexes the `rayon` and `rayon-core` library
sources, `tests/`, and the `rayon-demo` examples/benchmarks crate. It was
chosen over `serde_json` because its production code is generic/trait-heavy
(34,227 library source lines vs 2,102 test lines) and its adapter gain is not
test-dominated; `serde_json` remains the #44 measurement in
[`docs/adapters.md`](adapters.md).

### Command

```sh
# Python corpus (the SCIP producer is only needed for the adapter half):
git clone https://github.com/psf/requests /tmp/requests
git -C /tmp/requests checkout 3c7baaa05ab4aa49cc819ce3a819d260de19c157
cd /tmp/requests
npx --yes --package=@sourcegraph/scip-python@0.6.8 \
  scip-python index --project-name requests \
  --output /tmp/requests.scip

# Rust corpus (rust-analyzer 1.97.0; the crate's dependencies must be available):
git clone https://github.com/rayon-rs/rayon /tmp/rayon
git -C /tmp/rayon checkout 599425d4a4c64714632817f48496dbc53f656a90
cd /tmp/rayon
LD_LIBRARY_PATH=$HOME/.rustup/toolchains/stable-x86_64-unknown-linux-gnu/lib \
CARGO_HOME=/path/to/cargo-home \
  /path/to/rust-analyzer scip --output /tmp/rayon.scip .

# Matrix + adapter before/after, one process per corpus:
cd /path/to/repath
make bench-real-langs \
  REPATH_BENCH_PY_ROOT=/tmp/requests \
  REPATH_BENCH_PY_COMMIT=3c7baaa05ab4aa49cc819ce3a819d260de19c157 \
  REPATH_BENCH_PY_SCIP=/tmp/requests.scip \
  REPATH_BENCH_RS_ROOT=/tmp/rayon \
  REPATH_BENCH_RS_COMMIT=599425d4a4c64714632817f48496dbc53f656a90 \
  REPATH_BENCH_RS_SCIP=/tmp/rayon.scip
```

`TestBenchRealLanguages` checks the commit and a clean tree, removes
`.repath/`, touches ten source files (restored from memory), and logs a
`BUDGET MISS` instead of failing. The SCIP pass runs on a second cold index
(the harness rebuilds `.repath/` first), so "before" is the same cold graph
this page reports; the re-index time is logged rather than hidden.

### Results

Three runs on 2026-11-13 on the same shared host as the Kibana table; medians
with the range in parentheses. Small trees are dominated by process noise, so
these numbers are evidence that no language-specific regression appears, not a
scale claim.

| Metric | Python (`requests`, 37 files) | Rust (`rayon`, 191 files) |
| --- | --- | --- |
| Cold index | **162.2 ms** (117.1–166.7) | **436.3 ms** (248.7–449.7) |
| Peak RSS (cold) | **32.1 MiB** (32.0–32.4) | **45.3 MiB** (45.0–46.1) |
| 10-file incremental | **45.0 ms** (35.9–50.9) | **53.8 ms** (35.8–54.6) |
| `impact` p50 / p95 | **0.95 / 2.28 ms** | **1.82 / 3.10 ms** |
| `callers` p50 / p95 | **0.56 / 0.89 ms** | **1.30 / 1.53 ms** |
| `path` p50 / p95 | **2.01 / 7.51 ms** | **3.09 / 7.18 ms** |
| `map --max-tokens 2000` p50 / p95 | **4.17 / 5.11 ms** | **13.61 / 15.35 ms** |
| `check` | **13.1 ms** (16 findings) | **38.7 ms** (200 findings, capped) |
| Artifact | **464 KiB**, export 6.9 / pull 4.6 ms | **980 KiB**, export 16.2 / pull 5.3 ms |
| Artifact zstd | **104.6 KiB** (22.5%), export 14.3 / pull 7.1 ms | **291.7 KiB** (29.8%), export 25.6 / pull 8.5 ms |
| Peak RSS (all measurements) | **45.8 MiB** | **66.0 MiB** |

Sample sizes: 20 `impact`/`callers` samples and 19 `path` samples for Python,
20/16/15 for Rust, and 5 `map` samples, using the same deterministic picker as
the Kibana run. The host was shared, so medians are reported and the ranges
show the spread. Every row is below its PLAN §10 ceiling, but the budgets
target 10k/50k trees, so these small-tree rows are not scale evidence.

### Adapter before/after

The adapter pass consumes the pinned SCIP file. "Before" is the cold
tree-sitter graph; "after" is the same index after `repath adapter --scip`.

| | Python (`scip-python`) | Rust (`scip-rust`) |
| --- | --- | --- |
| Definitions / references | 1,485 / 6,734 | 14,878 / 53,694 |
| Matched pending refs | **20** | **834** |
| Replaced ambiguous groups / rows | **0 / 0** | **17 / 41** |
| New edges | **18** | **766** |
| Conflicts | 0 | 0 |
| Skipped / unmatched | 6,305 / 945 | 55,898 / 9,411 |
| Unresolved | **543 → 523** | **2,048 → 1,214** |
| Unresolved rate | **34.50% → 33.27%** | **48.66% → 29.44%** |
| Edges | 1,031 → 1,049 | 2,161 → 2,886 |
| Ambiguous rows (groups) | 25 (8) → 25 (8) | 75 (34) → 34 (17) |
| Adapter wall | 302 ms (300–320) | 1.43 s (1.40–1.44) |

Python's 18 new edges are all `scip.py.method` from `src/` (0 test): real
inherited/mixin calls such as `HTTPDigestAuth.__call__ ->
RequestHooksMixin.register_hook`, `JSONDecodeError.__init__ ->
RequestException.__init__`, and `PreparedRequest.copy ->
CaseInsensitiveDict.copy`. `scip-python` indexed `src/` and not `tests/`, so
this run cannot resolve references in test files; that is the producer's scope,
not a filter (see [`docs/adapters.md`](adapters.md)).

Rust's 766 new edges are **640** `scip.rs.method`, **105** `scip.rs.call`,
**17** `scip.rs.new`, and **4** `scip.rs.reference`. The 17 extra edges over the
749 pending-derived ones are the ambiguous-group replacements (17 groups, 41
candidate rows); all 17 have a non-test source path, which is why the non-test
split grows from 690 to 739. The #48 sub-split of the 749 pending edges by
source symbol's file:

| Source | Edges | Share |
| --- | --- | --- |
| Library production (`rayon` 629, `rayon-core` 8) | **637** | 85.0% |
| Inline `#[test]` functions | **32** | 4.3% |
| `tests/` integration files | **27** | 3.6% |
| `rayon-demo` examples/benchmarks | **53** | 7.1% |

So **92.1%** of the 749 pending-derived edges are non-test and **85.0%** are
library production; including the 17 replacements, the overall split is
739 non-test / 27 test-path (96.5% / 3.5%). This is the production/test split
#48 asked for, and it reverses the
#44 `serde_json` picture where the test module dominated. Test classification
is `internal/testpath.IsTest` plus a `#[test]` entrypoint (the harness logs
`59 test (path 27, entrypoint 32)`); a test-support module such as
`src/iter/test.rs` that is not itself an entrypoint stays in the production
column, so 637 is an upper bound. The demo crate is reported
separately rather than folded into library production. Representative edges
include `Callback.callback -> Consumer.split_at`, `BytesProducer.fold_with ->
Folder.consume_iter`, and `BlocksCallback.callback -> Producer.split_at`.

### Finding: warm re-indexes did not reproduce the cold graph (#49)

This run surfaced a real inconsistency, fixed by issue #49. The measurements
below are the evidence that found it and are kept as measured.

| Path | Cold | After ten-file incremental |
| --- | --- | --- |
| `requests` unresolved | 543 | **548** (+5) |
| `rayon` unresolved | 2,048 | **2,056** (+8) |
| Edges | unchanged | unchanged |

The five Python refs are `urlparse` calls in the re-indexed
`src/requests/adapters.py` and `src/requests/auth.py`; a cold run classifies
them external by following `requests.compat`'s re-export of
`urllib.parse.urlparse`. During an incremental run, resolution loads import
bindings only for the files it re-indexes (`resolve.Build(..., changed)`), so a
re-export chain through an unchanged module cannot be followed and the ref
lands in `pending` instead. The eight Rust refs are `broadcast`,
`spawn_broadcast`, and `spawn` calls in `rayon-core/src/broadcast/test.rs`, the
same shape through the `rayon-core` re-exports in `lib.rs`. Edges are unchanged
because re-indexing a file never deletes an edge from it: the cold Python refs
were external (no edge), while the eight Rust refs keep their
`rs.use.reexport` edges **and** gain a pending row, so the same call is
reported both resolved and unresolved.

A second inconsistency: running `repath index --full` twice on `rayon` gives
2,048 pending on the first (cold) run and **2,077** on the second, with edges
unchanged. The pending retry was skipped when no symbol was added or removed and
the run was not the initial one, so 29 refs that a cold run resolves stayed
pending; `--full` was not cold-equivalent. The #43 parity claim held for the
TypeScript Kibana tree (where barrel re-exports are a documented gap) but not
for Python/Rust re-export chains.

**Fix (#49).** `resolve.Index` now loads the import bindings of a re-export
chain's unchanged modules on first use (`importsOf`): one `imports_file`-indexed
read per file, cached for the run, with one bulk read replacing the remaining
lookups after 256 files, and the error checked before anything is written.
TypeScript is deliberately not expanded, so the Kibana incremental path still
reads only the changed files; barrel re-exports stay its documented gap. A
`--full` run is now treated as a fresh run for the pending retry, so it
completes the retry like a cold index instead of skipping it. Re-measured on the
same pinned clones, a ten-file comment edit and a second `index --full` produce
the cold graph exactly:

| Path | Cold | Ten-file incremental | Second `index --full` |
| --- | --- | --- | --- |
| `requests` pending / edges | **543 / 1,031** | **543 / 1,031** | **543 / 1,031** |
| `rayon` pending / edges | **2,048 / 2,161** | **2,048 / 2,161** | **2,048 / 2,161** |

The pending row sets (name, path, line, kind, form) and edge row sets
(src/dst, kind, rule, line, confidence) are identical to cold, and the
`urlparse` and `broadcast`/`spawn_broadcast`/`spawn` refs no longer appear as
both an edge and a pending row. The harness still rebuilds `.repath/` before the
adapter baseline; after #49 that rebuild merely re-confirms the cold graph
instead of working around a divergence. The regression tests are in
`internal/index/incremental_test.go`
(`TestIncrementalFollowsPythonReexportThroughUnchangedModule`,
`TestIncrementalFollowsRustReexportThroughUnchangedModule`,
`TestFullRunReproducesColdGraph`).

### Guards

- No schema, node-kind, node-identity, rule, or fixture-gate change; no new Go
  dependency. The SCIP producers stay external and are pinned by this page.
- TS/Python/Rust fixture edges remain **9 / 31 / 27** at precision **1.00** and
  recall **1.00**; the adapter-on gates are unchanged.
- `make perf` 10k/50k stayed green: this session measured 10k cold **4.15 s**,
  incremental **221 ms**, `map` p95 **61.4 ms**; 50k cold **12.05 s**,
  incremental **690 ms**, `map` p95 **193 ms**, RSS **296 MiB**, zstd **25.2%**.
  The budgets themselves are unchanged.
- `make bench-real` (Kibana) and `make bench-real-langs` stay opt-in and are not
  CI gates.

## Large corpora (2026-11-14)

The #48 corpora are small. This run adds a genuinely large Python repository
and a production Rust agent workspace, using the same opt-in
`make bench-real-langs` matrix. Both are external clones and are not
committed. The producers were pinned by version and output size.

### Pinned shape

| | Python | Rust |
| --- | --- | --- |
| Repository | [`django/django`](https://github.com/django/django) | [`Goutam-11/hyusk`](https://github.com/Goutam-11/hyusk) |
| Commit | `f24f38c6b2825f15628c07bf2892f1664eed9361` (2026-11-13) | `06bdb2b15fcc0a0f7dc3f2b06e0003dc1731cb82` (2026-11-12) |
| Files present | 7,102 | 217 |
| Indexed | 3,051 (2,943 `.py`, 108 `.js`) | 88 (68 `.rs`, 14 `.js`, 6 `.py`) |
| Nodes | 44,426 (29,999 method, 11,100 class, 3,327 function) | 2,428 (1,377 function, 570 method, 221 const, 182 struct, 46 enum, 19 class, 11 type, 2 trait) |
| Parse failures | 3 | 7 |
| Edges | 45,834 (17,281 `CALLS`, 19,865 `REFERENCES`, 8,688 `EXTENDS`) | 2,706 (2,356 `CALLS`, 349 `REFERENCES`, 1 `IMPLEMENTS`) |
| Unresolved | 105,319 (69.68%) | 4,222 (60.94%) |
| Ambiguous | 1,572 (3.43%) | 23 (0.85%) |
| Entrypoints | 18,443 (18,438 `py.test`, 5 `py.main`) | 549 (523 `rs.test`, 20 `py.test`, 4 `py.main`, 2 `rs.main`) |
| Shape note | 906 production `.py` vs 2,009 test `.py` | 68 `.rs` / 49,126 Rust LOC / 345 `#[test]`; 750-crate workspace |
| SCIP producer | `scip-python` 0.6.8, production-only 30.3 MiB (full tree 100.3 MiB, see below) | `rust-analyzer` 1.97.0, 7.1 MiB in 157.7 s |

`hyusk` is a production agent workspace with a large inline test surface; the
14 JavaScript and 6 Python files are tooling/scripts, so the Rust surface is
the 68 `.rs` files. Django is deliberately test-heavy: 2,009 test modules vs
906 production modules. Query sampling excludes test paths, so its
`impact`/`callers`/`path` rows are production-symbol measurements; the cold,
incremental, and artifact rows cover the whole tree.

### Command

```sh
# Python corpus. scip-python 0.6.8 OOMs on the full 2,941-file tree at Node's
# default heap. The production package works at a 3 GB heap and is the bounded
# fallback; the full tree works at a 4 GB heap (#57):
git clone --depth 1 https://github.com/django/django /tmp/django
git -C /tmp/django checkout f24f38c6b2825f15628c07bf2892f1664eed9361
NODE_OPTIONS=--max-old-space-size=3072 \
  scip-python index --project-name django --cwd /tmp/django \
  --target-only django --output /tmp/django-prod.scip
NODE_OPTIONS=--max-old-space-size=4096 \
  scip-python index --project-name django --cwd /tmp/django \
  --output /tmp/django-full.scip

# Rust corpus (rust-analyzer 1.97.0; a 750-crate workspace, ~158 s).
git clone --depth 1 https://github.com/Goutam-11/hyusk /tmp/hyusk
git -C /tmp/hyusk checkout 06bdb2b15fcc0a0f7dc3f2b06e0003dc1731cb82
rust-analyzer scip --output /tmp/hyusk.scip /tmp/hyusk

cd /path/to/repath
make bench-real-langs \
  REPATH_BENCH_PY_ROOT=/tmp/django \
  REPATH_BENCH_PY_COMMIT=f24f38c6b2825f15628c07bf2892f1664eed9361 \
  REPATH_BENCH_PY_SCIP=/tmp/django-prod.scip \
  REPATH_BENCH_RS_ROOT=/tmp/hyusk \
  REPATH_BENCH_RS_COMMIT=06bdb2b15fcc0a0f7dc3f2b06e0003dc1731cb82 \
  REPATH_BENCH_RS_SCIP=/tmp/hyusk.scip
```

The producers are not part of repath or CI. The production-package Python file
(909 documents) is the bounded mode; the full-tree file (2,941 `.py`, 100.3 MiB)
is the large-tree mode measured for #57. The Rust file covers the workspace
root.

### Results

Three runs on the same shared host, medians with the range in parentheses. The
page cache was warm after the first cold index; the first Django cold runs were
6.45–6.56 s before settling. Both trees are below every PLAN §10 ceiling.

| Metric | Python (`django`, 3,051 files) | Rust (`hyusk`, 88 files) |
| --- | --- | --- |
| Cold index | **4.54 s** (4.29–6.56) | **292 ms** (289–298) |
| Peak RSS (cold) | **293 MiB** (290–293) | **66.4 MiB** (64.2–69.2) |
| 10-file incremental | **209 ms** (207–209) | **41.4 ms** (38.9–95.3) |
| `impact` p50 / p95 | **12.6 / 14.2 ms** | **0.97 / 1.71 ms** |
| `callers` p50 / p95 | **10.6 / 11.7 ms** | **0.57 / 0.94 ms** |
| `path` p50 / p95 | **19.2 / 27.4 ms** | **2.16 / 12.55 ms** |
| `map --max-tokens 2000` p50 / p95 | **151.8 / 160.8 ms** | **7.63 / 8.48 ms** |
| `check` | **519 ms** (200 findings, capped) | **27.1 ms** (200 findings, capped) |
| Artifact | **22.3 MiB**, export 155 / pull 14.6 ms | **1.0 MiB**, export 8.8 / pull 3.3 ms |
| Artifact zstd | **5.8 MiB** (26.1%), export 251 / pull 46.4 ms | **313.7 KiB** (29.4%), export 17.6 / pull 6.4 ms |
| Peak RSS (all measurements) | **305 MiB** (301–349) | **92.5 MiB** (89.2–96.8) |

The 3,051-file Django tree is **82×** the indexed file count of the #48
`requests` corpus and **16×** the `rayon` corpus; its cold index is still
under the 10k-file budget, incremental is under 300 ms, and `map` is under
200 ms. The high unresolved rate (69.7%) is the documented Python ceiling:
untyped receivers, inheritance, and dynamic dispatch dominate a framework
codebase.

### Adapter before/after

Django uses the production-package SCIP index; `hyusk` uses the workspace
index. "Before" is the cold tree-sitter graph.

| | Python (`scip-python`, 909-file index) | Rust (`scip-rust`, 750-crate workspace) |
| --- | --- | --- |
| Definitions / references | 52,884 / 157,794 | 10,819 / 65,165 |
| Matched pending refs | **2,013** | **141** |
| Replaced ambiguous groups / rows | **15 / 32** | **0 / 0** |
| New edges | **1,684** | **88** |
| Conflicts | 10 | 2 |
| Skipped / unmatched | 142,241 / 30,550 | 63,242 / 10,208 |
| Unresolved | **105,319 → 103,306** | **4,222 → 4,081** |
| Unresolved rate | **69.68% → 68.50%** | **60.94% → 59.36%** |
| Edges | 45,834 → 47,486 | 2,706 → 2,794 |
| Ambiguous rows (groups) | 1,572 (487) → 1,540 (472) | 23 (11) → 23 (11) |
| Adapter wall | 6.08 s (6.07–6.26) | 0.62 s |

The ambiguous-pass overlap is small: the adapter reads 487 candidate groups
(1,572 rows) on Django and replaces 15 of them; `hyusk`'s 23 ambiguous rows are
not named by this producer's references and stay ambiguous. New edges are
counted after the 32/41 candidate-row deletions, so `Edges` before → after is
the exact reconciliation: `45,834 + 1,684 − 32 = 47,486`.

Django's 1,684 new edges are **1,627** `scip.py.method`, **47**
`scip.py.call`, and **10** `scip.py.reference`; all source files are under
`django/`. The harness classifies **81** of them as test-path because Django's
production `django/test/` package is the test client/framework and
`testpath.IsTest` treats `*/test/*` as test code — those are not the test
suite. `hyusk`'s 88 edges are 66 method and 22 call; **54** have a non-test
source and **34** are `#[test]` entrypoints, all under `src/`.

### Finding: producer scale — full tree works with a documented heap (#57)

`scip-python` 0.6.8 **crashes on the full Django tree at Node's default heap**:
after parsing 2,931 of 2,941 files it aborts with `FATAL ERROR: Ineffective
mark-compacts near heap limit - JavaScript heap out of memory`. Issue #57
re-ran the producer with an explicit heap and the full tree **succeeds**:

| Producer mode | Node heap | Documents | Wall | Max RSS | Index |
| --- | --- | --- | --- | --- | --- |
| full tree | `--max-old-space-size=4096` | **2,941 `.py`** | **2:08** | **3.74 GiB** | **100.3 MiB** (105,191,043 B) |
| production-only (bounded fallback) | `--max-old-space-size=3072` | 909 | 42.9 s | 1.77 GiB | 30.3 MiB |

Both exit `0`; the full-tree command is recorded in the Command block above.
Applying the full-tree index to the cold Django graph takes **33.1 s** and
347.6 MiB RSS and yields **169,269** definitions / **613,467** references:

| Full-tree adapter | Before | After |
| --- | --- | --- |
| Unresolved | 105,319 | **82,510** |
| Edges | 45,834 | **61,823** |
| Ambiguous rows (groups) | 1,572 (487) | **658 (192)** |
| Matched pending / replaced groups / rows | — | **22,809 / 295 / 914** |
| New edges by rule | — | **15,218** method, **1,210** call, **475** reference |
| Conflicts | — | **33** |

The reconciliation is exact: `45,834 + 16,903 − 914 = 61,823`. The full index
also covers the test suite, so **15,150** of the 16,903 new edges have a
test-path source and **1,753** are production; the production-only index above
is the application-code number. The replacement path participates at scale:
**295** ambiguous groups (914 candidate rows) are replaced, leaving **658**
ambiguous rows that the compiler's occurrence set does not disambiguate.

`rust-analyzer` 1.97.0 does finish `hyusk` (750 crates, 157.7 s, 7.1 MiB), but
the adapter gain is modest: only 141 of 65,165 references match a pending
tree-sitter row, because most references are external, standard-library, or
already resolved. The production-heavy split (54 of 88) is the useful signal;
this is not a repeat of `rayon`'s generic-bound corpus.

### Gaps

- Django is a Python framework with a huge test suite; a production-only
  application corpus (roughly 500–1,500 files) would separate "framework
  patterns" from "application code" in the unresolved rate.
- The full-tree Python run needs **~3.74 GiB** of producer RSS (documented 4 GB
  Node heap); small CI or laptop hosts must use the bounded
  `--target-only django` mode. That is a producer memory limit, not a repath
  limit — the adapter itself peaked at 347.6 MiB on the 100.3 MiB index.
- `hyusk` has 7 parse failures among its non-Rust files; they were not
  audited. The Rust-only matrix is the evidence, and the JS/Python scripts are
  tooling, not the corpus.
- One host, warm page cache, three runs; no CPU pinning or multi-machine
  replication. `check` stopped at the 200-finding cap for both trees.
- `callers`/`impact` now have a compiler ground-truth check on the smaller
  `requests` and `rayon` corpora (see [Reproducible corpora and compiler
  accuracy](#reproducible-corpora-and-compiler-accuracy-2026-11-15-61)); the
  large Django/hyusk numbers above were not accuracy-checked.

## Python inheritance and receiver typing (2026-11-16, #54)

Issue #54 closes the largest static Python gap. `self.m()`/`cls.m()` now follow
the class's MRO, zero-argument `super().m()` is followed, and receivers are typed
from annotations, constructors, same-file factory/method return annotations, and
class attributes (including attributes declared on a same-file base). The rule
ids are `py.self.inherited` (0.85), `py.self.super` (0.85), `py.class.inherited`
(0.85), `py.typed.inherited` (0.80), and `py.property.alias` (0.90); the ladder
and every remaining gap are in [`docs/resolution.md`](resolution.md).

Pinned corpora and the exact commands:

```sh
# django/django @ f24f38c6b2825f15628c07bf2892f1664eed9361 (3,051 indexed .py)
# psf/requests   @ 3c7baaa05ab4aa49cc819ce3a819d260de19c157 (37 indexed .py)
rm -rf <corpus>/.repath <corpus>/.git/repath
./bin/repath index --full --json --root <corpus>
./bin/repath status --json --no-refresh --root <corpus>
```

### Results

Same binary before and after the change, three clean cold runs each (`.repath`
and `.git/repath` removed every time) on the shared 12-core host. Load average
was 3.5–5.3 during the runs; medians with the range in parentheses.

| Metric | django (3,051 files) | requests (37 files) |
| --- | --- | --- |
| Cold index before | **4.95 s** (4.95–7.36) | **186 ms** (185–189) |
| Cold index after | **5.28 s** (5.14–5.79) | **202 ms** (195–206) |
| 10-file incremental after | **413 ms** (410–419) | **100 ms** (single run) |
| second `index --full` after | **5.05 s** (5.04–5.41) | **245 ms** (single run) |

The MRO work costs about **6–10%** on this corpus (cold +6.6%, incremental
+6.4%, full +9.6% versus the baseline measured with the same protocol); it is
well inside every PLAN §10 ceiling, and the synthetic `make perf` budgets were
re-run green (see below).

| | django | requests |
| --- | --- | --- |
| Edges | **45,834 → 56,806** | **1,031 → 1,044** |
| Pending | **105,319 → 92,618** | **543 → 510** |
| Unresolved rate | **69.68% → 61.98%** | **34.50% → 32.82%** |
| Parse failures | 3 → 3 | 0 → 0 |

Pending by reason (the buckets are the read-time heuristic from `status`; they
sum to `unresolved`):

| Reason | django before → after | requests before → after |
| --- | --- | --- |
| `parse-failure` | 0 → 0 | 0 → 0 |
| `member-scope` | 50,052 → **48,086** | 224 → **210** |
| `missing-definition` | 40,489 → 40,489 | 308 → **288** |
| `scope-gap` | 14,778 → **4,043** | 11 → 12 |

The scope-gap drop is the `self`/`cls` bucket: those rows were classed as
scope-gap because the method name exists somewhere in the tree. The
missing-definition drop on `requests` is receiver typing turning member calls
into edges. Two small increases are deliberate and honest: `scope-gap` +1 on
`requests` and **516** new pending rows on django are zero-argument `super()`
calls whose base is external; previously the member call was silently dropped,
now it is a pending row with `form = 'super'` (the retry never guesses a member).

New edges by rule (confidence in the ladder):

| Rule | Confidence | django | requests |
| --- | --- | --- | --- |
| `py.self.inherited` | 0.85 | **8,171** | **6** |
| `py.self.super` | 0.85 | **1,236** | **2** |
| `py.typed.inherited` | 0.80 | **743** | **2** |
| `py.class.inherited` | 0.85 | **311** | **1** |
| `py.property.alias` | 0.90 | **37** | 0 |
| `py.typed.method` (delta) | 0.85 | **+501** | **+2** |

The `py.typed.method` delta and the `py.typed.method.global` delta (−27 on
django) come from the new receiver sources: a local class attribute or a
same-file factory return annotation now names the receiver instead of falling
through to the weaker global lookup.

### By-design residual

django keeps **31,512 of 34,683** pending `self`/`cls` calls as
`assert*` methods (`assertEqual` alone is 19,032): their base is
`unittest.TestCase`, which is outside the repo, so the chain ends and the call
stays unresolved instead of being guessed. External bases, explicit
`super(Child, self)` (2-argument form), and receivers typed in another module
are documented gaps, not measurement failures.

### Parity (#49)

Cold, ten-file-incremental, and second `index --full` runs produce **identical
pending and edge row sets** on both corpora:

| | pending rows | edge rows | identical sets | edge+pending pairs |
| --- | --- | --- | --- | --- |
| `requests` | 510 | 1,044 | yes (both) | 0 |
| `django` | 92,618 | 56,806 | yes (both) | 19 |

django's 19 pairs are the same-line heuristic matching two different
references with one name (`self.encode(...)` resolved while `data.encode()` on
the same line is untyped); the baseline graph has 18 such pairs, and no
reference is both resolved and pending. The base links of a re-resolved file
replace the persisted ones (`AddInheritance`), which is the property that keeps
the warm graph equal to the cold one; compiler (`scip.*`) `EXTENDS` links are
not used for the MRO.

### Adapter re-measure

The pinned `scip-python` 0.6.8 files still merge into what tree-sitter leaves
unresolved, but the tree-sitter side closed most of the inherited calls it used
to supply:

| | pending before → after adapter | edges | matched | conflicts | wall |
| --- | --- | --- | --- | --- | --- |
| `requests` | **510 → 502** (32.82% → 32.30%) | 8 | 8 | 0 | 0.32 s |
| `django` (production SCIP, 909 files) | **92,618 → 91,356** (61.98% → 61.24%) | 1,010 | 1,262 | 13 | 7.0 s |

Before #54 the `requests` run merged 18 edges from 20 matched pending rows; the
remaining 8 are the misses this new ladder still leaves (untyped locals,
cross-module receivers).

### Guards

`make check` is clean, `go test ./internal/eval -v` stays at precision 1.00 /
recall 1.00 with the Python fixture extended from 31 to **65** hand-labelled
edges (unresolved 1, the callback parameter) and the TS/Rust fixtures untouched
at 9/9 and 27/27. `make perf` was run three times: 10k cold **2.56 s**
(2.36–4.49), 10k 10-file incremental **160 ms** (137–250), 50k cold **12.62 s**
(12.33–12.67), 50k incremental **715 ms** (679–742), peak RSS **290 MiB**,
`map` p95 204 ms (50k) — every budget green. The synthetic trees are
TypeScript, so they do not exercise the MRO path; the corpus table above is the
Python evidence.

### Not verified

- Metaclasses, monkey patching, `getattr`, and runtime dispatch stay outside
  the static model by design; the by-design residual above is not evidence
  they are absent from the graph.
- Inherited class attributes and factory return annotations are typed only when
  declared in the same file; the cross-module variants (`run_factory`,
  `run_child` in the adapter fixture) are still pending.
- One shared host, three runs, warm page cache after the first read; no CPU
  pinning or multi-machine replication. The 10k/50k synthetic numbers remain
  machine-dependent evidence, not a CI gate.

## Reproducible corpora and compiler accuracy (2026-11-15, #61)

The tables above were assembled from one-off runs. Issue #61 makes the same
matrix reproducible in one command, writes machine-readable JSON instead of
hand-edited tables, keeps a small baseline so a later run can print deltas, and
measures correctness — not just speed — against the compiler's SCIP index. It is
still **evidence, never a gate**: CI needs no producer and `make check` runs none
of the corpus work.

### Command and matrix

```sh
make bench-corpora
# or, with flags:
./scripts/bench-corpora.sh [--runs N] [--corpora django,hyusk,requests,rayon] \
  [--out DIR] [--no-clone] [--update-baseline]
```

| Corpus key | Language | Repository @ commit | SCIP producer | Accuracy |
| --- | --- | --- | --- | --- |
| `django` | Python | `django/django` @ `f24f38c6b2825f15628c07bf2892f1664eed9361` | `scip-python` 0.6.8, production-only (909 documents); full tree 100.3 MiB (#57) | — |
| `hyusk` | Rust | `Goutam-11/hyusk` @ `06bdb2b15fcc0a0f7dc3f2b06e0003dc1731cb82` | `rust-analyzer` 1.97.0 (default features) | — |
| `requests` | Python | `psf/requests` @ `3c7baaa05ab4aa49cc819ce3a819d260de19c157` | `scip-python` 0.6.8, `src/` only | ✅ |
| `rayon` | Rust | `rayon-rs/rayon` @ `599425d4a4c64714632817f48496dbc53f656a90` | `rust-analyzer` 1.97.0 | ✅ |

The script resolves this matrix, verifies each clone's `HEAD` against the pinned
commit and refuses a dirty tracked tree (`?? .repath/` is ignored), clones a
missing clone under `.cache/` unless `--no-clone` is passed, then runs
`TestBenchRealLanguages` N times per corpus **in separate processes** so each
linux `VmHWM` reading belongs to that tree. It also runs the hermetic
`TestCorporaFailureModes` and, for corpora marked `accuracy`, the opt-in
`TestSCIPAccuracy`. Per-corpus paths can be overridden without editing the
script:

```sh
REPATH_BENCH_CORPUS_ROOT_DJANGO=/tmp/django \
REPATH_BENCH_CORPUS_SCIP_DJANGO=/tmp/django.scip \
  make bench-corpora ARGS="--corpora django --runs 1"
```

A missing clone is fetched/cloned; a missing SCIP file only skips the adapter
half and accuracy for that corpus. The default output directory is
`.cache/bench-corpora/<UTC>/` (gitignored); nothing tracked is written unless
`--update-baseline` is passed explicitly.

### Output and JSON schema

`<out>/` contains:

| Path | What |
| --- | --- |
| `summary.json` | aggregated result: host/CPU/RAM/load, tool versions, per-metric medians/ranges, structure, adapter stats, condensed accuracy |
| `runs/<corpus>-<n>.json` | one process's raw run (schema below) |
| `accuracy-<corpus>.json` | full SCIP accuracy report for `requests`/`rayon`, including disagreements and exclusions |
| `failure-modes.json` / `failure-modes.txt` | per-case exit codes and the test log |
| `logs/` | full `go test -v` log per run |

`summary.json` and every run carry `"schema": 1`. A run record has:

```json
{
  "schema": 1,
  "corpus": "rayon",
  "language": "rust",
  "commit": "599425d4a4c64714632817f48496dbc53f656a90",
  "go_version": "go1.27.0",
  "started_at": "2026-11-15T01:13:00Z",
  "finished_at": "2026-11-15T01:13:02Z",
  "cold_index_ms": 245.967,
  "peak_rss_bytes": 47702000,
  "incremental_ms": 32.058,
  "incremental_indexed": 10,
  "queries": {
    "impact":  {"p50_ms": 1.007, "p95_ms": 1.728, "samples": 20},
    "callers": {"p50_ms": 0.749, "p95_ms": 0.849, "samples": 20},
    "path":    {"p50_ms": 1.599, "p95_ms": 3.993, "samples": 15},
    "map":     {"p50_ms": 7.218, "p95_ms": 8.184, "samples": 5}
  },
  "check": {"duration_ms": 21.747, "findings": 200},
  "artifact": {
    "uncompressed_bytes": 1003520, "export_ms": 8.427, "pull_ms": 2.909,
    "zstd_bytes": 298553, "zstd_export_ms": 14.892, "zstd_pull_ms": 4.567,
    "ratio": 29.75, "schema_version": 5,
    "file_count": 191, "node_count": 3402, "edge_count": 2886
  },
  "structure": {
    "files": 191, "nodes": 3402,
    "nodes_by_kind": {"function": 866, "method": 1600},
    "edges": {"CALLS": 1356, "REFERENCES": 472, "IMPLEMENTS": 333},
    "edges_total": 2161, "ambiguous": 75, "ambiguity_rate": 0.0347,
    "unresolved": 2048, "unresolved_rate": 0.4866,
    "unresolved_by_reason": {"member-scope": 1357, "missing-definition": 510,
                             "scope-gap": 181, "parse-failure": 0},
    "parse_failures": 0, "entrypoints": 467, "git_head": "599425d4…"
  },
  "adapter": {
    "name": "scip-rust", "definitions": 14878, "references": 53694,
    "matched": 834, "replaced": 17, "replaced_edges": 41,
    "edges": 766, "conflicts": 0, "skipped": 55898,
    "unmatched": 9411, "wall_ms": 1426.0,
    "unresolved_before": 2048, "unresolved_after": 1214,
    "edges_before": 2161, "edges_after": 2886,
    "new_edges_by_source": {"production": 739, "test_path": 27},
    "new_edges_by_rule": {"method": 640, "call": 105, "new": 17, "reference": 4}
  }
}
```

`new_edges_by_source.production` is the non-test bucket: for `rayon` it is 690
non-test edges, including the 53 `rayon-demo` edges the #48 table reports
separately; the harness log also carries the per-root split.
`new_edges_by_rule` counts `scip.<lang>.*` suffixes, so `reference` is the
catch-all beyond `method`/`call`/`new`.

`summary.json` wraps those runs: `host` (hostname, platform, kernel, CPU model,
logical CPUs, RAM bytes, load average before/after), `tools` (Go, Rust, Python,
Node, git), `repath_commit`/`repath_version`, `runs_per_corpus`,
`failure_modes`, and `corpora.<name>` with `metrics.<metric> =
{median, min, max, runs, values}`, the structure counters, the aggregated
adapter block, and the condensed accuracy report. `unresolved_by_reason` is the
#60 read-time classifier, so the four buckets sum to `unresolved`. All timings
are milliseconds and all byte counts are bytes; `artifact.ratio` is the zstd
percentage of the uncompressed artifact.

### Results (N=3, shared host)

Host: AMD Ryzen 5 5600H, 12 logical CPUs, 7.2 GiB RAM, Linux 7.2, shared and
noisy; load average **1.34/1.63/2.19** before and **3.81/2.29/2.37** after the
run window (both recorded in `summary.json`). Medians with the observed range in
parentheses. These are the same pinned graphs as the #48/#53 tables above; the
counts reconcile exactly.

| Metric | `django` | `hyusk` | `requests` | `rayon` |
| --- | --- | --- | --- | --- |
| Cold index | **3.97 s** (3.86–4.07) | **273 ms** (273–273) | **92.8 ms** (91.6–93.1) | **240 ms** (240–246) |
| Peak RSS (cold) | **285 MiB** (271–289) | **64.4 MiB** (62.9–65.3) | **32.5 MiB** (32.2–32.6) | **45.4 MiB** (45.2–45.9) |
| 10-file incremental | **193 ms** (190–208) | **31.2 ms** (29.6–43.1) | **28.2 ms** (26.9–29.7) | **31.5 ms** (31.4–32.4) |
| `impact` p50 / p95 | **12.0 / 13.5 ms** | **0.90 / 1.59 ms** | **0.55 / 1.25 ms** | **0.98 / 2.04 ms** |
| `callers` p50 / p95 | **10.0 / 10.8 ms** | **0.56 / 0.71 ms** | **0.32 / 0.48 ms** | **0.73 / 0.86 ms** |
| `path` p50 / p95 | **18.4 / 25.1 ms** | **1.98 / 11.9 ms** | **1.17 / 4.05 ms** | **1.57 / 4.05 ms** |
| `map` p50 / p95 | **143 / 155 ms** | **7.16 / 7.49 ms** | **2.37 / 2.66 ms** | **7.47 / 8.86 ms** |
| `check` | **492 ms** (200 findings, capped) | **25.9 ms** (200, capped) | **7.08 ms** (16) | **21.5 ms** (200, capped) |
| Artifact | **22.3 MiB**, export 149 / pull 13.2 ms | **1.04 MiB**, export 8.73 / pull 2.91 ms | **460 KiB**, export 4.18 / pull 2.65 ms | **980 KiB**, export 8.36 / pull 2.87 ms |
| Artifact zstd | **5.80 MiB** (26.03%), export 234 / pull 36.0 ms | **313 KiB** (29.32%), export 22.2 / pull 5.05 ms | **104.6 KiB** (22.76%), export 8.12 / pull 4.06 ms | **291.5 KiB** (29.80%), export 14.3 / pull 4.80 ms |
| Adapter wall | **5.50 s** (5.43–5.53) | **592 ms** (574–607) | **122 ms** (121–123) | **650 ms** (638–680) |
| Matched / new edges | **2,013 / 1,669** | **141 / 88** | **20 / 18** | **834 / 749** |
| Unresolved before → after | **105,319 → 103,306** | **4,222 → 4,081** | **543 → 523** | **2,048 → 1,214** |
| Edges before → after | **45,834 → 47,503** | **2,706 → 2,794** | **1,031 → 1,049** | **2,161 → 2,910** |

Those adapter rows are the #61 baseline (before ambiguous replacement). With
the #57 policy the same three-run corpus matrix measures Django **15**
replaced groups / **32** rows, rayon **17 / 41**, requests **0 / 0**, and
hyusk **0 / 0**; the per-corpus tables above and `summary.json` carry the
current `replaced` / `replaced_edges` fields. Re-run with `--update-baseline`
to refresh the checked-in history deliberately.

Structure (tree-sitter cold graph, constant across the three runs):

| | `django` | `hyusk` | `requests` | `rayon` |
| --- | --- | --- | --- | --- |
| Files / nodes / edges | 3,051 / 44,426 / 45,834 | 88 / 2,428 / 2,706 | 37 / 787 / 1,031 | 191 / 3,402 / 2,161 |
| Unresolved (rate) | 105,319 (69.68%) | 4,222 (60.94%) | 543 (34.50%) | 2,048 (48.66%) |
| Parse failures / ambiguous / entrypoints | 3 / 1,572 / 18,443 | 7 / 23 / 549 | 0 / 25 / 348 | 0 / 75 / 467 |
| Unresolved by reason (`member-scope` / `missing-definition` / `scope-gap` / `parse-failure`) | 50,052 / 40,489 / 14,778 / 0 | 911 / 2,332 / 244 / 735 | 224 / 308 / 11 / 0 | 1,357 / 510 / 181 / 0 |

The Django row shows why the reason buckets are a pointer, not a diagnosis:
3 files fail to parse but contribute no pending rows, so `parse-failure` is 0
while 105,319 references are still classified by the other three buckets.

### Ground-truth accuracy

`TestSCIPAccuracy` (opt-in, `REPATH_ACCURACY=1`) rebuilds the cold graph in a
temporary store, loads the pinned SCIP index with the same hand-rolled parser
the adapter uses, and treats the compiler as ground truth:

- a **definition occurrence** maps to its enclosing repath node (the adapter's
  `EnclosingNode(fileID, line+1)` mapping);
- a **reference occurrence** maps to the repath node enclosing its file+line,
  so the expected callers of a target are the enclosing nodes of its references;
- **actual** callers are one-hop reverse edges at confidence ≥0.5, restricted to
  source files the SCIP document set covers (scope-matched: `requests.scip`
  covers `src/` only, so the 156 test-file callers are excluded and counted, not
  scored as false positives);
- 20 targets with an attributable caller are sampled deterministically: sort by
  target path then SCIP symbol and take evenly spaced entries (175 candidates
  for `requests`, 1,001 for `rayon`);
- precision = TP / actual, recall = TP / expected, micro-averaged over the
  sample. The same sample is measured **before** the adapter and after
  `ApplySCIPAll` on the same store.

`impact` depth 2 is compared through `graph.Blast` (the walker behind
`query.Impact`; importing `query` from the adapter test would cycle) against a
depth-2 reverse closure of the compiler graph, with truncated results excluded.
`path` is **excluded**: SCIP gives definition/reference pairs, not a shortest
chain, so there is no defensible ground truth for it.

| | `requests` (Python) | `rayon` (Rust) |
| --- | --- | --- |
| Sampled / candidate targets | 20 / 175 | 20 / 1,001 |
| SCIP references / definitions | 6,734 / 1,485 | 53,694 / 14,878 |
| Attributable references | 440 | 5,670 |
| `callers` tree-sitter | P **100.0%**, R **33.3%** (35 actual, 105 expected, 35 TP) | P **84.2%**, R **9.4%** (19 actual, 171 expected, 16 TP) |
| `callers` after adapter | P **100.0%**, R **33.3%** (35 actual, 105 expected, 35 TP) | P **86.4%**, R **11.1%** (22 actual, 171 expected, 19 TP) |
| `impact`≤2 tree-sitter | P **98.5%**, R **43.1%** (67 actual, 153 expected, 66 TP) | P **96.0%**, R **29.9%** (100 actual, 321 expected, 96 TP) |
| `impact`≤2 after adapter | P **98.6%**, R **47.1%** (73 actual, 153 expected, 72 TP) | P **96.2%**, R **31.2%** (104 actual, 321 expected, 100 TP) |

The uniform sample can miss every target the adapter repaired, so the test also
reports the **adapter-repaired cohort** separately (targets with at least one
`scip.py.*`/`scip.rs.*` edge). Its "before" row is a **snapshot of the callers
taken before `ApplySCIPAll`**, not a post-hoc rule filter: ambiguous-group
replacement deletes 0.40 candidate rows, so filtering adapter rules after the
run would no longer reconstruct the pre-adapter graph. The snapshot is why the
rayon cohort grew from 177 targets / 1,432 expected callers (#48 measurement)
to 183 / 1,447 — targets whose only pre-adapter edges were replaced ambiguous
candidates are now in the cohort.

| Adapter-repaired cohort | `requests` | `rayon` |
| --- | --- | --- |
| Targets | 14 | 183 |
| Expected callers | 27 | 1,447 |
| `callers` tree-sitter | P **100.0%**, R **33.3%** (9 TP) | P **95.7%**, R **9.2%** (133 TP) |
| `callers` after adapter | P **100.0%**, R **100.0%** (27 TP) | P **98.1%**, R **60.7%** (878 TP, **+745**) |

Reading the numbers:

- **Precision is high; recall is the gap.** Every scoped tree-sitter edge the
  sample emitted for `requests` matched a compiler reference. The missing
  references are mostly type-only uses (`SupportsRead` in
  `src/requests/_types.py:32`), annotations, and same-class/mixin method calls
  that tree-sitter does not emit; in `rayon` the missing references are
  generic-bound/trait-dispatch and macro-generated calls. That is the documented
  resolution ceiling measured as a number instead of described.
- **The adapter is provably additive.** It recovers +18 TP across 14 sampled
  Python targets (R 33.3% → 100.0%) and +745 TP across the 183 Rust targets
  (R 9.2% → 60.7%) with no precision loss on those cohorts. Of the 745, **17**
  come from ambiguous-group replacements (one per replaced group) that the
  #48 harness could not see, and the rest from pending repairs. It cannot
  repair what still has no pending row or disambiguated group (the unchanged R
  on the uniform sample).
- **Some Rust false positives remain.** Before the adapter, 3 of 19 actual
  callers in the sample point at a different target than SCIP's definition
  (ambiguous inherent/trait `Type.method` attribution); the adapter's exact
  edges improve precision to 86.4%. These are listed in
  `accuracy-rayon.json`.

Excluded facts, counted honestly in the same JSON (`exclusions`):

| Exclusion | `requests` | `rayon` | Why |
| --- | --- | --- | --- |
| `unmapped-symbol` | 2,262 | 30,964 | local bindings/module descriptors reduce to no graph target |
| `definition-unmapped-name` | 433 | 12,368 | definition is a module/namespace, not a symbol |
| `no-definition` | 2,109 | 10,348 | external/std/builtin symbol with no repath node |
| `no-enclosing-node` | 186 | 2,061 | top-level reference; repath's documented policy skips it |
| `enclosing-not-function` | 110 | 477 | class/module-level reference, not a caller fact |
| `recursive` | 1,212 | 83 | enclosing node == target; the adapter skips these by design |
| `ambiguous-same-line-name` | 10 | 486 | one `(file, line, reduced name)` resolves to two compiler targets |
| `definition-no-enclosing-node` | 103 | 74 | definition line outside any repath node |
| `import` / `generated` | 0 | 0 | SCIP roles; excluded before attribution |

Caveats: attribution is line-level, exactly the adapter's approximation, so two
same-named calls on one line are excluded rather than guessed; the compiler
graph covers only the SCIP document set (`requests` is `src/`-only and `rayon`
is default-feature Rust); `impact` depth 2 compares a compiler graph built from
attributable references only, so it is a sampled approximation, not a proof.
The whole test takes 0.4 s (`requests`) and 1.6 s (`rayon`) and never runs in CI.

### Failure modes

`TestCorporaFailureModes` is hermetic (in-process CLI, no producer, no network)
and therefore runs under `make check`; the driver also records it in
`failure-modes.json`. Each case proves the index is not half-written where that
is observable (`status --no-refresh` opens the DB after a failed adapter run).

| Case | Command | Exit | Contract / note |
| --- | --- | --- | --- |
| Missing SCIP | `repath adapter --scip <missing>` | **3** | not-found, documented |
| Corrupt / non-SCIP bytes | `repath adapter --scip <garbage>` | **1** | observed internal error (`parse scip: unsupported protobuf wire type`); issue #61 suggested `2`, but the CLI contract has no malformed-adapter-input code and `2` is reserved for usage/flags, so reality is recorded here and not changed |
| Empty file | `repath adapter --scip <empty>` | **0** | parses as a zero-document index; it records a zero-fact run rather than failing |
| Non-git directory + `index` | `repath index` | **0** | supported: full-discovery fallback, same as a plain directory |
| Non-git directory + `tests --diff` | `repath tests --diff` | **3** | documented not-a-git-work-tree bucket |
| Read-only `.repath/` | `repath index` | **1** | `open index: PRAGMA journal_mode = WAL: unable to open database file`; no `index.db` is left behind |

The corrupt-vs-empty split is deliberate: the hand-rolled parser only fails on
malformed protobuf framing, and an empty index is a valid but empty fact set.
The read-only case is skipped when the suite runs as root.

### Baseline and history

- [`bench/baseline.json`](../bench/baseline.json) is the checked-in condensed
  baseline (~30 KB, text only): per-corpus commit, condensed metrics with
  medians/ranges, structure counters, adapter medians, and the accuracy summary.
  No database, artifact, or producer output is committed.
- [`docs/benchmarks-history.md`](benchmarks-history.md) is generated from that
  baseline by `--update-baseline` — do not edit it by hand.
- A normal run reads `bench/baseline.json` and prints deltas, for example:
  `django cold_index_ms: 4146.3 -> 4201.7 (+1.3%)` and
  `rayon structure.unresolved: 2048 -> 2051 (+3)`. A changed corpus commit is
  reported as not comparable instead of pretending a delta is meaningful.
- The **only** path that writes those two tracked files is
  `--update-baseline`; every other run writes exclusively under the outdir
  (default a gitignored `.cache/bench-corpora/<UTC>/`).

### Adding a corpus

1. Add a `corpus_spec` case in `scripts/bench-corpora.sh`:
   `language|repo-url|pinned-commit|default-root|default-scip|accuracy`.
   Roots and SCIP files live outside git (under `.cache/` by default).
2. Pin a SCIP producer only when the adapter half is wanted; a missing file
   skips it. Mark `accuracy=1` only when the SCIP document set covers the files
   you intend to score (otherwise every out-of-scope caller is an honest
   exclusion, not a false positive).
3. Add the row to the matrix table above with the producer version and exact
   SCIP size. Corpora are cloned/deleted, never committed.
4. Run `make bench-corpora ARGS="--corpora newcorpus --runs 1"` first, then a
   full `--runs 3` run; refresh the baseline only when the graph is intentional:
   `make bench-corpora ARGS="--corpora newcorpus --update-baseline"` (or omit
   `--corpora` to refresh all).
5. A new *language* also needs a SCIP family (`isXDocument`, symbol reducer,
   rule prefix) and an accuracy family branch; today only `python` and `rust`
   are wired into `TestSCIPAccuracy`.

### Not verified

- One host, one run window, three runs per corpus. The host was shared (load
  average 1.7/2.0/2.5) and the small trees are process-noise dominated; these
  are medians for regression detection, not a statistical benchmark.
- The clones already existed under `.cache/` from the earlier corpus-prep task.
  The clone/fetch path is implemented and fails loudly, but was not exercised
  end to end against the network in this run (`--no-clone` was not used either).
- The SCIP files are the pinned artifacts from that prep task, except the
  full-tree Django index, which was regenerated for #57 with
  `--max-old-space-size=4096` (recorded in
  `docs/adapters.md` and the producer table above). Producer versions/sizes are
  recorded in the matrix. Full-tree Django accuracy was not measured through
  `TestSCIPAccuracy`; the adapter before/after counts above are. `hyusk` uses a
  default-feature Rust index.
- Accuracy is sampled (20 targets per language) and scope-matched, and the
  excluded-fact counts above are part of the result. It is a measurement of this
  graph against this compiler index, not a general correctness guarantee.
- `path` has no ground truth and is not scored; `impact` depth 2 is a
  compiler-graph approximation with truncation counted, not a proof.
- The failure-mode corrupt-SCIP exit code is **1**, not the issue's suggested
  `2`; that discrepancy is documented above rather than silently changed.

## Rust generic bounds, workspace imports, and module layout (2026-11-17, #55)

Closes the static Rust gaps the ladder stopped at: generic bounds, local
workspace cross-crate `use` paths, `#[path]` and inline `mod` import targets,
trait default methods through persisted `IMPLEMENTS` links, same-file return and
associated-type receiver typing, and Rust 2018 uniform paths. Macro expansion,
proc macros, generic containers, and unknown projections remain out of scope.

### Corpora and method

Private copies of the pinned #48/#61 clones, with `.repath/` and
`.git/repath` removed before every cold run:

| Corpus | Commit | Files | Bytes of Rust | Baseline index |
| --- | --- | --- | --- | --- |
| `rayon-rs/rayon` | `599425d` | 191 | 34,227 production + 2,102 test lines | 3,402 nodes, 2,161 edges, 2,048 pending, 0 parse failures |
| `Goutam-11/hyusk` | `06bdb2b` | 88 | 49,126 LOC, 750-crate workspace | 2,428 nodes, 2,706 edges, 4,222 pending, 7 parse-failed files |

"Before" is `main` = `2659149` (`#66`); "after" is the #55 branch. Each number is
one cold `index --full` row set, cross-checked against the ten-file comment edit
and a second `--full` (below). Host was shared: load average **2.5–4.8** during
the runs (`uptime` sampled before and after each group), so timings are
process-noise dominated but well inside the synthetic budgets.

### Results

| Metric | rayon before | rayon after | hyusk before | hyusk after |
| --- | --- | --- | --- | --- |
| edges | 2,161 | **2,524** (+363, +16.8%) | 2,706 | **2,992** (+286, +10.6%) |
| pending | 2,048 (48.66%) | **1,394 (35.58%)** | 4,222 (60.94%) | **4,126 (57.97%)** |
| `member-scope` | 1,357 | 786 | 911 | 901 |
| `missing-definition` | 510 | 479 | 2,332 | 2,286 |
| `scope-gap` | 181 | 129 | 244 | 237 |
| `parse-failure` | 0 | 0 | 735 | 702 |
| `ambiguous` rows (`ambiguity_rate`) | 75 (3.47%) | 74 (2.93%) | 23 (0.85%) | 23 (0.77%) |
| new edges from #55 rules | — | 239 | — | 83 |

Rayon's 239 new-rule edges: `rs.generic.bound` **224**, `rs.trait.method` **7**,
`rs.typed.method.return` **7**, `rs.typed.method.crate` **1**. Hyusk's 83:
`rs.typed.method.return` **81**, `rs.module.path.attr` **3**, `rs.generic.bound`
**2** (it is a `#[path]`-heavy, generic-light tree). The rest of the edge growth
is reclassification through the improved module path: on hyusk `rs.local`
1,337 → 1,520, `rs.assoc.fn` 208 → 388, `rs.use.reexport` 0 → 21,
`rs.module.path.attr` 0 → 3, with `rs.global.unique` 210 → 28 — the same
references answered by scope instead of a repo-wide unique-name guess. Rayon
moves 171 → 205 `rs.use.glob`, 44 → 100 `rs.use.reexport`, 49 → 65
`rs.module.path`, and 7 → 7 `rs.typed.method.global`.

Edges by rule, after (rayon / hyusk): `rs.local` 851/1,520,
`rs.assoc.fn` 343/388, `rs.global.unique` 233/28, `rs.generic.bound` 224/2,
`rs.use.glob` 205/1, `rs.self.method` 194/175, `rs.typed.method` 126/114,
`rs.use.reexport` 100/21, `rs.module.path` 65/199, `rs.global.ambiguous` 53/2,
`rs.use.named` 45/48, `rs.typed.method.return` 7/81, `rs.trait.method` 7/0,
`rs.typed.method.crate` 1/0, `rs.module.path.attr` 0/3,
`rs.typed.method.ambiguous` 17/0, `rs.assoc.global.ambiguous` 4/0,
`rs.enum.variant` 3/56.

Ambiguity stays visible and is never collapsed: `status.ambiguous` counts the
0.40 rows and `ambiguity_rate` is emitted on both corpora (rayon 74 rows: 53
`rs.global.ambiguous`, 17 `rs.typed.method.ambiguous`, 4
`rs.assoc.global.ambiguous`; hyusk 23: 21 `py.global.ambiguous`, 2
`rs.global.ambiguous`). No first-candidate pick was introduced; the rate fell
only because resolved groups replaced ambiguous ones.

### Residual share (macro / external)

The graph does not persist external references, so "external" cannot be counted
directly; the residual is the pending buckets plus the ambiguity rows. On
rayon the 1,394 pending are 56% `member-scope`, 34%
`missing-definition`, 9% `scope-gap`; the top missing names (`add`, `install`,
`spawn`, `len`, `iter`, `complete`, `into_par_iter`) are macro-generated
internals and dependency collection/thread methods. On hyusk the 4,126 are 55%
`missing-definition`, 22% `member-scope`, 17% `parse-failure` (the same seven
files as before; the pinned hyusk tree includes files the grammar rejects), 6%
`scope-gap`. Macro bodies, proc macros, and `impl Trait`/generic-container
method calls remain the dominant honest ceiling.

### Parity (#49)

Cold, a ten-file comment edit, and a second `index --full` produce **identical
edge and pending row sets** on both corpora — comparing file path, qname, kind,
confidence, rule, line, and the ambiguous candidate id list. The Rust
`IMPLEMENTS` links used for trait default methods are resolved in a pre-pass and
persisted like an edge; a warm run loads the links for unchanged types, exactly
like Python's base-class links, so a partial re-index cannot stop at the first
unchanged type. An incremental run whose changed file is re-resolved also
corrects a pre-existing parity hole: a name imported inside an inline test
module used to shadow a same-file definition, leaving a pending row in the warm
graph while the cold run's retry produced a 0.60 edge. Resolution now falls
through to the local ladder when a binding names a module but not the symbol.

### Timing (shared host, load 2.5–4.8)

| Corpus | Cold (3 runs) | 10-file incremental | second `--full` | Cold RSS |
| --- | --- | --- | --- | --- |
| rayon | 0.36 / 0.38 / 0.38 s | **0.06 s** | 0.36 s | 46–48 MB |
| hyusk | 0.41 / 0.42 / 0.43 s | **0.14 s** | 0.38 s | 68–72 MB |

No regression against the baseline (rayon 0.39 s cold, hyusk 0.47 s cold in the
same window); the new crate-manifest read is per local crate (4 on rayon, 1
indexed crate on hyusk), and the extra `IMPLEMENTS` read only happens on a
warm Rust run.

### Adapter gap (unchanged adapter, #57 scope)

Applying the pinned `rust-analyzer` 1.97.0 files to the after graph:

| Corpus | resolver pending | after SCIP | SCIP edges | ambiguous groups replaced |
| --- | --- | --- | --- | --- |
| rayon | 1,394 (35.58%) | **1,156 (29.98%)** | 215 | 16 (39 candidate rows) |
| hyusk | 4,126 (57.97%) | **3,998 (56.62%)** | 71 | 0 |

The adapter still answers what the static ladder cannot (macro-expanded
methods, projections, third-party trait impls) and still never overwrites a
resolved edge. #55 reduces the adapter's *remaining* gap on rayon because the
generic-bound and trait-default calls it used to fill now have tree-sitter
answers; no adapter code changed in this PR.

### Guards

`make check` clean, `go test ./internal/eval -v` at Rust **55/55** edges,
precision 1.00 / recall 1.00, 0 unresolved and 3 ambiguous groups (TS 9/9,
Python 65/65 unchanged), the Rust gate stays 0.85/0.70 with `maxUnresolved` 2,
`make perf` green, and `repath check --fail-on none` exits 0. No schema,
node-kind, exit-code, rule-rename, fixture-loosening, TS/Python-resolver, or Go
dependency change.

### Not verified

- One host window; cold timings vary with load. The corpora are private copies
  under `.cache/`, not committed.
- The same-file inherent-vs-trait-impl case cannot show an ambiguity row because
  node identity is `(qname, file, arity)`; that is a documented schema-level
  residual, not a resolver pick. Cross-file and two-trait default-method
  ambiguity are fixture-gated.
- `#[path]` modules are resolved through the declaring file's directory and
  inline prefix exactly as rustc does in the two tested shapes; directory-valued
  `#[path]` targets and `include!` are not modelled.
- Workspace member names come from local Cargo.toml files only; dependency
  renames (`package = "..."`), `pnpm`-style metadata, and registries are not read.
- External references are not persisted, so the "external share" is reported as
  the pending buckets plus ambiguity, not as a separate counter.
- The `IMPLEMENTS` pre-pass adds one `IMPLEMENTS` edge read on warm Rust runs,
  but its effect on very large Rust workspaces (thousands of crates) was not
  measured; hyusk indexes only the root crate.

## LSP facts (2026-11-18, #58)

Issue #58 asked whether LSP can supply compiler facts where SCIP does not fit.
The approved decision is a **facts file**: an external LSP client speaks
JSON-RPC to a server and writes a versioned JSON file;
`repath adapter --lsp-facts FILE` ingests it. repath never starts a server, CI
needs no server, and the fixture is the gate. The format, the occurrence-count
guard, and the merge policy are in
[`docs/adapters.md`](adapters.md#lsp-facts-opt-in).

### Pinned producers and commands

| Role | Pinned version | Command |
| --- | --- | --- |
| TypeScript server | `typescript-language-server` 6.1.0, TypeScript 5.9.3, Node 24.18.0 | `typescript-language-server --stdio` with `--init-options '{"tsserver":{"path":"…/typescript/lib/tsserver.js"}}'` |
| Python server | `pyright-langserver` 1.1.414, Node 24.18.0 | `pyright-langserver --stdio` |
| Producer | `scripts/lsp-facts.py` (`repath-lsp-facts/1`) | `python3 scripts/lsp-facts.py --lang <ts\|python> --root <clone> --server "<cmd>" --out facts.json` |

TypeScript 7.0.2 is the native port and does not ship `lib/tsserver.js`, so the
language server is pinned to TypeScript 5.9.3. The producer is opt-in evidence
tooling; neither server is a build dependency and `make check` runs neither.

### Results (single run per corpus, shared host)

The tree-sitter graph is the same for the LSP and SCIP passes; the adapter is
applied to a fresh cold index. All numbers are measured on this host
(load ~1.5–3.5 during the window), not medians.

| Corpus | Producer wall / peak RSS / facts | Ingest wall / RSS | Pending before → after | Matched | New edges | Replaced groups / rows |
| --- | --- | --- | --- | --- | --- | --- |
| `microsoft/vscode-uri` @ `8b86482` (8 files) | **2.52 s** / **79 MiB** / 808 KB (677 defs, 3,801 refs) | **0.15 s** / 19 MiB | **91 → 74** | 17 | 14 | 0 / 0 |
| `psf/requests` @ `3c7baaa` (37 files), full scope | **37.7 s** / **640 MiB** / 1.9 MB (3,071 defs, 7,396 refs) | **1.01 s** / 25 MiB | **543 → 449** | 94 | 84 | 6 / 18 |
| `psf/requests`, pyright facts filtered to `src/` | (same facts) | — | **543 → 522** | 21 | 19 | 0 / 0 |

Comparison with the pinned SCIP runs on the same graphs:

- `vscode-uri` with `scip-typescript` 0.6.2: pending **91 → 69**, 18 edges,
  22 matched (`docs/adapters.md`). LSP is slightly behind on this 8-file repo.
- `requests` with `scip-python` 0.6.8 on the package (`src/`, 19 documents):
  pending **543 → 523**, 18 edges, 20 matched — effectively a wash with the
  scope-matched LSP row (522 / 19). The full-scope LSP number is better only
  because pyright analyses the tests that `scip-python` does not emit; 65 of the
  84 new edges have a test-path source, 19 are under `src/`. That is a coverage
  difference, not an LSP quality claim.
- Producer cost does not favour LSP: on `requests`, pyright is **37.7 s /
  640 MiB** against `scip-python` 0.6.8 at **4.78 s / 400 MiB** (989 KB index).
  The edge (and its `0.40` group replacements) is real; the transport is not
  cheaper. The value is that a server without a SCIP producer can contribute.

The fixture (`testdata/fixtures/lsp-adapter`) pins the two same-line outcomes:
two local targets on one line merge into two exact `lsp.ts.method` edges
(pending **4 → 2**, 10 definitions / 13 references, 5 skipped, 6 unmatched), and
the local + external pair is refused so both rows stay pending.

### Guards

`make check` is clean, `go test ./internal/eval -v` is unchanged (TS 9/9,
Python 65/65, Rust 55/55 at precision 1.00 / recall 1.00), and `make perf` was
rerun for the branch. No schema, node-kind, exit-code, fixture-gate, rule
rename, or Go dependency change; no producer runs in CI.

### Not verified

- One host and one run per corpus; producer/ingest timings are machine-dependent
  and not medians. The servers are pinned to this host's install.
- pyright analyses statically and the `requests` clone has no installed
  third-party dependencies, so external calls stay external; local resolution is
  unaffected. A workspace with a real venv may resolve more.
- The `scip-python` index is scoped to the package (`src/`), so the full-scope
  LSP row is not like-for-like; the `src/`-filtered row is.
- No large Python tree (Django, 3,051 files) was measured through LSP; pyright
  producer cost and memory at that scale are unknown.
- No Rust LSP run: `rust-analyzer scip` already covers Rust and is the better
  producer there. `path` still has no ground truth.
- The committed fixture is the only CI gate; the producer and the corpus rows
  remain opt-in evidence.
