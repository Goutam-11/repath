package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestChangedFilesAndBase(t *testing.T) {
	repo := initGitRepo(t, map[string]string{
		"src/a.ts":       "export function a(): number { return 1; }\n",
		"src/b.ts":       "export function b(): number { return 1; }\n",
		"src/old.ts":     "export function old(): number { return 1; }\n",
		"docs/readme.md": "# notes\n",
	})
	base := gitCmd(t, repo, "rev-parse", "HEAD")

	write(t, repo, "src/a.ts", "export function a(): number { return 2; }\n")
	write(t, repo, "src/new.ts", "export function fresh(): number { return 3; }\n")
	gitCmd(t, repo, "add", "src/new.ts")
	if err := os.Remove(filepath.Join(repo, "src", "old.ts")); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo, "mv", "src/b.ts", "src/renamed.ts")

	ctx := context.Background()
	changed, err := ChangedFiles(ctx, repo, base)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string, len(changed))
	for _, file := range changed {
		got[file.Path] = file.Status
	}
	want := map[string]string{
		"src/a.ts":       "modified",
		"src/new.ts":     "added",
		"src/old.ts":     "deleted",
		"src/renamed.ts": "renamed",
	}
	if len(got) != len(want) {
		t.Fatalf("changed = %+v, want %+v", got, want)
	}
	for path, status := range want {
		if got[path] != status {
			t.Errorf("changed[%s] = %q, want %q", path, got[path], status)
		}
	}
	for i := 1; i < len(changed); i++ {
		if changed[i-1].Path > changed[i].Path {
			t.Fatalf("changed files are not sorted: %+v", changed)
		}
	}

	if _, err := ChangedFiles(ctx, repo, "does-not-exist"); !errors.Is(err, ErrDiffBaseNotFound) {
		t.Fatalf("bad base error = %v, want ErrDiffBaseNotFound", err)
	}
	if _, err := ChangedFiles(ctx, t.TempDir(), "HEAD"); !errors.Is(err, ErrNotGitRepo) {
		t.Fatalf("non-repo error = %v, want ErrNotGitRepo", err)
	}
}

func TestResolveDiffBase(t *testing.T) {
	repo := initGitRepo(t, map[string]string{"a.ts": "export function a(): void {}\n"})
	ctx := context.Background()

	base, err := ResolveDiffBase(ctx, repo, "HEAD")
	if err != nil || base != "HEAD" {
		t.Fatalf("explicit base = %q, %v; want HEAD", base, err)
	}
	if _, err := ResolveDiffBase(ctx, repo, "nope"); !errors.Is(err, ErrDiffBaseNotFound) {
		t.Fatalf("explicit bad base error = %v", err)
	}
	// No origin/main or main in a fresh test repo, so the documented fallback
	// lands on HEAD.
	base, err = ResolveDiffBase(ctx, repo, "")
	if err != nil || base != "HEAD" {
		t.Fatalf("fallback base = %q, %v; want HEAD", base, err)
	}
}

func TestParseNameStatus(t *testing.T) {
	out := []byte("M\x00src/a.ts\x00A\x00src/new.ts\x00D\x00src/old.ts\x00R100\x00src/b.ts\x00src/c.ts\x00")
	changed, err := parseNameStatus(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []ChangedFile{
		{Path: "src/a.ts", Status: "modified"},
		{Path: "src/c.ts", Status: "renamed"},
		{Path: "src/new.ts", Status: "added"},
		{Path: "src/old.ts", Status: "deleted"},
	}
	if len(changed) != len(want) {
		t.Fatalf("changed = %+v, want %+v", changed, want)
	}
	for i := range want {
		if changed[i] != want[i] {
			t.Errorf("changed[%d] = %+v, want %+v", i, changed[i], want[i])
		}
	}
}

func TestChangedFilesRelativeToSubdirectoryRoot(t *testing.T) {
	repo := initGitRepo(t, map[string]string{
		"pkg/src/a.ts": "export function a(): number { return 1; }\n",
		"outside.ts":   "export function outside(): number { return 1; }\n",
	})
	base := gitCmd(t, repo, "rev-parse", "HEAD")
	write(t, repo, "pkg/src/a.ts", "export function a(): number { return 2; }\n")
	write(t, repo, "outside.ts", "export function outside(): number { return 2; }\n")

	changed, err := ChangedFiles(context.Background(), filepath.Join(repo, "pkg"), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].Path != "src/a.ts" || changed[0].Status != "modified" {
		t.Fatalf("changed = %+v, want only src/a.ts relative to pkg", changed)
	}
}
