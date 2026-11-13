package index_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "repath-test@example.com")
	gitRun(t, dir, "config", "user.name", "repath test")
	gitRun(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

func writeSource(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, message string) {
	t.Helper()
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", message)
}

func openIndex(t *testing.T, root string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		st.Close()
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func runIndex(t *testing.T, root string) index.Result {
	t.Helper()
	st := openIndex(t, root)
	defer st.Close()
	result, err := index.Run(context.Background(), st, root, index.Options{})
	if err != nil {
		t.Fatalf("index run: %v", err)
	}
	return result
}

func TestIncrementalUsesGitChangeSet(t *testing.T) {
	dir := newGitRepo(t)
	writeSource(t, dir, "a.ts", "export function a() { return 1; }\n")
	writeSource(t, dir, "b.ts", "import { a } from './a';\nexport function b() { return a(); }\n")
	commitAll(t, dir, "seed")

	cold := runIndex(t, dir)
	if cold.Files != 2 || cold.Indexed != 2 {
		t.Fatalf("cold = files %d indexed %d, want 2/2", cold.Files, cold.Indexed)
	}
	if cold.Edges == 0 {
		t.Fatal("cold index produced no edges")
	}

	again := runIndex(t, dir)
	if again.Indexed != 0 || again.Unchanged != 2 {
		t.Fatalf("clean run = indexed %d unchanged %d, want 0/2", again.Indexed, again.Unchanged)
	}
	if again.Edges != cold.Edges {
		t.Fatalf("clean run edges = %d, want %d", again.Edges, cold.Edges)
	}

	// A comment-only edit changes the hash but not the facts.
	writeSource(t, dir, "a.ts", "export function a() { return 1; }\n// comment\n")
	comment := runIndex(t, dir)
	if comment.Indexed != 1 || comment.Unchanged != 1 {
		t.Fatalf("comment run = indexed %d unchanged %d, want 1/1", comment.Indexed, comment.Unchanged)
	}
	if comment.Edges != cold.Edges {
		t.Fatalf("comment run edges = %d, want %d", comment.Edges, cold.Edges)
	}

	writeSource(t, dir, "c.ts", "export function c() { return 2; }\n")
	added := runIndex(t, dir)
	if added.Indexed != 1 || added.Files != 3 {
		t.Fatalf("new file run = indexed %d files %d, want 1/3", added.Indexed, added.Files)
	}

	if err := os.Remove(filepath.Join(dir, "c.ts")); err != nil {
		t.Fatal(err)
	}
	removed := runIndex(t, dir)
	if removed.Deleted != 1 || removed.Files != 2 {
		t.Fatalf("delete run = deleted %d files %d, want 1/2", removed.Deleted, removed.Files)
	}
}

func TestColdRunCompletesPendingRetry(t *testing.T) {
	dir := newGitRepo(t)
	writeSource(t, dir, "defs.ts", "export function helper() { return 1; }\n")
	// The import target exists but does not export helper, so tree-sitter
	// leaves the call pending; the unique global name can still be linked.
	writeSource(t, dir, "empty.ts", "export const marker = 1;\n")
	writeSource(t, dir, "use.ts", "import { helper } from './empty';\nexport function run() { return helper(); }\n")
	commitAll(t, dir, "seed")

	cold := runIndex(t, dir)
	if cold.Indexed != 3 {
		t.Fatalf("cold indexed %d, want 3", cold.Indexed)
	}

	st := openIndex(t, dir)
	defer st.Close()
	edges, err := st.Edges(context.Background(), 0.5)
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	found := false
	for _, edge := range edges {
		if edge.Rule == "ts.global.unique" && edge.SrcQName == "run" && edge.DstQName == "helper" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cold index did not complete the pending retry: %+v", edges)
	}
	pending, err := st.CountPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("pending = %d, want 0 after the cold retry", pending)
	}
}

func TestIncrementalFallsBackWhenHeadChanges(t *testing.T) {
	dir := newGitRepo(t)
	writeSource(t, dir, "a.ts", "export function a() { return 1; }\n")
	commitAll(t, dir, "seed")
	if runIndex(t, dir).Indexed != 1 {
		t.Fatal("seed index did not index a.ts")
	}

	writeSource(t, dir, "d.ts", "export function d() { return 3; }\n")
	commitAll(t, dir, "add d")
	next := runIndex(t, dir)
	if next.Indexed != 1 || next.Files != 2 {
		t.Fatalf("after HEAD change = indexed %d files %d, want 1/2", next.Indexed, next.Files)
	}
}

func TestIncrementalOutsideGitRepo(t *testing.T) {
	dir := t.TempDir()
	writeSource(t, dir, "a.ts", "export function a() { return 1; }\n")
	writeSource(t, dir, "b.ts", "import { a } from './a';\nexport function b() { return a(); }\n")

	if result := runIndex(t, dir); result.Indexed != 2 {
		t.Fatalf("cold indexed %d, want 2", result.Indexed)
	}
	writeSource(t, dir, "a.ts", "export function a() { return 2; }\n")
	changed := runIndex(t, dir)
	if changed.Indexed != 1 || changed.Unchanged != 1 {
		t.Fatalf("non-git incremental = indexed %d unchanged %d, want 1/1", changed.Indexed, changed.Unchanged)
	}
}
