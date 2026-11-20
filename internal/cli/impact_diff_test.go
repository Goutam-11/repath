package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/query"
)

// initImpactDiffRepo builds a small TS graph where two changed files reach the
// same two dependent symbols:
//
//	a.ts --alpha--\
//	               use.useBoth --consume.consume
//	b.ts --beta---/
func initImpactDiffRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Repath Test")
	writeFile(t, dir, "src/a.ts", "export function alpha(): number {\n  return 1;\n}\n")
	writeFile(t, dir, "src/b.ts", "export function beta(): number {\n  return 2;\n}\n")
	writeFile(t, dir, "src/use.ts", "import { alpha } from \"./a\";\nimport { beta } from \"./b\";\nexport function useBoth(): number {\n  return alpha() + beta();\n}\n")
	writeFile(t, dir, "src/consumer.ts", "import { useBoth } from \"./use\";\nexport function consume(): number {\n  return useBoth();\n}\n")
	writeFile(t, dir, "src/dead.ts", "export function dead(): number {\n  return 0;\n}\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "base")
	return dir, gitRun(t, dir, "rev-parse", "HEAD")
}

func TestImpactDiffUnionAndOrdering(t *testing.T) {
	dir, base := initImpactDiffRepo(t)
	// Two changed production files reach the same symbols; a deleted source and
	// an added non-code file must be reported as skipped, not scored.
	writeFile(t, dir, "src/a.ts", "export function alpha(): number {\n  return 10;\n}\n")
	writeFile(t, dir, "src/b.ts", "export function beta(): number {\n  return 20;\n}\n")
	if err := os.Remove(filepath.Join(dir, "src", "dead.ts")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "notes.md", "# notes\n")
	gitRun(t, dir, "add", "notes.md")

	code, out, errOut := run(t, "impact", "--diff", base, "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("impact --diff exit = %d: %s\n%s", code, errOut, out)
	}
	var result query.ImpactDiffResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("impact --diff output is not JSON: %v\n%s", err, out)
	}
	if result.Diff == nil || result.Diff.Base != base {
		t.Fatalf("diff provenance = %+v", result.Diff)
	}
	changed := map[string]string{}
	for _, file := range result.Diff.Changed {
		changed[file.Path] = file.Status
	}
	wantChanged := map[string]string{
		"src/a.ts":    "modified",
		"src/b.ts":    "modified",
		"src/dead.ts": "deleted",
		"notes.md":    "added",
	}
	for path, status := range wantChanged {
		if changed[path] != status {
			t.Errorf("changed[%s] = %q, want %q (all %+v)", path, changed[path], status, changed)
		}
	}
	skipped := map[string]bool{}
	for _, file := range result.Diff.Skipped {
		skipped[file.Path] = true
	}
	if !skipped["src/dead.ts"] || !skipped["notes.md"] {
		t.Fatalf("skipped = %+v, want dead.ts and notes.md", result.Diff.Skipped)
	}

	// The union has one row per symbol: useBoth is reached by both changed files
	// but appears once, at depth 1, with the lexicographically first provenance.
	if len(result.Dependents) != 2 {
		t.Fatalf("dependents = %+v, want exactly 2", result.Dependents)
	}
	if got := result.Dependents[0]; got.QName != "useBoth" || got.Depth != 1 || got.ChangedFile != "src/a.ts" {
		t.Fatalf("first dependent = %+v, want useBoth depth 1 from src/a.ts", got)
	}
	if got := result.Dependents[1]; got.QName != "consume" || got.Depth != 2 || got.ChangedFile != "src/a.ts" {
		t.Fatalf("second dependent = %+v, want consume depth 2 from src/a.ts", got)
	}
	if len(result.Files) != 2 {
		t.Fatalf("files = %+v, want use.ts and consumer.ts", result.Files)
	}
	if len(result.Modules) == 0 {
		t.Fatal("module rollup is empty")
	}

	// --depth is forwarded to every per-file walk.
	code, out, errOut = run(t, "impact", "--diff", base, "--root", dir, "--depth", "1", "--json")
	if code != 0 {
		t.Fatalf("depth run exit = %d: %s", code, errOut)
	}
	var shallow query.ImpactDiffResult
	if err := json.Unmarshal([]byte(out), &shallow); err != nil {
		t.Fatal(err)
	}
	if len(shallow.Dependents) != 1 || shallow.Dependents[0].QName != "useBoth" {
		t.Fatalf("depth 1 dependents = %+v", shallow.Dependents)
	}

	// The text renderer names the provenance on each row.
	code, out, errOut = run(t, "impact", "--diff", base, "--root", dir)
	if code != 0 {
		t.Fatalf("text run exit = %d: %s", code, errOut)
	}
	for _, want := range []string{"diff vs " + base, "useBoth", "← src/a.ts", "skipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
}

