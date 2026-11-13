# Real-repository benchmark

Issue #31 asked for the 50k/zstd claims to be validated on a real repository,
not only the synthetic one-line-function tree. This page records the pinned
repository, the reproducible command, and the honest numbers. Issue #43 then
profiled and closed the three budgets that #31 missed; the before/after
evidence and the remaining caveats are below. It is **evidence, not a gate**:
`make perf` remains the opt-in synthetic budget run, and neither target is part
of CI.

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
- Python and Rust real-repo performance was not measured.
- `check` stopped at the 200-finding cap; the findings were not audited for
  true/false positives.
- Artifact pull into a dirty real checkout (overlay cost) was not measured.

## Artifact size correction

The old PLAN §4.1 estimate of 250–400 MB at 50k files was **wrong by roughly
4×**. The real 38,548-file TypeScript artifact is 65.6 MiB uncompressed and
17.2 MiB zstd (26.2%, essentially the synthetic 25.4%). The artifact holds
compact symbol rows with no source text, so file count alone overstated the
size. The estimate is corrected in PLAN §4.1 and `docs/sharing.md`.
