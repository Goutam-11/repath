# Resolution

repath parses each file in isolation, then resolves its references globally. The
second step is where confidence comes from: a fact is `CALLS` at 0.6 only because
nothing better was available.

## Ladder

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

Zero candidates → the reference goes to the `pending` queue, where a later run
retries it if the symbol appears. More than five candidates → dropped, because
one ambiguous name (`map`, `get`) would otherwise create hundreds of edges.

## Import requests

`./x`, `../x` resolve against the file, trying `.ts`, `.tsx`, `.js`, `.jsx`,
`.mjs`, `.cjs`, `index.*`, and the TypeScript convention where `./x.js` refers to
`x.ts`.

`baseUrl` + `paths` from `tsconfig.json` (or `jsconfig.json`) are honoured, so
`@/util` resolves to `src/util.ts`. The parser tolerates JSON with comments.

Anything else — `react`, `@excalidraw/common` — is a package import and is
marked external. It is not an error and not counted as unresolved.

## Known gaps

- **Member calls without typed receivers.** `cart.total()` only resolves if
  `total` is unique, or if `cart` is a namespace import. Real type inference is
  out of scope for tree-sitter; this is the largest source of unresolved
  references on real repos.
- **Barrel re-exports.** `export * from "./x"` and `export { a } from "./x"` are
  not followed yet.
- **Monorepo workspace packages.** `@scope/pkg` imports do not resolve to the
  package's source; they are treated as external until workspace detection lands.
- **Top-level references.** A call outside any function has no containing symbol
  to attach an edge to, so it is skipped.
- **Dynamic imports and `require`.**

## Tuning

`--min-confidence` (default 0.5) is the dial: 0.9 gives import/this/local edges
only; 0.4 includes ambiguous candidates with their rule id visible.
