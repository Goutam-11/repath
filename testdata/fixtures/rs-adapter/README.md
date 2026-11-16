# Rust SCIP adapter fixture

A minimal Rust crate with one generic bound (`T: Total`), one trait object
(`dyn Total`), and one ordinary typed receiver (`cart: &Cart`).

Before issue #55 the two generic-bound `value.total()` calls were pending, which
is what this pinned SCIP file resolves. #55 makes the tree-sitter ladder follow
`T: Total` to the trait's local method, so those calls now resolve as
`rs.generic.bound` and `TestRustSCIPAdapterGate` seeds the pre-#55 pending rows
before applying SCIP. The SCIP file itself is still produced externally and is
not edited; it keeps the adapter gate independent of a Rust toolchain.

`index.scip` was produced by `rust-analyzer` 1.97.0 (commit `2d8144b`,
2026-07-07, matching rustc 1.97.0) from this directory:

```sh
rust-analyzer scip --output index.scip .
```

The committed SCIP file keeps `TestRustSCIPAdapterGate` independent of a Rust
toolchain in CI. Regenerate it deliberately when the source or producer
changes; the gate asserts both compiler facts resolve as `scip.rs.method` to
`Total.total`.
