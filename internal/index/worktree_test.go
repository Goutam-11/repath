package index

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Goutam-11/repath/internal/store"
)

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func initGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-q")
	gitCmd(t, dir, "config", "user.email", "test@example.com")
	gitCmd(t, dir, "config", "user.name", "Repath Test")
	for name, content := range files {
		write(t, dir, name, content)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-qm", "initial")
	return dir
}

func addWorktree(t *testing.T, repo string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	gitCmd(t, repo, "worktree", "add", "--detach", wt, "HEAD")
	return wt
}

func commonDir(t *testing.T, root string) string {
	t.Helper()
	common := gitCmd(t, root, "rev-parse", "--git-common-dir")
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	return filepath.Clean(common)
}

func TestPrepareSeedsSecondWorktree(t *testing.T) {
	ctx := context.Background()
	repo := initGitRepo(t, map[string]string{
		"src/a.ts": "export function add(a: number, b: number): number { return a + b; }\n",
		"src/b.ts": "import { add } from \"./a\";\nexport function sum(): number { return add(1, 2); }\n",
	})

	st, err := Prepare(ctx, repo, false)
	if err != nil {
		t.Fatalf("prepare main worktree: %v", err)
	}
	counts, err := st.Counts(ctx)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	if counts.Files != 2 || counts.Nodes != 2 {
		st.Close()
		t.Fatalf("main counts = %+v, want 2/2", counts)
	}
	st.Close()

	head := gitCmd(t, repo, "rev-parse", "HEAD")
	cache := filepath.Join(commitCacheDir(commonDir(t, repo)), head+".db")
	info, err := os.Stat(cache)
	if err != nil {
		t.Fatalf("commit cache not written: %v", err)
	}
	if info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("cache mode = %v, want read-only", info.Mode().Perm())
	}
	if _, err := os.Stat(cache + ".schema"); err != nil {
		t.Fatalf("cache schema sidecar missing: %v", err)
	}

	wt := addWorktree(t, repo)
	start := time.Now()
	st2, err := Prepare(ctx, wt, false)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("prepare second worktree: %v", err)
	}
	defer st2.Close()
	counts, err = st2.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Files != 2 || counts.Nodes != 2 {
		t.Fatalf("second worktree counts = %+v, want 2/2", counts)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("warm seed took %v, want < 2s", elapsed)
	}
	t.Logf("second worktree warm seed: %v", elapsed)

	// Each worktree owns its active database; only the immutable cache is shared.
	for _, path := range []string{
		filepath.Join(repo, ".repath", "index.db"),
		filepath.Join(wt, ".repath", "index.db"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("active database missing at %s: %v", path, err)
		}
	}
}

func TestPreparePatchesDirtyWorktreeWithoutPollutingCache(t *testing.T) {
	ctx := context.Background()
	repo := initGitRepo(t, map[string]string{
		"src/a.ts": "export function base() {}\n",
	})
	st, err := Prepare(ctx, repo, false)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	dirty := addWorktree(t, repo)
	write(t, dirty, "src/a.ts", "export function base() {}\nexport function changed() {}\n")
	write(t, dirty, "src/extra.ts", "export function extra() {}\n")

	stDirty, err := Prepare(ctx, dirty, false)
	if err != nil {
		t.Fatalf("prepare dirty worktree: %v", err)
	}
	matches, err := stDirty.SearchSymbols(ctx, "extra", 10)
	if err != nil {
		stDirty.Close()
		t.Fatal(err)
	}
	if len(matches) == 0 {
		stDirty.Close()
		t.Fatal("dirty worktree did not index the untracked symbol")
	}
	matches, err = stDirty.SearchSymbols(ctx, "changed", 10)
	if err != nil {
		stDirty.Close()
		t.Fatal(err)
	}
	if len(matches) == 0 {
		stDirty.Close()
		t.Fatal("dirty worktree did not index the modified symbol")
	}
	stDirty.Close()

	// A fresh clean worktree at the same commit must come from the clean cache,
	// not from the dirty overlay.
	clean := addWorktree(t, repo)
	stClean, err := Prepare(ctx, clean, false)
	if err != nil {
		t.Fatalf("prepare clean worktree: %v", err)
	}
	defer stClean.Close()
	for _, name := range []string{"extra", "changed"} {
		matches, err := stClean.SearchSymbols(ctx, name, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Fatalf("dirty symbol %q leaked into the commit cache", name)
		}
	}
}

func TestPrepareReseedsAfterHeadChange(t *testing.T) {
	ctx := context.Background()
	repo := initGitRepo(t, map[string]string{
		"src/a.ts": "export function one() {}\n",
	})
	st, err := Prepare(ctx, repo, false)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	write(t, repo, "src/b.ts", "export function two() {}\n")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "-qm", "second")

	st, err = Prepare(ctx, repo, false)
	if err != nil {
		t.Fatalf("prepare after head change: %v", err)
	}
	defer st.Close()
	matches, err := st.SearchSymbols(ctx, "two", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("new HEAD symbol missing after reseed")
	}
	meta, err := st.Meta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta["git_head"] != gitCmd(t, repo, "rev-parse", "HEAD") {
		t.Fatalf("meta.git_head = %q, want current HEAD", meta["git_head"])
	}
}

