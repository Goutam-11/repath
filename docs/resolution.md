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
| `ts.import.reexport` | 0.95 | the name is reached through `export { a } from "./x"` (or `export { default as a } from "./x"`) in the target module |
| `ts.import.star` | 0.85 | the name is reached through one or more `export * from "./x"` hops |
| `ts.import.workspace` | 0.95 | `import { a } from "@scope/pkg"` resolved through package.json `workspaces` to the package's source entry |
| `ts.import.workspace.reexport` | 0.90 | the workspace entry re-exports the name with `export { … } from` |
| `ts.import.workspace.star` | 0.80 | the workspace entry reaches it through `export *` |
| `ts.this.method` | 0.90 | `this.add()` inside `class Cart` → `Cart.add` |
| `ts.typed.method` | 0.85 | `cart.total()` where `cart` is `new Cart()`, `cart: Cart`, or annotated; the class is local or imported directly |
| `ts.local` | 0.85 | a symbol in the same file, unique (arity disambiguates) |
| `ts.typed.method.reexport` | 0.80 | the receiver type was imported through a barrel or workspace entry, so the class is resolved first and its method looked up in the defining file |
| `ts.typed.method.global` | 0.70 | receiver type known and exactly one `Type.method` exists in the repo |
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
| `py.property.alias` | 0.90 | `@label.setter` / `.deleter` / `.getter` → the `label` property |
| `py.module.relative` | 0.90 | `from . import util` then `util.clamp()` |
| `py.self.inherited` | 0.85 | `self.m()` / `cls.m()` where `m` is defined on a base class in the MRO |
| `py.self.super` | 0.85 | `super().m()`: the first class after the enclosing one in the MRO |
| `py.class.inherited` | 0.85 | `Service.m()` where `m` is inherited from a base |
| `py.typed.method` | 0.85 | `service.total()` where `service: Service`, `service = Service(...)`, or a same-file factory returns `Service` |
| `py.local` | 0.85 | a symbol in the same file, unique |
| `py.typed.inherited` | 0.80 | a typed receiver whose method lives on a base class |
| `py.import.star` | 0.80 | `from .util import *`, then a bare resolvable name |
| `py.typed.method.global` | 0.70 | the receiver type is known and exactly one `Type.method` exists |
| `py.global.unique` | 0.60 | exactly one symbol with that name in the repo |
| `py.global.ambiguous` | 0.40 | 2–5 candidates; one edge per candidate, filtered by default |

### Rust

| Rule id | Confidence | When |
| --- | --- | --- |
| `rs.use.named` | 1.00 | `use crate::util::clamp;` then `clamp()` |
| `rs.use.alias` | 1.00 | `use crate::util::helper as h;` then `h()` |
| `rs.use.reexport` | 0.95 | `pub use crate::models::User;` then `use crate::User;` |
| `rs.module.path` | 0.95 | `crate::util::clamp()`, `store::open()` |
| `rs.assoc.fn` | 0.95 | `Type::new()` where `Type` is a known struct/enum/trait |
| `rs.self.type` | 0.90 | `Self { .. }` inside an impl → the enclosing type |
| `rs.self.assoc` | 0.90 | `Self::new()` inside an impl |
| `rs.self.method` | 0.90 | `self.method()` inside an impl |
| `rs.enum.variant` | 0.90 | `Animal::Frog(..)` on a known enum; stored as REFERENCES |
| `rs.typed.method` | 0.85 | `value.method()` with a known receiver type |
| `rs.local` | 0.85 | a symbol in the same file, unique |
| `rs.use.glob` | 0.80 | `use crate::util::*;` then a bare name |
| `rs.assoc.global` | 0.70 | the type is not in scope but exactly one `Type.method` exists |
| `rs.typed.method.global` | 0.70 | receiver type known and exactly one `Type.method` exists |
| `rs.global.unique` | 0.60 | exactly one symbol with that name in the repo |
| `rs.global.ambiguous` | 0.40 | 2–5 candidates for a bare name |
| `rs.typed.method.ambiguous` | 0.40 | several `Type.method` impls (inherent vs trait, or two traits) |

