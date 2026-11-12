package cli

import (
	"encoding/json"
	"os"
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