func TestEvictCommitCaches(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	for i := 0; i < 7; i++ {
		path := filepath.Join(dir, fmt.Sprintf("%02d.db", i))
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".schema", []byte("4\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		stamp := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	evict(dir, 5)

	remaining, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 5 {
		t.Fatalf("remaining caches = %d, want 5: %v", len(remaining), remaining)
	}
	for _, gone := range []string{"00", "01"} {
		if _, err := os.Stat(filepath.Join(dir, gone+".db")); !os.IsNotExist(err) {
			t.Fatalf("oldest cache %s was not evicted", gone)
		}
		if _, err := os.Stat(filepath.Join(dir, gone+".db.schema")); !os.IsNotExist(err) {
			t.Fatalf("schema sidecar for %s was not evicted", gone)
		}
	}
}

func TestAcquireLockRecoversStale(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "abc.lock")
	if err := os.WriteFile(stale, []byte("pid=999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-lockStaleAfter - time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	lock, err := acquireLock(context.Background(), dir, "abc")
	if err != nil {
		t.Fatalf("acquireLock did not recover a stale lock: %v", err)
	}
	lock.release()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("lock was not released: %v", err)
	}
}

func TestCleanStatusIgnoresRepathDir(t *testing.T) {
	if !cleanStatus("?? .repath/\n?? .repath/index.db\n") {
		t.Fatal("our own .repath/ files made the worktree look dirty")
	}
	if cleanStatus(" M src/a.ts\n") {
		t.Fatal("a modified source file was reported clean")
	}
	if cleanStatus("?? src/new.ts\n") {
		t.Fatal("an untracked source file was reported clean")
	}
}

func TestCacheValidRequiresSchemaAndBuildRevision(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "abc.db")
	if err := os.WriteFile(cache, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	schema := strconv.Itoa(store.SchemaVersion) + "\n"

	cases := []struct {
		name   string
		schema string
		rev    string // empty: no revision sidecar at all
		want   bool
	}{
		{"schema and revision match", schema, "rev-b\n", true},
		{"revision mismatch", schema, "rev-a\n", false},
		{"missing revision sidecar", schema, "", false},
		{"schema mismatch", "4\n", "rev-b\n", false},
	}

	old := BuildRevision
	t.Cleanup(func() { BuildRevision = old })
	BuildRevision = "rev-b"

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, suffix := range []string{".schema", ".rev"} {
				if err := os.Remove(cache + suffix); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(cache+".schema", []byte(tc.schema), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.rev != "" {
				if err := os.WriteFile(cache+".rev", []byte(tc.rev), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := cacheValid(cache); got != tc.want {
				t.Fatalf("cacheValid = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCommitCacheRecordsBuildRevision(t *testing.T) {
	ctx := context.Background()
	repo := initGitRepo(t, map[string]string{
		"src/a.ts": "export function a() {}\n",
	})

	old := BuildRevision
	t.Cleanup(func() { BuildRevision = old })
	BuildRevision = "rev-a"

	st, err := Prepare(ctx, repo, false)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	st.Close()

	head := gitCmd(t, repo, "rev-parse", "HEAD")
	cache := filepath.Join(commitCacheDir(commonDir(t, repo)), head+".db")
	data, err := os.ReadFile(cache + ".rev")
	if err != nil {
		t.Fatalf("revision sidecar missing: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "rev-a" {
		t.Fatalf("revision sidecar = %q, want %q", got, "rev-a")
	}
	if !cacheValid(cache) {
		t.Fatal("cache is not valid under the revision that wrote it")
	}
}

func TestPrepareNeverSeedsAcrossBuildRevisions(t *testing.T) {
	ctx := context.Background()
	repo := initGitRepo(t, map[string]string{
		"src/a.ts": "export function a() {}\n",
		"src/b.ts": "export function b() {}\n",
	})
	other := initGitRepo(t, map[string]string{
		"src/only.ts": "export function only() {}\n",
	})

	old := BuildRevision
	t.Cleanup(func() { BuildRevision = old })
	BuildRevision = "rev-a"

	st, err := Prepare(ctx, repo, false)
	if err != nil {
		t.Fatalf("prepare repo: %v", err)
	}
	st.Close()

	stOther, err := Prepare(ctx, other, false)
	if err != nil {
		t.Fatalf("prepare other: %v", err)
	}
	stOther.Close()

	// Substitute another graph for this repo's cache bytes, keeping this repo's
	// sidecars (schema and rev-a). A same-revision seed must consume it.
	otherHead := gitCmd(t, other, "rev-parse", "HEAD")
	otherCache := filepath.Join(commitCacheDir(commonDir(t, other)), otherHead+".db")
	substitute, err := os.ReadFile(otherCache)
	if err != nil {
		t.Fatalf("read other cache: %v", err)
	}
	head := gitCmd(t, repo, "rev-parse", "HEAD")
	cache := filepath.Join(commitCacheDir(commonDir(t, repo)), head+".db")
	if err := os.Remove(cache); err != nil {
		t.Fatalf("remove cache: %v", err)
	}
	if err := os.WriteFile(cache, substitute, 0o444); err != nil {
		t.Fatalf("substitute cache: %v", err)
	}

	// Control: the matching revision seeds the substituted one-file graph.
	control := addWorktree(t, repo)
	stControl, err := Prepare(ctx, control, false)
	if err != nil {
		t.Fatalf("prepare control: %v", err)
	}
	controlCounts, err := stControl.Counts(ctx)
	stControl.Close()
	if err != nil {
		t.Fatal(err)
	}
	if controlCounts.Files != 1 {
		t.Fatalf("control seed files = %d, want 1 (the substituted cache)", controlCounts.Files)
	}

	// A different revision must rebuild from the worktree, never seed the
	// one-file graph above.
	BuildRevision = "rev-b"
	wt := addWorktree(t, repo)
	stNew, err := Prepare(ctx, wt, false)
	if err != nil {
		t.Fatalf("prepare revised worktree: %v", err)
	}
	counts, err := stNew.Counts(ctx)
	stNew.Close()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Files != 2 {
		t.Fatalf("revised worktree files = %d, want 2 (rebuilt, not seeded)", counts.Files)
	}

	rev, err := os.ReadFile(cache + ".rev")
	if err != nil {
		t.Fatalf("read revision sidecar: %v", err)
	}
	if got := strings.TrimSpace(string(rev)); got != "rev-b" {
		t.Fatalf("cache revision = %q, want rev-b (the worktree must rebuild it)", got)
	}
}

func TestEvictCommitCachesRemovesRevisionSidecar(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	for i := 0; i < 7; i++ {
		path := filepath.Join(dir, fmt.Sprintf("%02d.db", i))
		for _, suffix := range []string{"", ".schema", ".rev"} {
			if err := os.WriteFile(path+suffix, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		stamp := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	evict(dir, 5)

	for _, gone := range []string{"00", "01"} {
		if _, err := os.Stat(filepath.Join(dir, gone+".db.rev")); !os.IsNotExist(err) {
			t.Fatalf("revision sidecar for %s was not evicted", gone)
		}
	}
}

func TestRunRebuildsActiveIndexWhenBuildRevisionChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write(t, root, "src/a.ts", "export function a() {}\n")
	write(t, root, "src/b.ts", "export function b() {}\n")

	old := BuildRevision
	t.Cleanup(func() { BuildRevision = old })

	open := func() *store.Store {
		st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
		if err != nil {
			t.Fatalf("open index: %v", err)
		}
		if err := st.Migrate(ctx); err != nil {
			st.Close()
			t.Fatalf("migrate: %v", err)
		}
		return st
	}

	BuildRevision = "rev-a"
	st := open()
	first, err := Run(ctx, st, root, Options{})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	st.Close()
	if first.Indexed != 2 || first.Unchanged != 0 {
		t.Fatalf("cold run = %d indexed / %d unchanged, want 2/0", first.Indexed, first.Unchanged)
	}

	// The same revision keeps the incremental path: nothing changed.
	st = open()
	same, err := Run(ctx, st, root, Options{})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	st.Close()
	if same.Indexed != 0 || same.Unchanged != 2 {
		t.Fatalf("same-revision run = %d indexed / %d unchanged, want 0/2", same.Indexed, same.Unchanged)
	}

	// A new revision re-reads every file even though the tree is unchanged.
	BuildRevision = "rev-b"
	st = open()
	revised, err := Run(ctx, st, root, Options{})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	meta, err := st.Meta(ctx)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if revised.Indexed != 2 || revised.Unchanged != 0 {
		t.Fatalf("revised run = %d indexed / %d unchanged, want a full 2/0 re-read", revised.Indexed, revised.Unchanged)
	}
	if meta["build_revision"] != "rev-b" {
		t.Fatalf("meta build_revision = %q, want rev-b", meta["build_revision"])
	}
}

func TestPrepareRebuildsActiveIndexAfterRevisionChange(t *testing.T) {
	ctx := context.Background()
	repo := initGitRepo(t, map[string]string{
		"src/a.ts": "export function a() {}\n",
	})

	old := BuildRevision
	t.Cleanup(func() { BuildRevision = old })

	BuildRevision = "rev-a"
	st, err := Prepare(ctx, repo, false)
	if err != nil {
		t.Fatalf("prepare rev-a: %v", err)
	}
	first, err := st.Meta(ctx)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if first["build_revision"] != "rev-a" {
		t.Fatalf("first meta build_revision = %q, want rev-a", first["build_revision"])
	}

	// Same HEAD, new binary: the existing active database must be re-read, not
	// served as current.
	BuildRevision = "rev-b"
	st, err = Prepare(ctx, repo, false)
	if err != nil {
		t.Fatalf("prepare rev-b: %v", err)
	}
	meta, err := st.Meta(ctx)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if meta["build_revision"] != "rev-b" {
		t.Fatalf("meta build_revision = %q, want rev-b (the index was not refreshed)", meta["build_revision"])
	}
}
