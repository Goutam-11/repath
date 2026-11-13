package index

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitExec(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestParseStatusPathsHandlesRename(t *testing.T) {
	got := parseStatusPaths([]byte(" M a.ts\x00R  new.ts\x00old.ts\x00?? c.ts\x00"))
	for _, want := range []string{"a.ts", "new.ts", "old.ts", "c.ts"} {
		if !got[want] {
			t.Fatalf("parseStatusPaths missing %q: %+v", want, got)
		}
	}
}

func TestListPathSetsSeparatesUntrackedAndSkipWorktree(t *testing.T) {
	dir := t.TempDir()
	gitExec(t, dir, "init", "-q")
	gitExec(t, dir, "config", "user.email", "repath-test@example.com")
	gitExec(t, dir, "config", "user.name", "repath test")
	for _, name := range []string{"a.ts", "b.ts"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("export const x = 1;\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitExec(t, dir, "add", "-A")
	gitExec(t, dir, "commit", "-q", "-m", "seed")
	gitExec(t, dir, "update-index", "--skip-worktree", "b.ts")
	if err := os.WriteFile(filepath.Join(dir, "c.ts"), []byte("export const c = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	paths, untracked, err := listPathSets(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
	}
	if !seen["a.ts"] || !seen["c.ts"] {
		t.Fatalf("incremental paths = %+v, want a.ts and c.ts", seen)
	}
	if seen["b.ts"] {
		t.Fatalf("incremental paths included skip-worktree b.ts: %+v", seen)
	}
	if !untracked["c.ts"] {
		t.Fatalf("untracked = %+v, want c.ts", untracked)
	}

	full, _, err := listPathSets(context.Background(), dir, true)
	if err != nil {
		t.Fatalf("full list: %v", err)
	}
	fullSeen := map[string]bool{}
	for _, p := range full {
		fullSeen[p] = true
	}
	if !fullSeen["b.ts"] {
		t.Fatalf("full paths = %+v, want skip-worktree b.ts", fullSeen)
	}
}
