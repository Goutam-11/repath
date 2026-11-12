# Resolution

repath parses each file in isolation, then resolves its references globally. The
second step is where confidence comes from: a fact is `CALLS` at 0.6 only because
nothing better was available.

## Ladder

### TypeScript / JavaScript

| Rule id | Confidence | When |
| --- | --- | --- |
| `ts.import.named` | 1.00 | `import { add } from "./util"` and `add` is exported there |
| `ts.import.namespace` | 0.95 | `import * as ns` then `ns.add()` |
| `ts.import.default` | 0.95 | default import, target file has exactly one export |
| `ts.this.method` | 0.90 | `this.add()` inside `class Cart` → `Cart.add` |
| `ts.local` | 0.85 | a symbol in the same file, unique (arity disambiguates) |
| `ts.global.unique` | 0.60 | exactly one symbol with that name in the repo |
| `ts.method.unique` | 0.60 | `obj.add()` where exactly one method is named `add` |
| `ts.global.ambiguous` | 0.40 | 2–5 candidates; one edge per candidate, default queries filter it |
| `ts.method.ambiguous` | 0.40 | same, for member calls |

### Python

| Rule id | Confidence | When |
| --- | --- | --- |
| `py.import.named` | 1.00 | `from .cart import total` and `total` is defined in `cart.py` |
| `py.import.alias` | 1.00 | `from .cart import total as t` |
| `py.import.module` | 0.95 | `import pkg.mod` then `pkg.mod.f()`, or `from pkg import mod` |
| `py.import.reexport` | 0.95 | `from app import Service`, re-exported by `app/__init__.py` or a compat module |
| `py.this.method` | 0.90 | `self.add()` inside `class Cart` → `Cart.add` |
| `py.class.method` | 0.90 | `Service.create()` where `Service` is a class symbol |
| `py.module.relative` | 0.90 | `from . import util` then `util.clamp()` |
| `py.import.star` | 0.80 | `from .util import *`, then a bare resolvable name |
| `py.typed.method` | 0.85 | `service.total()` where `service: Service` or `service = Service(...)` |
| `py.local` | 0.85 | a symbol in the same file, unique |
| `py.typed.method.global` | 0.70 | the receiver type is known and exactly one `Type.method` exists |
| `py.global.unique` | 0.60 | exactly one symbol with that name in the repo |
| `py.global.ambiguous` | 0.40 | 2–5 candidates; one edge per candidate, filtered by default |

Zero candidates → the reference goes to the `pending` queue, where a later run
retries it if the symbol appears. More than five candidates → dropped, because
one ambiguous name (`map`, `get`) would otherwise create hundreds of edges.

A Python call whose target is a class is stored as `REFERENCES`, the analogue of
`new` in TypeScript; the rule still names how the class was found. Decorator
applications are reference edges once, attached to the decorated function,
method, or class — the decorator expression is not also counted as a call.
Class bases are `EXTENDS` edges; `metaclass=` keyword arguments are skipped.

## Import requests

### TypeScript / JavaScript

`./x`, `../x` resolve against the file, trying `.ts`, `.tsx`, `.js`, `.jsx`,
`.mjs`, `.cjs`, `index.*`, and the TypeScript convention where `./x.js` refers to
`x.ts`.

`baseUrl` + `paths` from `tsconfig.json` (or `jsconfig.json`) are honoured, so
`@/util` resolves to `src/util.ts`. The parser tolerates JSON with comments.

Anything else — `react`, `@excalidraw/common` — is a package import and is
marked external. It is not an error and not counted as unresolved.

### Python

The leading dots of a relative import are counted exactly as Python counts them:
`.` is the current package (the file's directory), `..` the parent, and
`from ..pkg import x` walks up one directory and then into `pkg`. An absolute
import is looked up at the repo root, then under `src/`, then under `lib/`, which
covers the common source layouts without a config file.

A module file is `name.py` or `name/__init__.py`. `from pkg import name` binds a
symbol when `name` is defined in `pkg/__init__.py`, and binds a submodule when
`pkg/name.py` exists; the receiver then resolves as a module. `import pkg.mod as
m` and `import pkg.mod` both keep the dotted path, so `pkg.mod.f()` and
`m.f()` resolve.

Import bindings are persisted per file in the `imports` table (schema v3). They
are resolution inputs, never nodes or edges. Storing them is what lets a package
`__init__.py` (or a compat module such as `requests.compat`) re-export a name to
a file that was not re-parsed in the current incremental run. A re-export chain
that ends at the standard library or a dependency is classified external rather
than unresolved.

`from x import *` is weaker because only a wildcard ties the name to the file:
it resolves at 0.80. Bindings are applied in file order, so when several star
imports define the same name the last one wins, matching Python execution.

## Known gaps

### Both languages

- **Member calls without typed receivers.** `cart.total()` only resolves if
  `total` is unique, `cart` is a module/namespace import, or its type is known
  from an annotation, `new`, `Constructor()`, or a parameter. Real inference is
  out of scope for tree-sitter; this is the largest source of unresolved
  references on real repos.
- **Top-level references.** A call outside any function has no containing symbol
  to attach an edge to, so it is skipped.

### TypeScript / JavaScript

- **Barrel re-exports.** `export * from "./x"` and `export { a } from "./x"` are
  not followed yet.
- **Monorepo workspace packages.** `@scope/pkg` imports do not resolve to the
  package's source; they are treated as external until workspace detection lands.
- **Dynamic imports and `require`.**

### Python

- **Inheritance is not walked for `self`.** `self.method()` resolves when the
  method is on the enclosing class; a method inherited from a base class (for
  example `Session.should_strip_auth` from `SessionRedirectMixin`) stays
  unresolved. Class bases are still edges, so `impact` sees the inheritance.
- **Untyped locals and attributes.** `session.get()` is unresolved unless
  `session` has a known type. Calls through callback parameters
  (`fn(1)`) are unresolved by design.
- **Module aliases re-exported as modules.** `from .compat import cookielib` is
  followed when `cookielib` is a local module, but not when the compat module
  aliases an external module (`import http.cookiejar as cookielib`).
- **`sys.path`, `PYTHONPATH`, and arbitrary layouts.** Only root, `src/`, and
  `lib/` are searched; namespace packages without `__init__.py` are not.
- **`__all__` and underscore visibility.** `__all__` is not consulted and an
  underscore name is still importable, matching Python. Wildcard precedence is
  not modelled.
- **Dynamic constructs.** `__import__`, `getattr`, metaclass-generated methods,
  and conditional imports (last import wins) are outside the static model.
- **Entrypoint marker.** `if __name__ == "__main__"` is not stored: v1 has no
  entrypoint node (PLAN §15), so the marker is deferred rather than invented.

## Tuning

`--min-confidence` (default 0.5) is the dial: 0.9 gives import/this/local edges
only; 0.4 includes ambiguous candidates with their rule id visible.
