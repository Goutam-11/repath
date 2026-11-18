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

`facts.json` was produced with the pinned producer on 2026-11-18:

```sh
python3 scripts/lsp-facts.py --lang ts \
  --root testdata/fixtures/lsp-adapter \
  --server ".cache/lsp-ts/node_modules/.bin/typescript-language-server --stdio" \
  --init-options '{"tsserver":{"path":"'$PWD'/.cache/lsp-ts/node_modules/typescript/lib/tsserver.js"}}' \
  --out testdata/fixtures/lsp-adapter/facts.json
```

Pinned producer versions: `typescript-language-server` **6.1.0**, TypeScript
**5.9.3**, Node **v24.18.0**, `repath-lsp-facts/1`. TypeScript 7.0.2 (the native
port) does not ship `lib/tsserver.js`, so the language server is pinned to 5.9.3.

The committed facts file keeps CI independent of Node and any language server.
It records 10 definitions and 13 references (the server reports symbols this
fixture does not need, such as `makeLeft` and the local variables); the gate
asserts the outcomes, not every reported fact:

- line 6: two `total` references with different targets merge into two exact
  `lsp.ts.method` edges at 0.99 (pending 4 → 2);
- line 8: one local reference for two pending rows is refused, so both rows
  stay pending and the reference is counted as unmatched.

Regenerate deliberately when the source, the producer, or the pinned server
changes, and update the gate counts in `internal/adapter/lspfacts_test.go`.
