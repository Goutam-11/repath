package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/query"
)

func copyTestsFixture(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "fixtures", "tests")
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

func TestTestsCommand(t *testing.T) {
	dir := copyTestsFixture(t)

	code, out, errOut := run(t, "tests", "total", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("tests exit = %d: %s", code, errOut)
	}
	var result query.TestsResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("tests output is not JSON: %v\n%s", err, out)
	}
	if len(result.Tests) != 4 {
		t.Fatalf("test files = %d, want 4: %+v", len(result.Tests), result.Tests)
	}
	rules := map[string]string{}
	for _, file := range result.Tests {
		rules[file.File] = file.Links[0].Rule
	}
	want := map[string]string{
		"src/cart.test.ts":        "ts.import.named",
		"src/checkout.test.ts":    "ts.import.named",
		"src/cart.spec.ts":        "testpath.convention",
		"src/import-only.test.ts": "testpath.import",
	}
	for file, rule := range want {
		if rules[file] != rule {
			t.Fatalf("test file %s rule = %q, want %q (all %+v)", file, rules[file], rule, rules)
		}
	}

	code, out, _ = run(t, "tests", "total", "--root", dir)
	if code != 0 || !strings.Contains(out, "structural reachability") || !strings.Contains(out, "src/checkout.test.ts") {
		t.Fatalf("text tests output = %d\n%s", code, out)
	}

	code, out, _ = run(t, "tests", "unusedByTests", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("no-test target exit = %d", code)
	}
	var empty query.TestsResult
	if err := json.Unmarshal([]byte(out), &empty); err != nil {
		t.Fatal(err)
	}
	if len(empty.Tests) != 0 {
		t.Fatalf("unexpected tests for unusedByTests: %+v", empty.Tests)
	}

	code, _, _ = run(t, "tests", "not_a_symbol", "--root", dir)
	if code != 3 {
		t.Fatalf("missing target exit = %d, want 3", code)
	}
}

