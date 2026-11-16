# Python ambiguous-reference adapter fixture

Tree-sitter cannot resolve two bare `total()` calls because two modules define
`total` and nothing imports either one, so the resolver emits 2-candidate
0.40-confidence edges with rule `py.global.ambiguous`:

- `one()` has one ambiguous reference occurrence (line 2);
- `two()` has two same-named ambiguous references on one line (line 6), which
  collapse into a single candidate row set because the schema has no reference
  column.

The adapter tests build SCIP indexes in-process (`SCIPIndex`, no producer in
CI) to pin the replacement policy: a single occurrence replaces its group with
an exact `scip.py.call` edge; two occurrences on one line must leave the group
alone instead of silently merging it.
