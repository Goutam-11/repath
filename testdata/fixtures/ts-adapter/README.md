# SCIP adapter fixture

A minimal TypeScript repo where tree-sitter cannot type `cart` (`make()` is not
a constructor), so `cart.total()` stays pending. `index.scip` was produced by:

```sh
npx --yes --package=@sourcegraph/scip-typescript@0.6.2 scip-typescript index \
  --output testdata/fixtures/ts-adapter/index.scip
```

The committed SCIP file keeps the adapter test independent of Node/npm in CI.
Regenerate it deliberately when the source or SCIP producer changes; the test
asserts the compiler fact resolves `run -> Cart.total` as `scip.ts.method`.
