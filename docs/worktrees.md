# Worktrees

Parallel agents usually run in git worktrees: they share the object store but
have different working directories, HEADs, and dirty files. repath gives each
worktree its own mutable index and shares only an immutable, commit-keyed cache
(PLAN §4.2).

```
<worktree>/.repath/index.db                     active, writable, per worktree
$GIT_COMMON_DIR/repath/commits/<sha>.db         immutable snapshot, shared
$GIT_COMMON_DIR/repath/commits/<sha>.db.schema  schema marker for that cache
$GIT_COMMON_DIR/repath/commits/<sha>.lock       O_EXCL build/seed lock
```

`GIT_COMMON_DIR` is the repository's main `.git` even from a linked worktree, so
every worktree sees the same cache directory. The active database is never
shared: SQLite has one writer, and every worktree's dirty state differs.

## Lifecycle

`repath` calls the same `index.Prepare` path from every CLI query, `status`,
`index`, and the MCP server:

1. **Active database is current** (`meta.git_head == HEAD`, schema matches) →
   open it. Queries do not re-hash; `status`/`index` refresh explicitly, as
   before.
2. **Active database is missing, HEAD is clean, cache hits** → copy the `0444`
   cache to `<worktree>/.repath/index.db` and answer. This is the sub-second
   second-worktree case.
3. **Active database is missing, HEAD is dirty, cache hits** → copy, then run
   the normal incremental pass over changed and untracked files. The cache is
   not written: a dirty tree is not a commit snapshot.
4. **Cache miss, clean** → build once under the per-commit lock, then store the
   result in the cache.
5. **Cache miss, dirty** → build the active database only; the next clean
   worktree at that commit builds the cache.
6. **HEAD changed** in an existing worktree → the active database is discarded
   and reseeded from the new commit's cache, or rebuilt.

An empty checkout after `repath pull` keeps the artifact snapshot; the overlay
only runs when `index.Discover` finds indexable files.

## Lock

The lock is `O_EXCL` with `pid` and a UTC timestamp. A second worktree waits up
to 5 minutes; a lock older than 10 minutes is treated as crash debris and
removed. Cache writes are serialized, so two cold worktrees do not both build
the same commit. If the common dir is read-only, preparation falls back to a
local active database rather than failing the query.

## Eviction

After a cache hit or write, the cache directory is sorted by mtime (touched on
use) and the five most recent `<sha>.db` files are kept; older databases and
their `.schema` sidecars are removed. The active database is never evicted.

## Safety properties

- The cache is chmod `0444` and is only ever copied, never opened for writing.
- The copy lands through a temp file and rename; stale `-wal`/`-shm` files
  beside the destination are removed first.
- `git status --porcelain` is filtered to ignore `.repath/`, so creating the
  active index never makes a clean worktree look dirty (a user repo may not have
  added `.repath/` to its `.gitignore` yet).
- A cache whose `.schema` sidecar does not match the binary's schema is ignored
  and rebuilt, so a binary upgrade cannot seed an old snapshot.
- Nothing here puts a SQLite file on a network filesystem; the cache is a local
  copy source, not a shared live database.

## Verification

`internal/index/worktree_test.go` covers:

- the main worktree builds and writes a read-only cache; a second worktree at
  the same HEAD warms from it (measured **12.9 ms**, budget <2 s);
- a dirty worktree patches a modified file and an untracked file while a later
  clean worktree still sees the clean cache (no dirty leakage);
- a new commit reseeds the active database and updates `meta.git_head`;
- LRU keeps five caches and removes the two oldest plus their sidecars;
- a stale lock is recovered;
- `.repath/` is ignored by the clean check.

`make perf` reports the 10k-file cold/incremental/query/MCP numbers in
`internal/index/perf_test.go`.
