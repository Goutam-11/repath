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
out/repath-<sha>.sqlite[.zst] + manifest.json   CI artifact, read-only
```

Paths are stored repo-relative POSIX, so the same database answers correctly
after it moves between directories. `meta.root` is updated on pull for
diagnostics; it is not used to resolve paths.

## Export

```sh
repath index --artifact out/              # uncompressed SQLite
repath index --artifact out/ --compress   # opt-in zstd
```

writes a consistent snapshot with SQLite `VACUUM INTO` (not a copy of a live WAL)
and a sidecar. With `--compress`, the snapshot is zstd-encoded after
`VACUUM INTO`; the manifest is the authority for which form is in the directory:

```json
{
  "schema_version": 6,
  "tool_version": "v0.5.0",
  "commit": "5c68771…",
  "file_count": 2,
  "node_count": 2,
  "edge_count": 1,
  "unresolved_rate": 0,
  "indexed_at": "2026-11-12T13:02:00Z",
  "format": "sqlite+zstd",
  "db_file": "repath-dev.sqlite.zst"
}
```

`format` is `sqlite` or `sqlite+zstd`; `pull` reads both. The database is chmod
`0444`: a CI artifact is evidence, not a working index. `pull` copies and
decompresses before opening, so the read-only mode is not a problem.

**Compression decision (v1.1, #18).** M5 deliberately shipped uncompressed
SQLite because zstd would have been a third dependency. #18 evaluated the
options; the approved decision is the Go dependency
`github.com/klauspost/compress/zstd` v1.20.4, behind an opt-in `--compress`
flag. This keeps the default artifact path dependency-free at runtime, avoids a
shell-out to an external `zstd` binary, and gives a measurable size win:

| Tree | Uncompressed | zstd | Ratio | Export time | Pull time |
| --- | --- | --- | --- | --- | --- |
| 10k synthetic | 3.8 MiB | 990 KiB | 25.3% | 25.4 → 41.2 ms | 3.9 → 8.7 ms |
| 50k synthetic | 19.2 MiB | 4.8 MiB | 25.2% | 127 → 190 ms | 11.8 → 30.9 ms |
| Kibana `x-pack/solutions` (38,548 real TS files) | 65.6 MiB | 17.2 MiB | 26.3% | 408 → 659 ms | 41 → 126 ms |

The real-repository run corrects the old 250–400 MB estimate: a 38.5k-file
TypeScript snapshot is **65.6 MiB**, not hundreds of megabytes, because the
artifact stores compact symbol rows rather than source text. The compression
ratio is stable at ~25–26% across synthetic and real trees. The schema gate is
unchanged and runs for both formats. Full method and budget misses are in
`docs/benchmarks.md`.

## Pull

```sh
repath pull out/                       # artifact directory
repath pull out/repath-abc.sqlite      # direct database, manifest beside it
repath pull out/repath-abc.sqlite.zst  # direct compressed artifact
repath pull https://ci.example/repath-abc.sqlite.zst
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
pull: artifact schema v99 is not supported by this binary (expects v5); upgrade repath or rebuild the artifact
```

The refusal happens before `.repath/index.db` is replaced, so a failed pull
cannot leave a half-seeded index. The manifest version and the database's own
`meta.schema_version` are both checked, before and after decompression.

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
- the same round-trip for `sqlite+zstd`, including the manifest format and a
  live query after pull;
- an unknown schema version is refused for both formats with the upgrade hint
  and no index file;
- an HTTP fetch of the database and its sibling manifest, for both compressed
  and uncompressed forms;
- a missing source maps to the not-found exit code;
- an empty checkout keeps the snapshot instead of wiping it.

The CLI path is covered by `TestIndexArtifactAndPull` and
`TestIndexCompressedArtifactAndPull` in `internal/cli`.
