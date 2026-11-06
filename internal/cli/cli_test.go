package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVersion(t *testing.T) {
	code, out, _ := run(t, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := strings.TrimSpace(out); got != version {
		t.Fatalf("stdout = %q, want %q", got, version)
	}
}

func TestNoArgsIsUsageError(t *testing.T) {
	code, out, errOut := run(t)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if !strings.Contains(errOut, "Usage:") {
		t.Fatalf("stderr missing usage:\n%s", errOut)
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	code, _, errOut := run(t, "nope")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, `unknown command "nope"`) {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestStatusRejectsUnknownFlag(t *testing.T) {
	code, _, errOut := run(t, "status", "--verbose")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "unknown argument") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestStatusIndexesRepoAndReusesIt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "app.ts", "export function run(x: number) {\n  return x;\n}\n")
	writeFile(t, dir, "lib.js", "function helper() {\n  return 1;\n}\n")
	writeFile(t, dir, "notes.md", "# not code\n")

	code, out, errOut := run(t, "status", "--json", "--root", dir)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, errOut)
	}

	var first statusReport
	if err := json.Unmarshal([]byte(out), &first); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if !first.Indexed {
		t.Fatal("indexed = false after a run")
	}
	if first.Files != 2 {
		t.Fatalf("files = %d, want 2 (markdown must be ignored)", first.Files)
	}
	if first.Nodes != 2 {
		t.Fatalf("nodes = %d, want 2", first.Nodes)
	}
	if first.ParseFailures != 0 {
		t.Fatalf("parse_failures = %d, want 0", first.ParseFailures)
	}
	if first.Refresh.FilesIndexed != 2 {
		t.Fatalf("files_indexed = %d, want 2", first.Refresh.FilesIndexed)
	}

	if _, err := os.Stat(filepath.Join(dir, ".repath", "index.db")); err != nil {
		t.Fatalf("index db not created: %v", err)
	}

	_, out, _ = run(t, "status", "--json", "--root", dir)
	var second statusReport
	if err := json.Unmarshal([]byte(out), &second); err != nil {
		t.Fatalf("second run stdout is not JSON: %v\n%s", err, out)
	}
	if second.Refresh.FilesIndexed != 0 || second.Refresh.FilesSkipped != 2 {
		t.Fatalf("second run indexed=%d skipped=%d, want 0/2",
			second.Refresh.FilesIndexed, second.Refresh.FilesSkipped)
	}
	if second.Nodes != 2 {
		t.Fatalf("nodes changed between runs: %d", second.Nodes)
	}
}

func TestStatusNoRefreshSkipsIndexing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.ts", "function a() {}\n")

	code, _, errOut := run(t, "status", "--json", "--no-refresh", "--root", dir)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, errOut)
	}

	code, out, _ := run(t, "status", "--json", "--root", dir)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	var report statusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if report.Files != 1 || report.Nodes != 1 {
		t.Fatalf("files=%d nodes=%d, want 1/1", report.Files, report.Nodes)
	}
}
