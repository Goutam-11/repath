# Real-repository benchmark

Issue #31 asked for the 50k/zstd claims to be validated on a real repository,
not only the synthetic one-line-function tree. This page records the pinned
repository, the reproducible command, and the honest numbers — including the
budgets that were missed. It is **evidence, not a gate**: `make perf` remains
the opt-in synthetic budget run, and neither target is part of CI.

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
Go 1.27, repath at the v1.2 branch. Peak RSS is `/proc/self/status` `VmHWM`.

| Metric | Result | PLAN §10 target | Status |
| --- | --- | --- | --- |
| Cold index (38,548 files) | **4 min 29.1 s** | < 3 min (50k) | **miss** |
| Peak RSS (cold) | **853.3 MiB** | < 1 GiB | pass |
| 10-file incremental | **4.81 s** | < 1 s | **miss** |
| Peak RSS (all measurements) | **986.8 MiB** | < 1 GiB | pass |
| `impact` p50 / p95 | **84.4 / 118.9 ms** | < 150 ms p95 | pass |
| `callers` p50 / p95 | **58.1 / 73.0 ms** | < 150 ms p95 | pass |
| `path` p50 / p95 | **85.2 / 130.1 ms** | < 250 ms p95 | pass |
| `map --max-tokens 2000` p50 / p95 | **398.5 / 455.6 ms** | < 300 ms p95 | **miss** |
| `check` | **848.5 ms** (200 findings, capped) | no target | — |
| Artifact uncompressed | **65.6 MiB** | no target | — |
| Artifact export / pull | **407.7 ms / 40.6 ms** | no target | — |
| Artifact zstd | **17.2 MiB** (26.3%) | no target | — |
| zstd export / pull | **658.6 ms / 125.8 ms** | no target | — |

Sample sizes: 20 deterministic `impact`/`callers` samples and 17 `path`
samples over symbols with both callers and callees; 5 `map` samples. The first
query of each kind is included, so these are warm-index, cold-process numbers
(no query cache is warmed deliberately).

## What the misses mean

- **Cold is 1.5× over the 3-minute target.** Real files average ~11 KiB with
  deep TypeScript syntax trees; the synthetic tree is one-line functions. The
  cost is parsing plus 61,894-edge resolution, not discovery (38k `stat` calls).
  This issue did not optimize the pipeline; the honest result is a miss.
- **10-file incremental is 4.8× over the 1-second target.** Re-indexing ten
  deep files also re-runs the pending/edge merge for the graph. The synthetic
  run (709 ms at 50k) stays green; the real-tree number is new evidence.
- **`map` p95 is 1.5× over 300 ms.** The real graph is denser per module than
  the synthetic chain; `BuildMap`'s PageRank/cycle work scales with the module
  graph. No optimization was attempted in a validation-only issue.
- **RSS passed narrowly.** 853 MiB cold and 987 MiB after artifact work is
  under 1 GiB, but there is little headroom on a 7 GiB machine with other
  workloads. Re-run on a quiet machine before treating it as a hard ceiling.

These misses do **not** change the synthetic gates: `make perf` still reports
10k cold **6.0 s**, 10k incremental **247 ms**, 50k cold **15.2 s**, 50k
incremental **709 ms**, 50k `map` p95 **245.7 ms**, and 25.2% zstd on both
synthetic trees. PLAN §12 keeps the synthetic table as the gate and records the
real-repo numbers as separate evidence.

## Artifact size correction

The old PLAN §4.1 estimate of 250–400 MB at 50k files was **wrong by roughly
4×**. The real 38,548-file TypeScript artifact is 65.6 MiB uncompressed and
17.2 MiB zstd (26.3%, essentially the synthetic 25.2%). The artifact holds
compact symbol rows with no source text, so file count alone overstated the
size. The estimate is corrected in PLAN §4.1 and `docs/sharing.md`.

## Not verified

- The full Kibana tree (114,967 TS/JS files) was **not** indexed: it is outside
  the 100k+ out-of-scope boundary. The `x-pack/solutions` subset is the pinned
  shape.
- Only the TypeScript/JavaScript family is measured on a real repo here. Python
  and Rust real-repo runs remain future work.
- One Linux machine, one run. Page cache was not dropped; cold numbers are
  cold-process, not cold-page-cache.
- `check` stopped at the 200-finding cap; the 200 findings were not audited for
  true/false positives.
- Artifact pull into a dirty real checkout (overlay cost) was not measured.
