package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func initScopeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Repath Test")
	writeFile(t, dir, "a/one.py", "def alpha():\n    return 1\n")
	writeFile(t, dir, "a/two.py", "from a.one import alpha\n\n\ndef use():\n    return alpha()\n")
	writeFile(t, dir, "b/one.py", "def beta():\n    return 2\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "scope fixture")
	return dir
}

type scopeStatus struct {
	Files      int      `json:"files"`
	Scope      []string `json:"scope"`
	ScopeFiles int      `json:"scope_files"`
}

func statusScope(t *testing.T, root string) scopeStatus {
	t.Helper()
	code, out, errOut := run(t, "status", "--root", root, "--json")
	if code != 0 {
		t.Fatalf("status exit = %d, stderr = %s", code, errOut)
	}
	var report scopeStatus
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("status json: %v\n%s", err, out)
	}
	return report
}

func TestIndexScopeReportsAndRebuilds(t *testing.T) {
	dir := initScopeRepo(t)

	code, out, errOut := run(t, "index", "--root", dir, "--scope", "a")
	if code != 0 {
		t.Fatalf("scoped index exit = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(out, "scope a (2 files)") {
		t.Fatalf("scoped index output = %q, want the scope line", out)
	}

	report := statusScope(t, dir)
	if report.Files != 2 || report.ScopeFiles != 2 {
		t.Fatalf("status files = %d scope_files = %d, want 2/2", report.Files, report.ScopeFiles)
	}
	if len(report.Scope) != 1 || report.Scope[0] != "a" {
		t.Fatalf("status scope = %v, want [a]", report.Scope)
	}

	// The text status names the scope as well.
	code, text, errOut := run(t, "status", "--root", dir)
	if code != 0 {
		t.Fatalf("status exit = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(text, "scope a (2 files)") {
		t.Fatalf("status text = %q, want the scope line", text)
	}

	// A plain `index` asks for the whole repository: it rebuilds and clears the
	// recorded scope.
	code, out, errOut = run(t, "index", "--root", dir)
	if code != 0 {
		t.Fatalf("whole index exit = %d, stderr = %s", code, errOut)
	}
	if strings.Contains(out, "\nscope ") {
		t.Fatalf("plain index still reports a scope: %q", out)
	}
	report = statusScope(t, dir)
	if report.Files != 3 || report.ScopeFiles != 0 || len(report.Scope) != 0 {
		t.Fatalf("after whole index: files=%d scope_files=%d scope=%v, want 3/0/none",
			report.Files, report.ScopeFiles, report.Scope)
	}
}

func TestIndexScopeJSONAndUsageErrors(t *testing.T) {
	dir := initScopeRepo(t)

	code, out, errOut := run(t, "index", "--root", dir, "--scope", "b", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut)
	}
	var result struct {
		Files int      `json:"files"`
		Scope []string `json:"scope"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("index json: %v\n%s", err, out)
	}
	if result.Files != 1 || len(result.Scope) != 1 || result.Scope[0] != "b" {
		t.Fatalf("index json = %+v, want files=1 scope=[b]", result)
	}

	cases := [][]string{
		{"index", "--root", dir, "--scope", "../escape"},
		{"index", "--root", dir, "--scope", "/tmp"},
		{"index", "--root", dir, "--scope", "."},
		{"index", "--root", dir, "--scope", "missing-dir"},
		{"index", "--root", dir, "--scope"},
	}
	for _, args := range cases {
		if code, _, _ := run(t, args...); code != 2 {
			t.Fatalf("%v exit code = %d, want 2", args, code)
		}
	}
}
