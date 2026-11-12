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
	g := inspectGit(ctx, root)
	if !g.isGit || g.head == "" {
		return openLocal(ctx, root, noBuild)
	}

	active := filepath.Join(root, ".repath", "index.db")
	if st, ok, err := openIfCurrent(ctx, active, g.head); err != nil {
		return nil, err
	} else if ok {
		if !noBuild {
			if err := ensureIndexed(ctx, st, root, g); err != nil {
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
		return openLocal(ctx, root, noBuild)
	}
	lock, err := acquireLock(ctx, cacheDir, g.head)
	if err != nil {
		return openLocal(ctx, root, noBuild)
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

func openLocal(ctx context.Context, root string, noBuild bool) (*store.Store, error) {
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("migrate index: %w", err)
	}
	if !noBuild {
		if err := ensureIndexed(ctx, st, root, gitState{}); err != nil {
			st.Close()
			return nil, err
		}
	}
	return st, nil
}

// ensureIndexed performs the lazy first index pass and, in a clean worktree,
// refreshes the commit cache.
func ensureIndexed(ctx context.Context, st *store.Store, root string, g gitState) error {
	counts, err := st.Counts(ctx)
	if err != nil {
		return err
	}
	if counts.Files > 0 {
		return nil
	}
	if _, err := Run(ctx, st, root, Options{}); err != nil {
		return err
	}
	if g.isGit && g.clean && g.head != "" {
		_ = StoreCommit(ctx, root, st)
	}
	return nil
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
	return strings.TrimSpace(string(data)) == strconv.Itoa(store.SchemaVersion)
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
	}
}
