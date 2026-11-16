# Adapters: compiler facts over tree-sitter

repath's tree-sitter graph is the zero-setup default. Its documented ceiling is
member calls without a typed receiver: `cart.total()` only resolves when the
receiver type is known from an annotation, `new`, a constructor, or a parameter
([`docs/resolution.md`](resolution.md)). Python has a second gap: inheritance is
not walked, so a method inherited from a base stays unresolved. The adapter seam
lets a compiler supply those facts without replacing the graph.

Three adapters consume **SCIP**, the Sourcegraph Code Intelligence Protocol index:

| Producer | Documents | Adapter name | Rules |
| --- | --- | --- | --- |
| `scip-typescript` | `.ts`, `.tsx`, `.js`, `.jsx`, `.mjs`, `.cjs` | `scip` | `scip.ts.*` |
| `scip-python` | `.py` | `scip-python` | `scip.py.*` |
| `rust-analyzer scip` | `.rs` | `scip-rust` | `scip.rs.*` |

repath does not bundle SCIP, TypeScript, Node, Python, Rust, or a language
server: it parses the `.scip` protobuf itself, merges the facts that tree-sitter
could not resolve, and may replace a tree-sitter **ambiguous** candidate group
with the compiler's exact target (see
[Ambiguous replacement](#ambiguous-replacement)). Every producer is an external
tool, not a Go dependency.

## Setup

```sh
# TypeScript/JavaScript, in a project with a tsconfig.json:
npx --yes --package=@sourcegraph/scip-typescript@0.6.2 \
  scip-typescript index --output index.scip

# Python, from the project root (scip-python 0.6.8 measured here):
npx --yes --package=@sourcegraph/scip-python@0.6.8 \
  scip-python index --project-name myproject --output index.scip

# Python, full tree that OOMs at the default Node heap (measured on Django):
NODE_OPTIONS=--max-old-space-size=4096 \
  scip-python index --project-name myproject --output index.scip

# Rust, from the crate/workspace root (rust-analyzer 1.97.0 measured here):
rust-analyzer scip --output index.scip .

# Build the normal graph first (or let the adapter refresh it):
repath status --json
repath adapter --scip index.scip --json
repath status --json          # adapters[] now carries the run's coverage
```

`repath adapter` refreshes the index like `status` and `check`, then
auto-detects the producer from the SCIP documents. A TypeScript index is stored
under `adapter.scip`; Python under `adapter.scip-python`; Rust under
`adapter.scip-rust`, so `status.adapters[]` distinguishes the runs. A mixed
index (several producers in one file) is applied per language and reports one
entry each; that shape is unusual because the producers normally run
separately.

If the SCIP file is rooted at a subproject (for example `src/tsconfig.json`),
SCIP paths such as `charCode.ts` are matched to the repo's `src/charCode.ts` by
a unique path suffix. An exact repo-relative match wins; an ambiguous suffix is
skipped rather than guessed.

## What gets merged

The adapter reads per-document occurrences. A definition occurrence gives a
symbol string and a file/line; a reference occurrence gives the same symbol plus
its own file/line. The merge is deliberately narrow:

- **Pending references are the first candidate class.** A pending row is
  matched by **file + line + name**. The index does not store reference
  columns, so two same-named calls on one line cannot be distinguished; that is
  a documented approximation, not a compiler-grade guarantee.
- **A tree-sitter ambiguous group may be replaced** when the compiler's
  occurrence maps to the same source node and line and names one of that
  group's candidates. This is the second, narrow candidate class; the exact
  rules are below.
- If tree-sitter already resolved a reference at `>= 0.5`, the adapter leaves
  it alone. The replacement path only ever deletes candidate rows of one
  ambiguous group; it cannot remove an import/`this`/local/typed edge.
- The target node comes from the SCIP definition at its file/line; the source
  node is the tree-sitter symbol enclosing the reference line.
- Matched pending rows are removed, so `unresolved` drops immediately.
- Adapter edges use a distinct rule id and confidence `0.99`:

| Rule | Language | When |
| --- | --- | --- |
| `scip.ts.method` | TS/JS | the pending reference was a member/`this` call |
| `scip.ts.call` | TS/JS | a bare identifier call |
| `scip.ts.new` | TS/JS | constructor-like reference |
| `scip.ts.reference` | TS/JS | any other pending reference |
| `scip.py.method` | Python | a member/`self` call, including an inherited or factory-typed method |
| `scip.py.call` | Python | a bare identifier call |
| `scip.py.new` | Python | constructor-like reference (reserved: the Python extractor currently emits `call`) |
| `scip.py.reference` | Python | any other pending reference |
| `scip.rs.method` | Rust | a member/`self` call, including a generic-bound or trait-object method |
| `scip.rs.call` | Rust | a bare identifier call |
| `scip.rs.new` | Rust | constructor-like reference |
| `scip.rs.reference` | Rust | any other pending reference |

## Ambiguous replacement

Tree-sitter emits an ambiguous reference as one edge per same-name candidate,
2-5 of them, at confidence `0.40` with a rule ending in `.ambiguous` (for
example `py.global.ambiguous`). The whole group shares source node, reference
line, edge kind, rule, and the `candidates` id list stored on each row. The
adapter may replace **that group only** with the compiler's exact target:

1. A SCIP definition occurrence gives the exact target node (the same
   `EnclosingNode` mapping as the pending path).
2. A SCIP reference occurrence gives the source node enclosing its file+line.
   The group at that source node and line is a candidate if the target id is
   one of its candidates.
3. Exactly **one** SCIP reference occurrence must map to the group. If two
   occurrences map to it — the same-line, same-name case the schema cannot
   distinguish — the group is left untouched and the references are counted as
   unmatched. The adapter never merges two references.
4. `store.ReplaceAmbiguousGroup` runs in one transaction: it deletes rows only
   when `src`, `line`, `kind`, `confidence < 0.5`, rule suffix `.ambiguous`,
   and `dst IN (candidates)` all match, then inserts the exact edge at `0.99`
   with the visible `scip.<lang>.*` rule. A resolved row at the exact key is
   reported as a conflict and nothing is deleted or overwritten.
5. The run counts every replacement: `replaced` groups and `replaced_edges`
   candidate rows in the adapter stats (persisted in `meta`, surfaced in
   `status.adapters`). A group the compiler cannot disambiguate stays ambiguous
   and is visible in `status.ambiguous`.

The rule id of the replacement follows the same selector as the pending path:
an ambiguous method rule (`rs.typed.method.ambiguous`) becomes
`scip.rs.method`; an edge kind of `CALLS` becomes `scip.<lang>.call`; other
kinds become `scip.<lang>.reference`. The schema stores the resolved edge kind,
not the original reference form, so a global ambiguous call whose candidates
are all classes is a `REFERENCES` edge and gets the `reference` rule.

## Merge rules and conflicts

`store.MergeEdges` inserts only when `(src, dst, kind)` is free and counts a
conflict otherwise. It never performs the upsert that tree-sitter edges use.
`store.ReplaceAmbiguousGroup` has the same rule for its exact edge: if a
resolved edge already owns the key it reports a conflict and changes nothing.
A conflict therefore means an edge already existed; the existing edge keeps its
rule and confidence, and the adapter reports the conflict instead of choosing a
winner.

## Status

`status --json` includes one object per adapter run:

```json
{
  "adapters": [
    {
      "adapter": "scip",
      "definitions": 544,
      "references": 2212,
      "matched": 22,
      "replaced": 0,
      "replaced_edges": 0,
      "edges": 18,
      "conflicts": 0,
      "skipped": 2050,
      "unmatched": 284,
      "last_run": "2026-11-12T14:20:15Z"
    },
    {
      "adapter": "scip-python",
      "definitions": 1485,
      "references": 6734,
      "matched": 20,
      "replaced": 0,
      "replaced_edges": 0,
      "edges": 18,
      "conflicts": 0,
      "skipped": 6305,
      "unmatched": 945,
      "last_run": "2026-11-12T16:21:43Z"
    },
    {
      "adapter": "scip-rust",
      "definitions": 5200,
      "references": 23264,
      "matched": 114,
      "replaced": 0,
      "replaced_edges": 0,
      "edges": 74,
      "conflicts": 1,
      "skipped": 22640,
      "unmatched": 4947,
      "last_run": "2026-11-13T11:02:46Z"
    }
  ]
}
```

- `definitions` / `references`: occurrences seen in the SCIP file.
- `matched`: pending references the adapter attached to a definition.
- `replaced`: tree-sitter ambiguous groups replaced by one exact compiler edge.
- `replaced_edges`: candidate rows deleted by those replacements.
- `edges`: newly inserted adapter edges (including replacement edges).
- `conflicts`: edges that already existed (never overwritten).
- `skipped`: occurrences with no usable graph mapping (module/local symbols,
  external definitions, or no enclosing source symbol).
- `unmatched`: references resolved by SCIP with no pending row and no
  replaceable ambiguous group (the normal case for code tree-sitter already
  handled, plus groups the compiler could not disambiguate).

## Producer matrix

Versions measured for this repository; none is a build dependency and CI never
runs a producer (the SCIP inputs are pinned fixtures). Times and sizes below
are from the corpora in [`docs/benchmarks.md`](benchmarks.md).

| Producer | Version | Output measured | Scope | Notes |
| --- | --- | --- | --- | --- |
| `scip-typescript` | 0.6.2 | 8-file `microsoft/vscode-uri` index | repo with `tsconfig.json` | smallest producer; fixture `testdata/fixtures/ts-adapter` |
| `scip-python` | 0.6.8 | 798 KiB (`requests`), 30.3 MiB (Django production 909 files), 100.3 MiB (Django full tree 2,941 `.py`) | project root | needs Python 3.12; 0.6.8 rejects Python 3.14; full Django tree needs `NODE_OPTIONS=--max-old-space-size=4096` |
| `rust-analyzer` | 1.97.0 | 7.4 MiB (`rayon`), 7.1 MiB (`hyusk`) | crate/workspace root | needs `cargo metadata` plus the crate's dependencies |

The pinned fixtures were produced externally and committed:
`testdata/fixtures/ts-adapter` (scip-typescript 0.6.2),
`testdata/fixtures/py-adapter` (scip-python 0.6.8), and
`testdata/fixtures/rs-adapter` (rust-analyzer 1.97.0). Regenerate them
deliberately when the producer or fixture source changes, and record the
producer version in the fixture README. The ambiguity tests build their SCIP
index in-process (`SCIPIndex`) instead of committing another producer file.

## Measured

- **TypeScript fixture** (`testdata/fixtures/ts-adapter`, `scip-typescript`
  0.6.2): `cart.total()` changes from pending to `run -> Cart.total`
  (`scip.ts.method`); unresolved **1 → 0**.
- **`microsoft/vscode-uri` @ `8b86482704a3c34a52b8c32e96a4bb142f4fdebe`**
  (8 files, 41 nodes): unresolved **91 → 69** (unresolved rate **81.25% →
  63.89%**), **18** adapter edges, **0** conflicts, **22** matched pending refs.
  The new edges are typed member calls such as `URI.parse`, `Utils.joinPath`, and
  `URI.toString`.
- **Python fixture** (`testdata/fixtures/py-adapter`, `scip-python` 0.6.8):
  two pending inherited/factory member calls (`cart.total()` in `run` and
  `run_factory`) become `scip.py.method` edges to `Base.total`; unresolved
  **2 → 0**. The run sees 12 definitions, 17 references, 2 matched, 2 edges,
  0 conflicts, 14 skipped, 5 unmatched.
- **`psf/requests` @ `3c7baaa05ab4aa49cc819ce3a819d260de19c157`** (37 indexed
  files, 787 nodes, 543 pending): unresolved **543 → 523** (unresolved rate
  **34.50% → 33.27%**) with **18** adapter edges, **20** matched pending refs,
  **0** replaced groups, **0** conflicts. The 25 ambiguous candidate rows
  (8 groups, 3–4 candidates each) were not named by this producer's references
  and stay ambiguous. The adapter wall time was **0.30 s** (0.30–0.32, three
  runs) and peak RSS **18.6 MiB** (18.2–18.7) on an 798 KiB SCIP file. The new
  edges are real inherited/mixin method calls such as
  `HTTPDigestAuth.__call__ -> RequestHooksMixin.register_hook`,
  `JSONDecodeError.__init__ -> RequestException.__init__`, and
  `PreparedRequest.copy -> CaseInsensitiveDict.copy`. Re-measured for #48 with
  the bench harness on the cold graph: the same 543 → 523, 18 edges, 20
  matched, 0 conflicts. A comment-only ten-file incremental now reproduces the
  cold graph (543 pending, 1,031 edges); before #49 it added five `urlparse`
  pending rows because a re-export chain through unchanged `compat.py` was not
  loaded during incremental resolution. See
  [`docs/benchmarks.md`](benchmarks.md).

- **Rust fixture** (`testdata/fixtures/rs-adapter`, `rust-analyzer` 1.97.0):
  two generic-bound calls (`fn run_generic<T: Total>` and
  `run_generic_again<T: Total>`) stay pending because repath does not model
  `T: Trait`. `rust-analyzer scip` resolves both to `scip.rs.method` edges to
  `Total.total`; unresolved **2 → 0**. The fixture's typed receivers
  (`run_cart(Cart)`, `run_dyn(dyn Total)`) already resolve as
  `rs.typed.method` and are left untouched. The gate is
  `TestRustSCIPAdapterGate`.
- **`serde-rs/json` @ `8c535a260ce561eaab4111aafc29038eb9f77ff3`** (72 indexed
  files, 1,676 nodes, 596 pending): unresolved **596 → 482** (rate **37.34% →
  30.98%**) with **74** new edges (**41** `scip.rs.method`, **31**
  `scip.rs.call`, **2** `scip.rs.new`), **114** matched pending references,
  **1** conflict, 0.31 s adapter wall time and 26 MiB peak RSS on a 2.9 MiB
  SCIP file produced in 18.5 s by `rust-analyzer` 1.97.0. The new edges are
  real generic-bound and test-helper calls such as `test_roundtrip_f64 →
  from_str`, `test_malicious_number → to_value`, and
  `error/impl#[Error][Error]custom()` targets. The single conflict is an edge
  that another reference in the same file had already resolved; it keeps its
  tree-sitter rule and is reported, never overwritten.
- **`rayon-rs/rayon` @ `599425d4a4c64714632817f48496dbc53f656a90`** (191
  indexed files, 3,402 nodes, 2,048 pending): the production-heavy corpus added
  for #48. Unresolved **2,048 → 1,214** (rate **48.66% → 29.44%**) with **766**
  new edges (**640** `scip.rs.method`, **105** `scip.rs.call`, **17**
  `scip.rs.new`, **4** `scip.rs.reference`), **834** matched pending
  references, **17 replaced ambiguous groups** (**41** candidate rows
  removed), **0** conflicts, **55,898** skipped, **9,411** unmatched, and
  **1.43 s** median adapter wall time (1.40–1.44, three runs; peak RSS
  **38.9 MiB**) on a 7.4 MiB SCIP file produced in 34.4 s by `rust-analyzer`
  1.97.0. Ambiguous rows fall **75 → 34** (groups **34 → 17**), so the
  remaining ambiguity is the genuine same-line/unknown-receiver residue, not a
  silent collapse. By the source symbol's file the new edges split **739**
  non-test (**96.5%**, including inline `#[test]` functions and the
  `rayon-demo` crate) and **27** test-path sources. Representative edges are
  `Callback.callback -> Consumer.split_at`, `BytesProducer.fold_with ->
  Folder.consume_iter`, and `BlocksCallback.callback -> Producer.split_at`.
  The full table and the production/test definition are in
  [`docs/benchmarks.md`](benchmarks.md).

- **`django/django` @ `f24f38c6b2825f15628c07bf2892f1664eed9361`** (3,051
  indexed files, 44,426 nodes, 105,319 pending) with the production-package
  SCIP index (909 documents, 30.3 MiB, `--target-only django`): unresolved
  **105,319 → 103,306** with **1,684** new edges (**1,627** `scip.py.method`,
  **47** `scip.py.call`, **10** `scip.py.reference`), **2,013** matched pending
  refs, **15 replaced ambiguous groups** (**32** candidate rows removed),
  **10** reported conflicts, and ambiguous rows **1,572 → 1,540** (groups
  **487 → 472**). Adapter wall time **6.08 s** median (6.07–6.26, three runs;
  peak RSS **128.7 MiB**); the wall includes the post-index refresh. By source
  file the 1,684 edges split **1,603 production / 81 test-path**.
- **Full-tree Django**: `scip-python` 0.6.8 with
  `NODE_OPTIONS=--max-old-space-size=4096` indexes all **2,941 `.py`** files in
  **2:08** (max RSS **3.74 GiB**, **100.3 MiB** SCIP, exit 0). Applying it to
  the same cold graph takes **33.1 s** (347.6 MiB RSS) and yields **169,269**
  definitions / **613,467** references: unresolved **105,319 → 82,510**,
  **22,809** matched pending refs, **16,903** new edges (**15,218**
  `scip.py.method`, **1,210** `scip.py.call`, **475** `scip.py.reference`),
  **295 replaced ambiguous groups** (**914** rows removed), **33** conflicts,
  ambiguous rows **1,572 → 658** (groups **487 → 192**), edges **45,834 →
  61,823**. The full index covers the test suite, so **15,150** of the 16,903
  edges have a test-path source and only **1,753** are production; the
  production-only file above is the useful application-code number. This is the
  documented large-tree mode; see [Producer scale](#producer-scale).

The tree-sitter-only fixture gates in `internal/eval` are unchanged and were
not loosened; the adapter-on gates are the separate `TestSCIPAdapterGate`,
`TestPythonSCIPAdapterGate`, and `TestRustSCIPAdapterGate`.

## Limits

- Python documents are only those `scip-python` emits. In the pinned `requests`
  run it indexed `src/` (19 files) and not `tests/`, so pending references in
  test files cannot be resolved by this adapter run. That is the producer's
  scope, not a silent filter.
- Rust documents are only those `rust-analyzer scip` emits from the
  crate/workspace root. It includes test targets, so test-helper calls resolve
  too; external crates and `std` symbols are not local definitions and stay
  skipped. In the #48 `rayon` run, 27 of the 766 new edges are under `tests/`,
  32 are inline `#[test]`, and 53 are `rayon-demo`; the 17 replacement edges are
  all non-test, so the production share is reported rather than assumed. Producing the index needs a Rust toolchain and
  the crate's dependencies available to `cargo metadata`, which is why the
  committed fixture is pinned and CI never runs the producer.
- Rust SCIP symbol strings are parsed by `rustSymbolName`: crate/module
  descriptors (`crate/`, `models/`) and `local` bindings are skipped; types
  (`Cart#`), terms (`run().`), enum variants (`Kind#A#`), fields
  (`Cart#count.`), and impl members (`impl#[Cart][Total]total().`) reduce to
  their display names. A new producer grammar may need a focused case; the
  parser is intentionally narrow rather than a general SCIP name resolver.
- SCIP must be produced externally. There is no embedded language server and no
  LSP transport yet.
- Reference matching by file+line+name is weaker than a column-exact match.
  Ambiguous replacement uses the same source-node + line mapping. Because the
  schema stores no reference column, two same-named calls on one line cannot be
  attributed; when two compiler occurrences map to one group the adapter leaves
  the group alone and counts the references, so it never invents a merge. A
  column-aware schedule would need a schema change and is not part of this
  policy.
- Dynamic Python (monkey patching, `getattr`, unannotated `Any`, runtime
  dispatch) remains invisible; the adapter lowers unresolved, it does not
  prove type correctness.
- The adapter adds edges and removes pending rows; it never adds node kinds,
  changes node identity, or rewrites a tree-sitter edge.
- Producer output is pinned by the committed fixture, not by a Go dependency.
  Regenerate deliberately when the producer or fixture changes.

## Producer scale

`scip-python` 0.6.8 is the only measured producer that needs a bounded mode:

| Mode | Command | Result |
| --- | --- | --- |
| full tree | `NODE_OPTIONS=--max-old-space-size=4096 scip-python index --project-name django --cwd <django> --output django-full.scip` | 2,941 `.py`, 2:08 wall, 3.74 GiB max RSS, 100.3 MiB index |
| bounded fallback | `NODE_OPTIONS=--max-old-space-size=3072 scip-python index --project-name django --cwd <django> --target-only django --output django-prod.scip` | 909 production files, 30.3 MiB index, 42.9 s, 1.77 GiB max RSS |

At Node's default heap the full tree aborts with `FATAL ERROR: Ineffective
mark-compacts near heap limit - JavaScript heap out of memory` after parsing
2,931 of 2,941 files. The bounded mode is the documented fallback for hosts
without ~4 GiB to give the producer; the full mode is what the large-tree run
above uses. `rust-analyzer` 1.97.0 finished both measured workspaces without
tuning (34.4 s for `rayon`, 157.7 s / 7.1 MiB index for the 750-crate `hyusk`),
and `scip-typescript` 0.6.2 needs no tuning on the measured repo.
