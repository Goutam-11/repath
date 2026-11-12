# Adapters: compiler facts over tree-sitter

repath's tree-sitter graph is the zero-setup default. Its documented ceiling is
member calls without a typed receiver: `cart.total()` only resolves when the
receiver type is known from an annotation, `new`, a constructor, or a parameter
([`docs/resolution.md`](resolution.md)). Python has a second gap: inheritance is
not walked, so a method inherited from a base stays unresolved. The adapter seam
lets a compiler supply those facts without replacing the graph.

Two adapters consume **SCIP**, the Sourcegraph Code Intelligence Protocol index:

| Producer | Documents | Adapter name | Rules |
| --- | --- | --- | --- |
| `scip-typescript` | `.ts`, `.tsx`, `.js`, `.jsx`, `.mjs`, `.cjs` | `scip` | `scip.ts.*` |
| `scip-python` | `.py` | `scip-python` | `scip.py.*` |

repath does not bundle SCIP, TypeScript, Node, Python, or a language server: it
parses the `.scip` protobuf itself and merges only the facts that tree-sitter
could not resolve. Both producers are external tools, not Go dependencies.

## Setup

```sh
# TypeScript/JavaScript, in a project with a tsconfig.json:
npx --yes --package=@sourcegraph/scip-typescript@0.6.2 \
  scip-typescript index --output index.scip

# Python, from the project root (scip-python 0.6.8 measured here):
npx --yes --package=@sourcegraph/scip-python@0.6.8 \
  scip-python index --project-name myproject --output index.scip

# Build the normal graph first (or let the adapter refresh it):
repath status --json
repath adapter --scip index.scip --json
repath status --json          # adapters[] now carries the run's coverage
```

`repath adapter` refreshes the index like `status` and `check`, then
auto-detects the producer from the SCIP documents. A TypeScript index is stored
under `adapter.scip`; a Python index is stored under `adapter.scip-python`, so
`status.adapters[]` distinguishes the runs. A mixed index (both producers in one
file) is applied per language and reports both entries; that shape is unusual
because the producers normally run separately.

If the SCIP file is rooted at a subproject (for example `src/tsconfig.json`),
SCIP paths such as `charCode.ts` are matched to the repo's `src/charCode.ts` by
a unique path suffix. An exact repo-relative match wins; an ambiguous suffix is
skipped rather than guessed.

## What gets merged

The adapter reads per-document occurrences. A definition occurrence gives a
symbol string and a file/line; a reference occurrence gives the same symbol plus
its own file/line. The merge is deliberately narrow:

- **Only pending references are candidates.** If tree-sitter already resolved a
  reference, the adapter leaves it alone. This is what makes overwriting
  impossible in the normal path.
- A pending row is matched by **file + line + name**. The index does not store
  reference columns, so two same-named calls on one line cannot be distinguished;
  that is a documented approximation, not a compiler-grade guarantee.
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

## Merge rules and conflicts

`store.MergeEdges` inserts only when `(src, dst, kind)` is free and counts a
conflict otherwise. It never performs the upsert that tree-sitter edges use.
A conflict therefore means an edge already existed; the existing edge keeps its
rule and confidence, and the adapter reports the conflict instead of choosing a
winner. In the normal path there are no conflicts because only pending rows are
candidates.

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
      "edges": 18,
      "conflicts": 0,
      "skipped": 6305,
      "unmatched": 945,
      "last_run": "2026-11-12T16:21:43Z"
    }
  ]
}
```

- `definitions` / `references`: occurrences seen in the SCIP file.
- `matched`: pending references the adapter attached to a definition.
- `edges`: newly inserted adapter edges.
- `conflicts`: edges that already existed (never overwritten).
- `skipped`: occurrences with no usable graph mapping (module/local symbols,
  external definitions, or no enclosing source symbol).
- `unmatched`: references resolved by SCIP but with no pending tree-sitter row
  (the normal case for code tree-sitter already handled).

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
  **0** conflicts. The adapter wall time was **0.27 s** (peak RSS 18 MiB) on an
  798 KiB SCIP file. The new edges are real inherited/mixin method calls such as
  `HTTPDigestAuth.__call__ -> RequestHooksMixin.register_hook`,
  `JSONDecodeError.__init__ -> RequestException.__init__`, and
  `PreparedRequest.copy -> CaseInsensitiveDict.copy`.

The tree-sitter-only fixture gates in `internal/eval` are unchanged and were
not loosened; the adapter-on gates are the separate `TestSCIPAdapterGate` and
`TestPythonSCIPAdapterGate`.

## Limits

- Python documents are only those `scip-python` emits. In the pinned `requests`
  run it indexed `src/` (19 files) and not `tests/`, so pending references in
  test files cannot be resolved by this adapter run. That is the producer's
  scope, not a silent filter.
- SCIP must be produced externally. There is no embedded language server and no
  LSP transport yet.
- Reference matching by file+line+name is weaker than a column-exact match.
- Dynamic Python (monkey patching, `getattr`, unannotated `Any`, runtime
  dispatch) remains invisible; the adapter lowers unresolved, it does not
  prove type correctness.
- The adapter adds edges and removes pending rows; it never adds node kinds,
  changes node identity, or rewrites a tree-sitter edge.
- Producer output is pinned by the committed fixture, not by a Go dependency.
  Regenerate deliberately when the producer or fixture changes.
