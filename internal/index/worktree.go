package index

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Goutam-11/repath/internal/store"
)

const (
	// lockStaleAfter is how long an abandoned lock may keep other worktrees
	// waiting before it is treated as crash debris.
	lockStaleAfter = 10 * time.Minute
	// lockWaitTimeout bounds a wait for another worktree's cold build.
	lockWaitTimeout = 5 * time.Minute
	// commitCacheLimit is the PLAN §4.2 LRU bound.
	commitCacheLimit = 5
)

// gitState is what worktree seeding needs to know about a root.
type gitState struct {
	isGit     bool
	commonDir string
	head      string
	clean     bool
}

// Prepare opens root's active index, seeding it from the commit-keyed cache
// when it is a git worktree (PLAN §4.2). The active database always lives at
// <root>/.repath/index.db, so two worktrees never share a mutable SQLite file.
//
// A clean worktree copies the cached database for its HEAD. A dirty worktree
// copies it and then runs the normal incremental pass, which patches changed
// and untracked files. A missing cache is built once under an O_EXCL lock and
// stored for other worktrees. noBuild skips the lazy index pass (the index and
// status commands run it explicitly).
func Prepare(ctx context.Context, root string, noBuild bool) (*store.Store, error) {
	return PrepareScoped(ctx, root, noBuild, nil)
}

// PrepareScoped is Prepare for an explicit `index --scope` run. A scoped index
// is not a whole-repository snapshot, so it never seeds from (and never stores)
// the commit cache: that cache is keyed by HEAD only and describes the whole
// tree. Other commands call Prepare and keep whatever scope the index records.
func PrepareScoped(ctx context.Context, root string, noBuild bool, scope []string) (*store.Store, error) {
	g := inspectGit(ctx, root)
	if !g.isGit || g.head == "" {
		return openLocal(ctx, root, noBuild, Options{Scope: scope})
	}

	active := filepath.Join(root, ".repath", "index.db")
	if st, ok, err := openIfCurrent(ctx, active, g.head); err != nil {
		return nil, err
	} else if ok {
		if !noBuild {
			// A nil scope means "keep what the index records"; only an explicit
			// `--scope` changes it.
			opts := Options{}
			if scope != nil {
				opts = Options{Scope: scope}
			}
			if err := ensureIndexed(ctx, st, root, g, opts, false); err != nil {
				st.Close()
				return nil, err
			}
		}
		return st, nil
	}

	// The active database is stale (a different HEAD or schema). A scope is an
	// index property, so carry it across the rebuild: a lazy refresh of a scoped
	// index must not silently turn into a whole-repository build.
	if scope == nil {
		scope = recordedScope(ctx, active)
	}
	if len(scope) > 0 {
		if err := removeDB(active); err != nil {
			return nil, err
		}
		st, err := openMigrated(ctx, active)
		if err != nil {
			return nil, err
		}
		if !noBuild {
			if err := ensureIndexed(ctx, st, root, g, Options{Scope: scope}, true); err != nil {
				st.Close()
				return nil, err
			}
		}
		return st, nil
	}

	if err := removeDB(active); err != nil {
		return nil, err
	}

	cacheDir := commitCacheDir(g.commonDir)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		// The cache is an optimization; a read-only common dir must not make a
		// worktree unusable.
		return openLocal(ctx, root, noBuild, Options{})
	}
	lock, err := acquireLock(ctx, cacheDir, g.head)
	if err != nil {
		return openLocal(ctx, root, noBuild, Options{})
	}
	defer lock.release()

	cache := filepath.Join(cacheDir, g.head+".db")
	seeded := false
	if cacheValid(cache) {
		if err := copyDB(cache, active); err != nil {
			// A damaged cache is rebuilt rather than becoming a permanent
			// failure; the next clean index rewrites it.
			_ = removeDB(active)
			_ = os.Remove(cache)
			_ = os.Remove(cache + ".schema")
			_ = os.Remove(cache + ".rev")
		} else {
			touch(cache)
			evict(cacheDir, commitCacheLimit)
			seeded = true
		}
	}

	st, err := store.Open(ctx, active)
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("migrate index: %w", err)
	}

	if noBuild {
		return st, nil
	}

	if !seeded {
		if _, err := Run(ctx, st, root, Options{}); err != nil {
			st.Close()
			return nil, err
		}
		if g.clean {
			// Cache is an optimization; a read-only common dir must not fail a
			// successful index.
			_ = storeCommitLocked(ctx, st, g, cacheDir)
		}
		return st, nil
	}

	if !g.clean {
		if _, err := Run(ctx, st, root, Options{}); err != nil {
			st.Close()
			return nil, err
		}
		return st, nil
	}

	counts, err := st.Counts(ctx)
	if err != nil {
		st.Close()
		return nil, err
	}
	if counts.Files == 0 {
		if _, err := Run(ctx, st, root, Options{}); err != nil {
			st.Close()
			return nil, err
		}
		_ = storeCommitLocked(ctx, st, g, cacheDir)
	}
	return st, nil
}