func TestTestsLimitTruncates(t *testing.T) {
	dir := copyTestsFixture(t)
	code, out, errOut := run(t, "tests", "total", "--root", dir, "--limit", "1", "--json")
	if code != 0 {
		t.Fatalf("tests exit = %d: %s", code, errOut)
	}
	var result query.TestsResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Tests) != 1 || !result.Truncated {
		t.Fatalf("limited result = %d files, truncated=%v", len(result.Tests), result.Truncated)
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
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

func initTestsDiffRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Repath Test")
	writeFile(t, dir, "src/cart.ts", "export function total(): number {\n  return 1;\n}\n")
	writeFile(t, dir, "src/cart.test.ts", "import { total } from \"./cart\";\nexport function cartTest(): number {\n  return total();\n}\n")
	writeFile(t, dir, "src/other.test.ts", "export function otherTest(): number {\n  return 1;\n}\n")
	writeFile(t, dir, "src/old.ts", "export function old(): number {\n  return 1;\n}\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "base")
	return dir, gitRun(t, dir, "rev-parse", "HEAD")
}

func TestTestsDiffUnionAndProvenance(t *testing.T) {
	dir, base := initTestsDiffRepo(t)

	// One changed production file, one changed test file, one added file, one
	// deleted file.
	writeFile(t, dir, "src/cart.ts", "export function total(): number {\n  return 2;\n}\n")
	writeFile(t, dir, "src/cart.test.ts", "import { total } from \"./cart\";\nexport function cartTest(): number {\n  return total() + 1;\n}\n")
	writeFile(t, dir, "src/fresh.ts", "export function fresh(): number {\n  return 3;\n}\n")
	gitRun(t, dir, "add", "src/fresh.ts")
	if err := os.Remove(filepath.Join(dir, "src", "old.ts")); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run(t, "tests", "--diff", base, "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("tests --diff exit = %d: %s\n%s", code, errOut, out)
	}
	var result query.TestsResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("tests --diff output is not JSON: %v\n%s", err, out)
	}
	if result.Diff == nil {
		t.Fatal("diff provenance is missing")
	}
	if result.Diff.Base != base {
		t.Fatalf("diff base = %q, want %q", result.Diff.Base, base)
	}
	changed := map[string]string{}
	for _, file := range result.Diff.Changed {
		changed[file.Path] = file.Status
	}
	wantChanged := map[string]string{
		"src/cart.ts":      "modified",
		"src/cart.test.ts": "modified",
		"src/fresh.ts":     "added",
		"src/old.ts":       "deleted",
	}
	for path, status := range wantChanged {
		if changed[path] != status {
			t.Errorf("changed[%s] = %q, want %q (all %+v)", path, changed[path], status, changed)
		}
	}
	if len(result.Diff.Skipped) != 1 || result.Diff.Skipped[0].Path != "src/old.ts" {
		t.Fatalf("skipped = %+v, want the deleted old.ts", result.Diff.Skipped)
	}

	if len(result.Tests) != 1 || result.Tests[0].File != "src/cart.test.ts" {
		t.Fatalf("tests = %+v, want only src/cart.test.ts", result.Tests)
	}
	rules := map[string]query.TestLink{}
	for _, link := range result.Tests[0].Links {
		rules[link.Rule+"\x00"+link.ChangedFile] = link
	}
	graphLink, ok := rules["ts.import.named\x00src/cart.ts"]
	if !ok {
		t.Fatalf("missing graph link from src/cart.ts: %+v", result.Tests[0].Links)
	}
	if graphLink.TestSymbol != "cartTest" || graphLink.AffectedSymbol != "total" || graphLink.Depth != 1 {
		t.Fatalf("graph link = %+v", graphLink)
	}
	changedTest, ok := rules["testpath.changed\x00src/cart.test.ts"]
	if !ok {
		t.Fatalf("missing changed-test link: %+v", result.Tests[0].Links)
	}
	if changedTest.Confidence != 1 || changedTest.Depth != 0 {
		t.Fatalf("changed-test link = %+v", changedTest)
	}
	for _, test := range result.Tests {
		if test.File == "src/other.test.ts" {
			t.Fatal("unrelated test file was included")
		}
	}

	// Text output names the base and the changed file that pulled the test in.
	code, out, _ = run(t, "tests", "--diff", base, "--root", dir)
	if code != 0 {
		t.Fatalf("text tests --diff exit = %d\n%s", code, out)
	}
	for _, want := range []string{"diff vs ", "src/cart.test.ts", "← src/cart.ts", "skipped (no current symbols)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text output missing %q:\n%s", want, out)
		}
	}
}

func TestTestsDiffCleanAndErrors(t *testing.T) {
	dir, _ := initTestsDiffRepo(t)

	code, out, errOut := run(t, "tests", "--diff", "HEAD", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("clean diff exit = %d: %s", code, errOut)
	}
	var empty query.TestsResult
	if err := json.Unmarshal([]byte(out), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Diff == nil || len(empty.Diff.Changed) != 0 || len(empty.Tests) != 0 {
		t.Fatalf("clean diff = %+v", empty)
	}

	code, _, errOut = run(t, "tests", "--diff", "no-such-ref", "--root", dir)
	if code != 3 || !strings.Contains(errOut, "diff base not found") {
		t.Fatalf("bad base exit = %d, stderr = %q; want 3", code, errOut)
	}

	plain := t.TempDir()
	writeFile(t, plain, "a.ts", "export function a(): void {}\n")
	code, _, errOut = run(t, "tests", "--diff", "HEAD", "--root", plain)
	if code != 3 || !strings.Contains(errOut, "not a git work tree") {
		t.Fatalf("non-repo exit = %d, stderr = %q; want 3", code, errOut)
	}

	code, _, errOut = run(t, "tests", "--diff", "HEAD", "extra", "--root", dir)
	if code != 2 || !strings.Contains(errOut, "takes no target") {
		t.Fatalf("extra target exit = %d, stderr = %q; want 2", code, errOut)
	}
}