func TestImpactDiffLimit(t *testing.T) {
	dir, base := initImpactDiffRepo(t)
	writeFile(t, dir, "src/a.ts", "export function alpha(): number {\n  return 10;\n}\n")
	writeFile(t, dir, "src/b.ts", "export function beta(): number {\n  return 20;\n}\n")

	code, out, errOut := run(t, "impact", "--diff", base, "--root", dir, "--limit", "1", "--json")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, errOut)
	}
	var result query.ImpactDiffResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Dependents) != 1 || !result.Truncated {
		t.Fatalf("limit 1 = %d dependents truncated=%v, want 1 true", len(result.Dependents), result.Truncated)
	}
}

func TestImpactDiffCleanAndErrors(t *testing.T) {
	dir, _ := initImpactDiffRepo(t)
	indexDir := filepath.Join(dir, ".repath")

	code, out, errOut := run(t, "impact", "--diff", "HEAD", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("clean diff exit = %d: %s", code, errOut)
	}
	var empty query.ImpactDiffResult
	if err := json.Unmarshal([]byte(out), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Diff == nil || len(empty.Diff.Changed) != 0 || len(empty.Dependents) != 0 {
		t.Fatalf("clean diff = %+v", empty)
	}
	// The clean path must not build or open an index.
	if _, err := os.Stat(indexDir); !os.IsNotExist(err) {
		t.Fatalf("clean diff touched .repath: %v", err)
	}

	code, _, errOut = run(t, "impact", "--diff", "no-such-ref", "--root", dir)
	if code != 3 || !strings.Contains(errOut, "diff base not found") {
		t.Fatalf("bad base exit = %d, stderr = %q; want 3", code, errOut)
	}

	plain := t.TempDir()
	writeFile(t, plain, "a.ts", "export function a(): void {}\n")
	code, _, errOut = run(t, "impact", "--diff", "HEAD", "--root", plain)
	if code != 3 || !strings.Contains(errOut, "not a git work tree") {
		t.Fatalf("non-repo exit = %d, stderr = %q; want 3", code, errOut)
	}

	code, _, errOut = run(t, "impact", "--diff", "HEAD", "extra", "--root", dir)
	if code != 2 || !strings.Contains(errOut, "takes no target") {
		t.Fatalf("extra target exit = %d, stderr = %q; want 2", code, errOut)
	}
}

func TestImpactSingleTargetStillWorks(t *testing.T) {
	dir, _ := initImpactDiffRepo(t)
	code, out, errOut := run(t, "impact", "alpha", "--root", dir, "--depth", "1", "--json")
	if code != 0 {
		t.Fatalf("single impact exit = %d: %s", code, errOut)
	}
	var result struct {
		Dependents []struct {
			QName string `json:"qname"`
		} `json:"dependents"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Dependents) != 1 || result.Dependents[0].QName != "useBoth" {
		t.Fatalf("dependents = %+v", result.Dependents)
	}
}
