# Rust SCIP adapter fixture

A minimal Rust crate where tree-sitter leaves two member calls pending: a
generic bound (`T: Total`) and a trait object (`dyn Total`). repath models
neither bound, so `value.total()` stays unresolved in both functions. The
ordinary typed receiver in `run_cart` already resolves as `rs.typed.method`.

`index.scip` was produced by `rust-analyzer` 1.97.0 (commit `2d8144b`,
2026-07-07, matching rustc 1.97.0) from this directory:

```sh
rust-analyzer scip --output index.scip .
```

The committed SCIP file keeps `TestRustSCIPAdapterGate` independent of a Rust
toolchain in CI. Regenerate it deliberately when the source or producer
changes; the gate asserts both compiler facts resolve as `scip.rs.method` to
`Total.total`.