// StoreCommit keeps the commit cache current after an explicit index or status
// refresh. It is a no-op outside git, for a dirty tree (a dirty tree is not a
// commit snapshot), or when the cache is already valid.
func StoreCommit(ctx context.Context, root string, st *store.Store) error {
	g := inspectGit(ctx, root)
	if !g.isGit || g.head == "" || !g.clean {
		return nil
	}
	cacheDir := commitCacheDir(g.commonDir)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	lock, err := acquireLock(ctx, cacheDir, g.head)
	if err != nil {
		return err
	}
	defer lock.release()
	return storeCommitLocked(ctx, st, g, cacheDir)
}

func openLocal(ctx context.Context, root string, noBuild bool, opts Options) (*store.Store, error) {
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("migrate index: %w", err)
	}
	if !noBuild {
		if err := ensureIndexed(ctx, st, root, gitState{}, opts, opts.Scope != nil); err != nil {
			st.Close()
			return nil, err
		}
	}
	return st, nil
}

// openMigrated opens an index database without git-aware seeding.
func openMigrated(ctx context.Context, path string) (*store.Store, error) {
	st, err := store.Open(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("migrate index: %w", err)
	}
	return st, nil
}

// ensureIndexed performs the lazy first index pass and, in a clean worktree,
// refreshes the commit cache. opts carries an explicit scope; with an empty
// Options the run preserves the scope already recorded in the index. force
// bypasses the "already indexed" short-circuit (a scope change must rebuild).
func ensureIndexed(ctx context.Context, st *store.Store, root string, g gitState, opts Options, force bool) error {
	if !force {
		counts, err := st.Counts(ctx)
		if err != nil {
			return err
		}
		if counts.Files > 0 {
			meta, err := st.Meta(ctx)
			if err != nil {
				return err
			}
			// A binary upgrade without a schema bump leaves a database that
			// looks current but was built by other extraction code. Let Run
			// decide (it forces a full re-read); do not short-circuit it (#134).
			if meta["build_revision"] == BuildRevision {
				return nil
			}
		}
	}
	if _, err := Run(ctx, st, root, opts); err != nil {
		return err
	}
	if g.isGit && g.clean && g.head != "" {
		_ = StoreCommit(ctx, root, st)
	}
	return nil
}

// recordedScope reads the scope out of an existing index before it is replaced.
// A missing, corrupt, or older-schema database has no scope to carry.
func recordedScope(ctx context.Context, path string) []string {
	st, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return nil
	}
	defer st.Close()
	meta, err := st.Meta(ctx)
	if err != nil {
		return nil
	}
	return ScopeFromMeta(meta)
}

// openIfCurrent opens an existing active database only when it belongs to this
// HEAD at the current schema. A corrupt or stale file is left for the caller to
// replace.
func openIfCurrent(ctx context.Context, active, head string) (*store.Store, bool, error) {
	if _, err := os.Stat(active); err != nil {
		return nil, false, nil
	}
	st, err := store.Open(ctx, active)
	if err != nil {
		return nil, false, nil
	}
	// Check meta before Migrate: Migrate rewrites schema_version, which would
	// make a database from a future binary look current and then be reused
	// against a schema it does not actually have.
	meta, err := st.Meta(ctx)
	if err != nil {
		st.Close()
		return nil, false, nil
	}
	if meta["git_head"] != head || meta["schema_version"] != strconv.Itoa(store.SchemaVersion) {
		st.Close()
		return nil, false, nil
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, false, nil
	}
	return st, true, nil
}

