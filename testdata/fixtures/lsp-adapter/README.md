# LSP facts fixture

A minimal TypeScript repo where tree-sitter cannot type the receivers, so the
four `total()` calls stay pending:

- `src/cart.ts` defines `Left.total` (line 2) and `Right.total` (line 6).
- `src/run.ts` line 6 calls `a.total()` and `b.total()`; both receivers come
  from a factory without an annotation, so tree-sitter leaves both pending and
  the language server resolves them to two **different** targets on one line.
- `src/run.ts` line 8 calls `c.total()` (resolvable to `Left.total`) and
  `d.total()`, where `d: Dep` is imported from a module outside the repo. The
  language server can only report the local half, so the compiler accounts for
  one of the two pending occurrences and the adapter must refuse that key.

`facts.json` is the **contract fixture** for the seam: it is written by hand from
the source (the exact shape an external LSP client must produce) and is
regenerated with the pinned producer in the #58 producer PR. It is not a claim
about any server version.

Commands (once the producer exists):

```sh
python3 scripts/lsp-facts.py --lang ts --root testdata/fixtures/lsp-adapter \
  --server "typescript-language-server --stdio" --out testdata/fixtures/lsp-adapter/facts.json
```

The gate `TestLSPFactsAdapterGate` asserts the line-6 pair becomes two exact
`lsp.ts.method` edges at 0.99 (pending 4 → 2) and the line-8 key is left alone.
