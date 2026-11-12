# Python SCIP adapter fixture

A minimal Python repo where tree-sitter cannot resolve two member calls:
`run(cart: Cart)` calls a method inherited from `Base` (repath does not walk
inheritance), and `run_factory()` assigns the result of `make() -> Cart` to an
untyped local (repath only types constructor assignments). Both
`cart.total()` references stay pending; `scip-python` resolves them to
`Base.total`.

`index.scip` was produced by `scip-python` 0.6.8 from this directory:

```sh
npx --yes --package=@sourcegraph/scip-python@0.6.8 \
  scip-python index --project-name py-adapter --project-version 0.0.0 \
  --output index.scip
```

The producer was run with Python 3.12 in `PATH`; 0.6.8 reports Python 3.14 as
unsupported and writes an empty index. The committed SCIP file keeps the
adapter test independent of Node/Python in CI. Regenerate it deliberately when
the source or producer changes; `TestPythonSCIPAdapterGate` asserts the compiler
facts resolve both calls as `scip.py.method`.
