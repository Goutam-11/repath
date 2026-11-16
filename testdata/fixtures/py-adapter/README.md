# Python SCIP adapter fixture

A minimal Python repo with two member calls tree-sitter cannot resolve:

- `run_factory()` assigns the result of `make()` to an untyped local; the factory
  and its `-> Cart` return annotation live in another module, so the receiver
  type is unknown.
- `run_child()` calls through `node.parent`, a class attribute annotated in
  another module; receiver typing only follows attributes declared in the same
  file.

Both `total()` references stay pending; `scip-python` resolves them to
`Base.total`. (The inherited-call gap this fixture used to cover is closed by
issue #54: `self.m()` now walks the base classes.)

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
