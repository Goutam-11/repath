# Test-impact fixture

`total` is called directly by `src/cart.test.ts`, transitively through
`src/checkout.ts` by `src/checkout.test.ts`, imported without a call by
`src/import-only.test.ts`, and named by the path convention
`src/cart.spec.ts`. `unusedByTests` has no test reachability.
