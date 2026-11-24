# Merged cross-repo index

`repath merge` answers "what breaks in the consumer when the producer changes"
without a daemon or a shared database: it copies several single-repo artifacts
into one **deterministic, read-only** snapshot. It is PLAN decision 25 (#88).

```sh
repath index --artifact producer/        # in producer/ (a repo)
repath index --artifact consumer/        # in consumer/ (a repo)
repath merge producer/ consumer/ --out merged/
repath export --dot --artifact merged/ --min-confidence 0.4 --out merged.dot
```

## Contract

- **Inputs** are artifact directories (a database plus its `manifest.json`), as
  written by `repath index --artifact` or a prior `repath merge`. They are
  opened read-only and never modified. A missing input is exit `3`; an
  unsupported schema is exit `1`; no inputs, no `--out`, or an `--out` that
  already holds an artifact is exit `2`.
- **Identity stays per repo.** Every file path becomes `<repo>/<path>`, so two
  repos that both declare `User` keep two distinct nodes. Nodes are never
  merged, renamed, or deduplicated across parts. The repo name comes from the
  part's recorded root; a collision is broken deterministically with a commit
  prefix, and parts are processed in name order.
- **Cross-repo links are derived, opt-in, and never guessed.** A type-like
  symbol (`class`, `interface`, `struct`, `trait`, `type`) whose qname occurs
  **exactly once** inside each of two parts becomes one `REFERENCES` edge from
  the earlier part to the later one: rule `xrepo.same_type_as`, confidence
  **0.45**. That is below the default 0.5 floor — the same trade as xlang
  (decision 23) — so single-repo default answers are unchanged and
  `--min-confidence 0.4` opts in. A qname declared twice in either part is
  never linked.
- **Determinism is a test.** Parts, files, nodes, pending rows, and entrypoints
  are copied in sorted order; the manifest carries no timestamp. Merging the
  same inputs twice produces byte-identical `index.db` and `manifest.json`.
- **The output is a projection, not a resolver.** Imports and `xlang_anchors`
  are not copied (a merged index is never re-resolved), entrypoints are
  re-anchored to the new node ids, and pending rows are preserved with their
  new file ids. Cross-repo **calls** are deferred: a `pending` row records the
  name and form but not the enclosing symbol, so repath cannot prove which
  caller in one repo matches a receiver type in the other. Adding that field
  would be a schema change with its own amendment.
- **Single-repo behaviour is unchanged.** `merge` only writes a new artifact;
  no existing command reads or writes a merged database implicitly.

## Manifest

A merged artifact's `manifest.json` sets `commit: "merged"` and lists each
part with its repo name, root, commit, and contributed counts under `parts`:

```json
{
  "schema_version": 6,
  "commit": "merged",
  "format": "sqlite",
  "parts": [
    {"name": "echo", "root": "/work/echo", "commit": "59aacdd…", "files": 115, "nodes": 1897, "edges": 2618, "pending": 2495},
    {"name": "gin",  "root": "/work/gin",  "commit": "0f09c3a…", "files": 99,  "nodes": 1755, "edges": 2336, "pending": 877}
  ]
}
```

The database `meta` also records `repo=merged`, `merge.parts`,
`merge.parts.<name>.root|commit`, and `merge.cross_repo_edges`.

## Reading a merged index

Merged artifacts are read-only, and every read command can answer from one
with `--artifact merged/` (#129):

```sh
repath callers producer/types.ts:1 --min-confidence 0.4 --artifact merged/ --json
repath impact  producer/types.ts:1 --min-confidence 0.4 --artifact merged/ --json
repath find    Widget --artifact merged/ --json          # both halves, as candidates
repath map --md --artifact merged/                       # module rollup
repath export --dot --artifact merged/ --min-confidence 0.4 --out merged.dot
```

- The `xrepo.same_type_as` link is at **0.45**, so it is invisible at the
  default 0.5 floor and `--min-confidence 0.4` opts in — the same trade as
  xlang.
- Two repos that declare the same qname keep two nodes, so a name lookup is
  ambiguous (exit `4`). Disambiguate with `file:line` (which the namespaced
  path makes unique); `find` lists both halves. `callers` accepts the same
  forms as `impact`.
- `repath pull merged/` seeds a checkout's `.repath/index.db` only as a
  snapshot; any later `index`/`status` refresh re-indexes that checkout and
  drops the merged parts. Use `--artifact` for durable cross-repo reads.

## Evidence

`gin-gonic/gin` @ `0f09c3a` (99 files / 1,755 nodes / 2,336 edges) merged with
`labstack/echo` @ `59aacdd` (115 files / 1,897 nodes / 2,618 edges):

| Metric | Value |
| --- | --- |
| files | 214 |
| nodes | 3,652 |
| edges | 4,962 (incl. 8 cross-repo links) |
| pending | 3,372 |
| entrypoints | 1,492 |
| merge wall / RSS | 0.22 s / 33 MiB (host load 2.47) |
| determinism | `index.db` and `manifest.json` byte-identical on a second run |

The eight links are `BindUnmarshaler`, `bindTestStruct`, `Context`,
`HandlerFunc`, `Skipper`, `RouteInfo`, `node`, and `testRoute`. The fixture pair
in `internal/merge` uses a hand-written `Widget` on both sides and asserts one
link, no link when the name is ambiguous, and byte-identical output.

## Limits

- Same **name**, not same shape: the link says "two repos declare a type with
  this name", not that the types are compatible. It is evidence for drift, and
  it is below the default floor for that reason.
- No re-resolution: a merged index cannot grow new cross-repo calls, and
  imports/anchors are not carried.
- Cross-repo links are only between parts, never inside one part.
- Merged snapshots are not seeded into the commit cache and are never written
  by a read command.
