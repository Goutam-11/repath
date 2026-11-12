package index

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
