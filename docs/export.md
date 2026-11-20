# Committable exports

repath answers through the CLI, MCP, and the read-only HTTP server. The two
export commands write the same answers as files, so a reviewer can open the
graph in a PR diff and a viewer can lay it out — without repath owning a
renderer (decisions 1 and 18: no UI).

Both are **read-only projections of the local index**. They add no nodes, no
edges, and no rules; every node and edge they write already exists in
`.repath/index.db` at the `--min-confidence` floor (default `0.5`). Pending
(unresolved) references are deliberately not exported: they are not edges.

## `repath map --md`

```sh
./bin/repath map --md                     # writes .repath/map/, prints the file list
./bin/repath map --md --out docs/repath-map --max-tokens 8000
```

Writes one `index.md` plus one page per module and prints the files it wrote
with their estimated token cost:

```
repath map --md · 5 files · 44,426 symbols · ~1,842 tokens (budget 2,000)
  docs/repath-map/index.md                        312 tokens
  docs/repath-map/modules/django.md               204 tokens
  docs/repath-map/modules/tests.md                188 tokens
  …
  (symbol rows trimmed to fit the budget; raise --max-tokens for more)
```

- `index.md` carries the module table (`name`, `symbols`, `depends on`), the
  module cycles, and the globally highest-ranked symbols.
- `modules/<slug>.md` carries the module's dependency and reverse-dependency
  lists, its ten largest files, and its highest-ranked symbols. Module names are
  slugified (`(root)` → `root`, `a.b` stays `a.b`) and collision-suffixed in
  name order, so the mapping is stable.
- `--out` is anchored at the repository root when it is relative, so
  `--root other-repo` never writes into your working directory. The default is
  `.repath/map`, which git ignores; pass `--out` for a bundle you intend to
  commit.
- `--max-tokens` bounds the **whole bundle**. The estimate is roughly four bytes
  per token, and the structural parts (tables, headers) are written even when
  they alone exceed the budget — the command says so instead of silently
  dropping modules. The default is 2,000, the same budget as the `map` answer;
  raise it when you want a fuller committed map.
- Output is deterministic: module pages are name-sorted, symbol rows follow the
  PageRank order with qname tie-breaking, and nothing embeds a timestamp or an
  absolute path beyond the ones you asked for. A second run over the same index
  is byte-identical, so the bundle diffs cleanly.

The bundle is a **summary**, not the graph: ranks and caller counts, not file
contents. Use `--json` on `map` for the same rollup as a single answer, or
`export` for the full graph.

## `repath export --graphml|--dot`

```sh
./bin/repath export --graphml --out graph.graphml     # Gephi, yEd, Obsidian
./bin/repath export --graphml | xmllint --format -    # stdout when --out is omitted
./bin/repath export --dot --out graph.dot             # Graphviz
./bin/repath export --graphml --modules --out modules.graphml
```

Without `--modules`, nodes are every indexed symbol and edges are every resolved
edge at the confidence floor:

| Element | Attributes |
| --- | --- |
| node | `qname`, `kind`, `file`, `line` |
| edge | `kind`, `confidence`, `rule` |

With `--modules`, the graph collapses to the module dimension:

| Element | Attributes |
| --- | --- |
| node | `qname` (module name), `kind="module"`, `symbols` (count) |
| edge | `kind="DEPENDS"`, `confidence` (strongest underlying edge), `rule="module.aggregate"`, `weight` (number of symbol edges) |

Self-edges are dropped in module mode, the same way the cycle rollup treats the
module graph. The two modes declare different GraphML `<key>`s, so a viewer only
shows attributes that mean something for that mode.

- Output goes to stdout unless `--out` is given; with `--out`, stdout is a
  one-line summary (`wrote FILE (graphml, N nodes, M edges, min-confidence
  0.50)`), which keeps the two modes composable.
- The writer is text + `encoding/xml` only — no dependency, no layout engine.
  DOT is plain text; GraphML is written by hand with `xml.EscapeText` for
  escaping so the byte stream stays stable.
- Determinism: nodes sort by file, line, qname; edges by source, target, kind,
  rule, confidence; module ids are assigned in module-name order. Two runs over
  the same index are byte-identical.
- GraphML node ids are the internal symbol ids (`n123`); qnames can repeat in
  different files, so the qname is an attribute, not the id. Module nodes use
  `m0`, `m1`, … in name order.

## Deliberately omitted

- **No new data.** No pending rows, no inferred edges, no synthetic edges beyond
  the documented module aggregation (which only counts and relabels edges that
  already exist).
- **No file bodies, source text, or unresolved rows** — exports carry locations,
  kinds, confidences, and rule ids only.
- **No layout.** GraphML/DOT consumers own the drawing; repath writes no SVG,
  HTML, or Obsidian vault.
- **Artifacts are read in place.** `export --graphml|--dot --artifact DIR|FILE`
  and `map --md --artifact DIR|FILE` open the artifact's SQLite snapshot
  read-only through `store.OpenReadOnly`; nothing is pulled into `.repath/` and
  nothing is written beside the artifact (a zstd artifact is decoded to a
  temporary file). The manifest and the database's own `schema_version` must
  both match the binary's schema, or the command refuses with exit `1`; a
  missing artifact is exit `3`. The output is byte-identical to exporting the
  live index. (`index --artifact` writes the snapshot; that is a different
  direction.)
- **No `--json` or `--md` on `export`**, and `--graphml`/`--dot` are mutually
  exclusive; both are usage exit `2`. `export` never returns `3`/`4`/`5`.
