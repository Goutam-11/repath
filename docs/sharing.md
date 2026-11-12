# Sharing artifacts

A repath index is a file, so sharing it is a file operation: CI builds a
commit-keyed snapshot, and a laptop pulls it instead of paying for a cold index.
The snapshot contains structure only — names, paths, kinds, line numbers, edges,
rules, confidence. It never contains source text. Symbol names still leak intent,
so artifacts are internal build products, not a public cache.

## Three lives, one format

```
<repo>/.repath/index.db                     working copy (WAL, gitignored)
$GIT_COMMON_DIR/repath/commits/<sha>.db     shared across worktrees (docs/worktrees.md)
out/repath-<sha>.sqlite + manifest.json     CI artifact, read-only
```

Paths are stored repo-relative POSIX, so the same database answers correctly
after it moves between directories. `meta.root` is updated on pull for
diagnostics; it is not used to resolve paths.

## Export

```sh
repath index --artifact out/
```

writes a consistent snapshot with SQLite `VACUUM INTO` (not a copy of a live WAL)
and a sidecar:

```json
{
  "schema_version": 4,
  "tool_version": "v0.5.0",
  "commit": "5c68771…",
  "file_count": 2,
  "node_count": 2,
  "edge_count": 1,
  "unresolved_rate": 0,
  "indexed_at": "2026-11-12T13:02:00Z",
  "format": "sqlite",
  "db_file": "repath-dev.sqlite"
}
```

The database is chmod `0444`: a CI artifact is evidence, not a working index.
`pull` copies before opening, so the read-only mode is not a problem.

**Format deviation (M5).** PLAN §4.1 originally sketched
`repath-<sha>.sqlite.zst`. Compression would add a dependency beyond the two
the project allows, so M5 ships uncompressed SQLite and records the deviation in
PLAN and in `manifest.format`. A future compressed form can be added behind the
same manifest without guessing what an old artifact is.

## Pull

```sh
repath pull out/                       # artifact directory
repath pull out/repath-abc.sqlite      # direct database, manifest beside it
repath pull https://ci.example/repath-abc.sqlite
```

`pull` seeds `<repo>/.repath/index.db`, then overlays the local checkout:

- If the checkout has indexable files, the normal incremental path runs. A clean
  checkout at the same commit hashes every file and skips all of them; a dirty
  checkout patches only changed and untracked files.
- If the directory is empty, the snapshot is kept intact so an artifact can be
  inspected without the source tree.

A stale `-wal`/`-shm` beside a replaced database is removed first; sharing a
mutable database over a network filesystem is still forbidden (SQLite locking
does not survive that, and every developer's dirty state differs).

## Schema gate

The binary refuses an artifact whose `schema_version` it does not know:

```
pull: artifact schema v99 is not supported by this binary (expects v4); upgrade repath or rebuild the artifact
```

The refusal happens before `.repath/index.db` is replaced, so a failed pull
cannot leave a half-seeded index. The manifest version and the database's own
`meta.schema_version` are both checked.

## CI sketch

```yaml
- run: repath index --artifact out/
- uses: actions/upload-artifact@v4
  with: { name: repath-index, path: out/ }
```

A reviewer or a second job downloads `out/` and runs `repath pull out/`.

## Verification

`internal/artifact/artifact_test.go` covers:

- export → pull round-trip in two temp directories, including repo-relative
  paths, read-only mode, manifest counts, `meta.root`, and a live `impact`;
- an unknown schema version is refused with the upgrade hint and no index file;
- an HTTP fetch of the database and its sibling manifest;
- a missing source maps to the not-found exit code;
- an empty checkout keeps the snapshot instead of wiping it.

The CLI path is covered by `TestIndexArtifactAndPull` in `internal/cli`.
