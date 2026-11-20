# Scoped indexes

Discovery resolves to the enclosing git root, which is right for "what does this
change reach" but wrong for a monorepo when you only care about one subtree:
running `repath index` inside `x-pack/solutions/security/packages` indexed all
38,548 Kibana files (~23 s, ~630 MiB) instead of the 2,939 files under that
directory.

`repath index --scope <dir>` restricts **discovery** to a subtree while keeping
the repository root for module detection and git-aware invalidation.

```sh
./bin/repath index --scope x-pack/solutions/security/packages
# indexed 2939 files (+2939 ~0 -0), 7538 nodes, 22 parse failures in 1612ms
# scope x-pack/solutions/security/packages (2939 files)

./bin/repath status --json | jq '{files, scope, scope_files}'
# {"files": 2939, "scope": ["x-pack/solutions/security/packages"], "scope_files": 2939}
```

`--scope` is repeatable (`--scope a --scope b`). Paths are repository-relative,
normalised (`.`, `//`, backslashes), de-duplicated and sorted; an absolute path,
a path that escapes the root, `.`, or a path that is not an existing directory
is usage exit `2`.

## What a scope changes

| | Behaviour |
| --- | --- |
| Discovery | Only files inside the scope are read; a scope is a filter, never a rename of the root. |
| Modules | `DetectModules` still runs from the repository root over the discovered files, so module boundaries keep their repository meaning (`x-pack` for a deep subtree). |
| Git invalidation | Unchanged: `git status`/`ls-files` still decide what to re-read, so incremental and `--full` runs are as cheap as they are whole-repo. |
| Rows | Everything outside the scope is absent: no files, nodes, edges, pending rows, entrypoints, or anchors for them. |
| Status | `status --json` reports `scope` and `scope_files` (the indexed, in-scope file count); the text status prints `scope <dirs> (N files)`. `index --json` reports `scope` too. |
| MCP / HTTP `status` | Additive fields; the six answers are otherwise unchanged. |

## Changing the scope

The scope is recorded in `meta.scope` (a JSON array; absent means the whole
repository). It is an index property, and the index is never a blend of two file
sets:

- `repath index` **without** `--scope` targets the whole repository. On an index
  built with a scope it wipes the graph (files, nodes, edges, pending, imports,
  entrypoints, anchors, modules) and rebuilds — that is the explicit way back to
  a whole-repo index, and it is why a plain `index` is not a cheap refresh of a
  scoped index.
- `repath index --scope ...` with a **different set** does the same wipe and
  rebuild. Changing the scope invalidates the index; it never splices rows.
- `repath index --scope <same set>` is the incremental/`--full` refresh of the
  scoped index.
- Every **other** command (`status`, `impact`, `map`, `find`, `path`, `check`,
  …) preserves the recorded scope: it has no scope argument, so its lazy refresh
  must not change the answer's file set. That includes a HEAD change — the scope
  is read from the stale database before it is replaced and carried into the
  rebuild.

## Worktree seeding and artifacts

- The commit cache (`$GIT_COMMON_DIR/repath/commits/<sha>.db`) is keyed by HEAD
  and describes the **whole tree**. A scoped index is therefore never seeded
  from it and never stored into it; a scoped run is not a commit snapshot.
  Scope lives with the active `.repath/index.db`, so a *fresh* worktree or a
  cache seed starts whole-repo — re-run `index --scope ...` there. There is no
  per-scope cache file in v1.
- `index --artifact DIR` records the scope in the manifest (`"scope"`), and the
  snapshot database carries `meta.scope`. `repath pull` keeps both, refuses a
  manifest whose scope disagrees with the database it ships (exit `1`), and the
  next plain `index` clears the scope like any other whole-repo request.

## Evidence (Kibana @ 0551196, this host, load ~2)

| | Whole tree | `--scope x-pack/solutions/security/packages` |
| --- | --- | --- |
| Files | 38,548 | **2,939** |
| Nodes / edges / pending | 87,478 / 62,489 / 157,205 | **7,538 / 3,592 / 9,461** |
| Parse failures | 158 | **22** |
| Cold wall / peak RSS | **23.3 s / 645 MiB** | **2.34 s / 123 MiB** |
| 10-file incremental | 632 ms (whole tree) | **0.94 s** (2,939-file index) |

Cold / incremental / second `--full` scoped runs produce **identical edge and
pending row sets** (3,592 / 9,461), the same parity rule as #49. The whole-tree
numbers are unchanged from the pre-#86 measurement. Tests:
`TestScopeRestrictsAndRebuilds`, `TestScopeParity`,
`TestScopeSurvivesAHeadChange`, `TestScopedIndexNeverTouchesTheCommitCache`,
`TestScopedArtifactRecordsScope`, and the CLI `TestIndexScope*`.

## Deliberate omissions

- No `--scope` on the read commands (a query does not pick a different file set
  than the index it reads).
- No per-scope worktree cache, no scope in the database file name.
- No partial incremental when the scope set changes: sets are atomic.
- No `.gitignore`-independent discovery: git still decides what is a source.