Methods from inherent and trait `impl` blocks share the `Type.method` qname, so
a receiver with a known type can still be genuinely ambiguous; those edges are
kept at 0.40 and counted in `status` rather than guessed. A call resolving to a
struct or enum (tuple-struct call, struct literal) is `REFERENCES`; a call to an
associated function is `CALLS`.

Zero candidates → the reference goes to the `pending` queue, where a later run
retries it if the symbol appears. More than five candidates → dropped, because
one ambiguous name (`map`, `get`) would otherwise create hundreds of edges.
The optional SCIP adapter may later replace an entire ambiguous group with one
exact `0.99` edge when the compiler names one of its candidates; that is an
adapter-only pass with its own guardrails, never tree-sitter resolver behavior.
See [`docs/adapters.md`](adapters.md#ambiguous-replacement).

A Python call whose target is a class is stored as `REFERENCES`, the analogue of
`new` in TypeScript; the rule still names how the class was found. Decorator
applications are reference edges once, attached to the decorated function,
method, or class — the decorator expression is not also counted as a call.
Class bases are `EXTENDS` edges; `metaclass=` keyword arguments are skipped.

Python inheritance is walked for `self`, `cls`, `super()`, class receivers, and
typed receivers. The lookup order is the class's C3 linearization, capped at
eight classes, with a visited set so a cyclic hierarchy terminates and a
deterministic depth-first fallback when the hierarchy is inconsistent (Python
itself would reject it). A base resolved in the current run comes from the fresh
`extends` facts; a base in a file the run did not re-resolve comes from its
persisted `EXTENDS` edge, which is what keeps an incremental graph identical to
a cold one. Ambiguous base names are not followed, a compiler (`scip.*`) base
link never changes the MRO, and a base outside the repo (for example
`unittest.TestCase`) ends the chain: the inherited reference stays unresolved by
design instead of being guessed. Only the zero-argument `super().m()` form is
followed.

Receiver types come from an annotation, a constructor call, a same-file function
or method return annotation (`x = make()` where `def make() -> Service`), a class
attribute declared in this file (including on a same-file base), and
`Optional`/`Union`/`X | None` or quoted forward references that name exactly one
type. A generic element type is not unwrapped (`items: list[Service]` types
`items` as a list, a builtin), and a union of two project types stays untyped
rather than picking one.

A `@property` getter is an ordinary method symbol. `@value.setter` (and
`.deleter`/`.getter`) is one decorator reference to that property at 0.90
(`py.property.alias`), not a pending call to a method named `setter`. A
`@staticmethod`'s first parameter is a real argument and counts toward the
symbol's arity; a `@classmethod`'s `cls` does not.

## Import requests

### TypeScript / JavaScript

`./x`, `../x` resolve against the file, trying `.ts`, `.tsx`, `.js`, `.jsx`,
`.mjs`, `.cjs`, `index.*`, and the TypeScript convention where `./x.js` refers to
`x.ts`.

`baseUrl` + `paths` from `tsconfig.json` (or `jsconfig.json`) are honoured, so
`@/util` resolves to `src/util.ts`. The parser tolerates JSON with comments.

`export * from "./x"`, `export { a } from "./x"`, and
`export { a as b } from "./x"` are recorded as public import bindings, which is
what lets the resolver follow a barrel. The walk is bounded at eight hops,
cycle-safe through a visited set, and deterministic: a declaration exported by
the module wins over a named re-export, a named re-export over `export *`, and
several `export *` lines are tried in source order. `export *` never re-exports
a default (matching TypeScript), so a default import only crosses a barrel
through `export { default } from` or `export { X as default } from`. A chain
that ends at a dependency is classified external instead of pending, which is
why imports of names a local barrel re-exports from `@scope/pkg` no longer fall
back to a repo-wide same-name guess.

A bare specifier is then matched against the root `package.json` `workspaces`
patterns (`packages/*`, and the object form's `packages` array). Each matching
directory's `package.json` maps its `name` to a source entry, tried in this
order: `exports` (string, `.` condition, or `import`/`types`/`require`/`default`
condition), `source`, `types`, `typings`, `module`, `main`, then the
`src/index` / `index` convention. An entry that is not in the index (build
output under `dist/`) falls through to the next candidate, so a package whose
`main` points at compiled JavaScript still resolves to its TypeScript source.
`@scope/pkg/sub` resolves inside the package directory. This is local-manifest
only: no registry, lockfile, or package manager is consulted, and
`pnpm-workspace.yaml` is **not** read (Kibana declares its workspaces there, so
its `@kbn/*` imports stay external).

A static dynamic import is a binding too: `const m = require("./x")` and
`const m = await import("./x")` bind a namespace (resolved like
`import * as m`, including bundler magic comments inside the argument list),
`const { a, b: c } = require("./x")` binds names (resolved like named imports),
and `import x = require("./x")` binds a namespace. A computed or template
specifier creates no binding and stays external, and a side-effect-only
`import("./x")` has no symbol to point at, so it produces no edge.

Anything else — `react`, `@excalidraw/common` — is a package import and is
marked external. It is not an error and not counted as unresolved.

### Rust

Cargo packages define the crate roots: the directory containing `src/lib.rs` or
`src/main.rs` (workspace members included). A file maps to a module path by
convention — `src/foo.rs` is `crate::foo`, `src/foo/mod.rs` is `crate::foo`,
and `lib.rs`/`main.rs` are the crate root. `crate::`, `self::`, and `super::`
walk that path; an unqualified path is tried against the current crate first and
is external when no local module file exists.

`use` trees are flattened into bindings: named imports, `self`, aliases, nested
lists, and wildcards. A binding whose source is a module stays a module binding,
so `use crate::util; util::clamp()` works. `pub use` bindings carry the `public`
flag and are followed as re-exports, which is how `pub use crate::models::User`
in `lib.rs` makes `use crate::User` resolve to the definition in `models.rs`.

Two Rust-specific boundaries are deliberate. Macro invocations and
`macro_rules!` bodies are token trees and are never traversed, so they create no
edges and no symbols. An `impl Trait for Type` emits `IMPLEMENTS` only when
`Type` is a symbol in the same file (the common case); for an imported type the
impl methods are still indexed as `Type.method`, but there is no same-file
source node for the type edge.

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

Import bindings are persisted per file in the `imports` table (schema v5, extended for entrypoints). They
are resolution inputs, never nodes or edges. Storing them is what lets a package
`__init__.py` (or a compat module such as `requests.compat`) re-export a name to
a file that was not re-parsed in the current incremental run. A re-export chain
that ends at the standard library or a dependency is classified external rather
than unresolved.

`from x import *` is weaker because only a wildcard ties the name to the file:
it resolves at 0.80. Bindings are applied in file order, so when several star
imports define the same name the last one wins, matching Python execution.

## Known gaps

### All languages

- **Member calls without typed receivers.** `cart.total()` only resolves if
  `total` is unique, `cart` is a module/namespace import, or its type is known
  from an annotation, `new`, `Constructor()`, or a parameter. Real inference is
  out of scope for tree-sitter; this is the largest source of unresolved
  references on real repos. The opt-in SCIP adapter
  (`repath adapter --scip`, [`docs/adapters.md`](adapters.md)) can supply these
  typed facts without changing the tree-sitter ladder or its gates.
- **Top-level references.** A call outside any function has no containing symbol
  to attach an edge to, so it is skipped.

### TypeScript / JavaScript

- **`export * as ns`.** A namespace re-export is not followed, so
  `import { ns } from "./barrel"` for a barrel that writes
  `export * as ns from "./x"` stays unresolved. Named and star re-exports are
  followed.
- **Workspace manifests other than package.json.** Only the root package.json
  `workspaces` field is read. `pnpm-workspace.yaml` (Kibana), `lerna.json`, and
  per-package overrides are not consulted, so those repos' `@scope/*` imports
  stay external even when the sources are indexed.
- **Computed specifiers.** `import(path)` / `require(name)` with a variable or
  a template substitution is not a static module edge. `require` inside a
  non-declaration position (`module.exports = require("./x")`, a call argument)
  binds nothing; CommonJS `module.exports` / `exports.foo` export shapes are
  not modelled as exports either.
- **Barrel condition resolution.** `exports` conditions are tried in a fixed
  order (`import`, `types`, `require`, `default`, `node`); a package whose
  conditions disagree with what this repo would load can map to the other
  file. Subpath `exports` maps are ignored; `@scope/pkg/sub` resolves as a
  path inside the package directory.
- **Re-export chains through dependencies.** A local barrel that re-exports
  from an external package classifies the name external. That is deliberate
  (the definition is not in the repo) but it removes the 0.60 same-name
  fallback a pending retry used to provide; see the #56 note in
  [`docs/benchmarks.md`](benchmarks.md).

### Rust

- **Macro blind spots.** Bodies of `macro_rules!` and procedural macros are not
  parsed, so symbols and calls generated by `#[derive]` or macro expansion are
  absent. Typed receiver methods with no local definition are classified as
  external (a dependency or macro-generated trait impl) instead of pending;
  this trades a missing edge for a clean unresolved count and is documented
  rather than hidden.
- **Module path conventions.** `#[path = "..."]`, inline modules (`mod x { .. }`)
  as import targets, and non-standard layouts are not resolved. Inline module
  items are still indexed with a `mod.item` qname.
- **Workspace cross-crate imports.** `use other_crate::...` is external; workspace
  member sources are not linked yet.
- **Glob re-exports.** Named `pub use` bindings are followed; `pub use foo::*`
  is not expanded.
- **Generic bounds and associated types.** A method call on a type parameter
  (`T: Trait`) is not resolved through the bound, and return-type/iterator
  inference is not attempted.
- **Trait-edge source.** `IMPLEMENTS` is emitted only when the implementing type
  is defined in the same file; imported types have methods but no type edge.
- **Privacy.** `pub` sets the exported flag, but resolution does not reject a
  private item reached from another module.
- **Enum-variant heuristic.** A CamelCase associated call on a known enum is
  treated as variant construction at 0.90; an associated constant with the same
  shape would be misclassified.

### Python

- **External and ambiguous bases end the inheritance walk.** A base outside the
  repo (`unittest.TestCase`, a third-party class) is external, so inherited
  calls such as `self.assertEqual()` stay unresolved by design; an ambiguous
  base name is not followed either. Metaclass-generated, monkey-patched, and
  `getattr`-dispatched methods stay invisible to the static model.
- **Bounded `super()`.** Only the zero-argument `super().m()` form is followed;
  `super(Child, self).m()` stays unresolved because the reference does not
  carry its starting class.
- **Receiver types are file-local.** `self.cache.read()` is typed when `cache`
  is annotated in this file or on a same-file base; a class attribute declared
  in another module, a factory whose return annotation lives in another module,
  and an untyped local or attribute stay unresolved. Calls through callback
  parameters (`fn(1)`) are unresolved by design.
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
- **Entrypoint marker.** `if __name__ == "__main__"` is stored as the
  `py.main` entrypoint dimension when a module-level `main()` exists; a script
  with only top-level statements has no symbol to anchor and stays
  undetected. See `docs/flows.md`.

## Tuning

`--min-confidence` (default 0.5) is the dial: 0.9 gives import/this/local edges
only; 0.4 includes ambiguous candidates with their rule id visible.
