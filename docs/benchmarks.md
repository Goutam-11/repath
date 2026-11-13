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
| New edges | **18** | **749** |
| Conflicts | 0 | 0 |
| Skipped / unmatched | 6,305 / 945 | 55,898 / 9,428 |
| Unresolved | **543 → 523** | **2,048 → 1,214** |
| Unresolved rate | **34.50% → 33.27%** | **48.66% → 29.44%** |
| Edges | 1,031 → 1,049 | 2,161 → 2,910 |
| Adapter wall | 241 ms (160–249) | 1.264 s (0.681–1.313) |

Python's 18 new edges are all `scip.py.method` from `src/` (0 test): real
inherited/mixin calls such as `HTTPDigestAuth.__call__ ->
RequestHooksMixin.register_hook`, `JSONDecodeError.__init__ ->
RequestException.__init__`, and `PreparedRequest.copy ->
CaseInsensitiveDict.copy`. `scip-python` indexed `src/` and not `tests/`, so
this run cannot resolve references in test files; that is the producer's scope,
not a filter (see [`docs/adapters.md`](adapters.md)).

Rust's 749 new edges are **640** `scip.rs.method`, **92** `scip.rs.call`, and
**17** `scip.rs.new`. By the source symbol's file:

| Source | Edges | Share |
| --- | --- | --- |
| Library production (`rayon` 629, `rayon-core` 8) | **637** | 85.0% |
| Inline `#[test]` functions | **32** | 4.3% |
| `tests/` integration files | **27** | 3.6% |
| `rayon-demo` examples/benchmarks | **53** | 7.1% |

So **85.0%** of the new edges are library production and **92.1%** are
non-test. This is the production/test split #48 asked for, and it reverses the
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
| SCIP producer | `scip-python` 0.6.8, production-only 30.3 MiB | `rust-analyzer` 1.97.0, 7.1 MiB in 157.7 s |

`hyusk` is a production agent workspace with a large inline test surface; the
14 JavaScript and 6 Python files are tooling/scripts, so the Rust surface is
the 68 `.rs` files. Django is deliberately test-heavy: 2,009 test modules vs
906 production modules. Query sampling excludes test paths, so its
`impact`/`callers`/`path` rows are production-symbol measurements; the cold,
incremental, and artifact rows cover the whole tree.

### Command

```sh
# Python corpus. scip-python 0.6.8 OOMs on the full 2,941-file tree at Node's
# default heap, so the adapter half indexes the production package only.
git clone --depth 1 https://github.com/django/django /tmp/django
git -C /tmp/django checkout f24f38c6b2825f15628c07bf2892f1664eed9361
NODE_OPTIONS=--max-old-space-size=3072 \
  scip-python index --project-name django --cwd /tmp/django \
  --target-only django --output /tmp/django-prod.scip

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

The producers are not part of repath or CI. The Python file is a
production-package index (909 files), not the whole repository; the Rust file
covers the workspace root.

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
| New edges | **1,669** | **88** |
| Conflicts | 10 | 2 |
| Skipped / unmatched | 142,241 / 30,565 | 63,242 / 10,208 |
| Unresolved | **105,319 → 103,306** | **4,222 → 4,081** |
| Unresolved rate | **69.68% → 68.50%** | **60.94% → 59.36%** |
| Edges | 45,834 → 47,503 | 2,706 → 2,794 |
| Adapter wall | 5.72 s | 0.62 s |

Django's 1,669 new edges are **1,627** `scip.py.method`, **32**
`scip.py.call`, and **10** `scip.py.reference`; all source files are under
`django/`. The harness classifies **79** of them as test-path because Django's
production `django/test/` package is the test client/framework and
`testpath.IsTest` treats `*/test/*` as test code — those are not the test
suite. `hyusk`'s 88 edges are 66 method and 22 call; **54** have a non-test
source and **34** are `#[test]` entrypoints, all under `src/`.

### Finding: producer scale limits

`scip-python` 0.6.8 **crashes on the full Django tree**: after parsing 2,931
of 2,941 files it aborts with `FATAL ERROR: Ineffective mark-compacts near heap
limit - JavaScript heap out of memory` at Node's ~2 GB default heap. The
production-only run (909 files) succeeds under
`NODE_OPTIONS=--max-old-space-size=3072` and produces a 30.3 MiB index in about
two minutes; that is the index used above. The adapter is therefore not
usable on a full large Python repository with this producer version without
raising Node's heap, and even then the whole tree was not measured. This is a
producer limit, not a repath crash, and it is recorded rather than hidden.

`rust-analyzer` 1.97.0 does finish `hyusk` (750 crates, 157.7 s, 7.1 MiB), but
the adapter gain is modest: only 141 of 65,165 references match a pending
tree-sitter row, because most references are external, standard-library, or
already resolved. The production-heavy split (54 of 88) is the useful signal;
this is not a repeat of `rayon`'s generic-bound corpus.

### Gaps

- Django is a Python framework with a huge test suite; a production-only
  application corpus (roughly 500–1,500 files) would separate "framework
  patterns" from "application code" in the unresolved rate.
- The full-tree Python adapter is not measured because 0.6.8 OOMs; a producer
  upgrade or a bounded heap run on a mid-size repo is needed before claiming
  large-tree adapter coverage.
- `hyusk` has 7 parse failures among its non-Rust files; they were not
  audited. The Rust-only matrix is the evidence, and the JS/Python scripts are
  tooling, not the corpus.
- One host, warm page cache, three runs; no CPU pinning or multi-machine
  replication. `check` stopped at the 200-finding cap for both trees.
- No `impact`/`callers`/`path` ground-truth spot-check against a compiler/LSP
  was performed on these corpora.