func storeCommitLocked(ctx context.Context, st *store.Store, g gitState, cacheDir string) error {
	// The commit cache is keyed by HEAD and describes the whole tree; a scoped
	// index must never become another worktree's seed.
	meta, err := st.Meta(ctx)
	if err != nil {
		return err
	}
	if len(ScopeFromMeta(meta)) > 0 {
		return nil
	}
	cache := filepath.Join(cacheDir, g.head+".db")
	if cacheValid(cache) {
		touch(cache)
		return nil
	}

	tmp := cache + ".tmp-" + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmp)
	if err := st.VacuumInto(ctx, tmp); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o444); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, cache); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// The sidecar is how a cache written by another schema version is
	// recognised without opening (and therefore mutating) a read-only file.
	if err := writeAtomic(cache+".schema", []byte(strconv.Itoa(store.SchemaVersion)+"\n"), 0o644); err != nil {
		return err
	}
	// The revision sidecar records the extraction/resolution code that built
	// this graph, so the next binary does not seed from it (#134).
	if err := writeAtomic(cache+".rev", []byte(BuildRevision+"\n"), 0o644); err != nil {
		return err
	}
	touch(cache)
	evict(cacheDir, commitCacheLimit)
	return nil
}

func cacheValid(cache string) bool {
	if _, err := os.Stat(cache); err != nil {
		return false
	}
	data, err := os.ReadFile(cache + ".schema")
	if err != nil {
		return false
	}
	if strings.TrimSpace(string(data)) != strconv.Itoa(store.SchemaVersion) {
		return false
	}
	// Schema version is a migration guard, not a behaviour guard: a binary
	// upgrade can change extraction or resolution without a schema bump, so the
	// cache is also keyed by the build revision that produced it (#134).
	rev, err := os.ReadFile(cache + ".rev")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(rev)) == BuildRevision
}

func commitCacheDir(commonDir string) string {
	return filepath.Join(commonDir, "repath", "commits")
}

func inspectGit(ctx context.Context, root string) gitState {
	commonOut, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return gitState{}
	}
	headOut, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return gitState{}
	}
	statusOut, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain").Output()
	clean := err == nil && cleanStatus(string(statusOut))

	common := strings.TrimSpace(string(commonOut))
	if common == "" {
		return gitState{}
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	return gitState{
		isGit:     true,
		commonDir: filepath.Clean(common),
		head:      strings.TrimSpace(string(headOut)),
		clean:     clean,
	}
}

// cleanStatus ignores .repath/ so our own active database never makes a
// worktree look dirty (a user repo may not have added it to .gitignore yet).
func cleanStatus(porcelain string) bool {
	for _, line := range strings.Split(porcelain, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) <= 3 {
			return false
		}
		path := strings.TrimSpace(line[3:])
		if path == ".repath" || strings.HasPrefix(path, ".repath/") {
			continue
		}
		return false
	}
	return true
}

type indexLock struct {
	path string
}

func acquireLock(ctx context.Context, dir, name string) (*indexLock, error) {
	lockPath := filepath.Join(dir, name+".lock")
	deadline := time.Now().Add(lockWaitTimeout)
	for {
		file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			fmt.Fprintf(file, "pid=%d\nstarted=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			file.Close()
			return &indexLock{path: lockPath}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("acquire index lock: %w", err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for index lock %s", lockPath)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (l *indexLock) release() {
	if l != nil && l.path != "" {
		_ = os.Remove(l.path)
	}
}

func copyDB(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp-" + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(dest + "-wal")
	_ = os.Remove(dest + "-shm")
	return os.Rename(tmp, dest)
}

func removeDB(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	return nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func touch(path string) {
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// evict keeps the most recently used commit databases and drops the rest.
func evict(dir string, keep int) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil || len(matches) <= keep {
		return
	}
	type entry struct {
		path string
		mod  time.Time
	}
	entries := make([]entry, 0, len(matches))
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		entries = append(entries, entry{path: path, mod: info.ModTime()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod.After(entries[j].mod) })
	for i := keep; i < len(entries); i++ {
		_ = os.Remove(entries[i].path)
		_ = os.Remove(entries[i].path + ".schema")
		_ = os.Remove(entries[i].path + ".rev")
	}
}
