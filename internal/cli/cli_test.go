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

func TestCallersAndFind(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.ts", "export function add(a: number, b: number): number {\n  return a + b;\n}\n")
	writeFile(t, dir, "b.ts", "import { add } from \"./a\";\n\nexport function sum(): number {\n  return add(1, 2);\n}\n")

	if code, _, errOut := run(t, "status", "--json", "--root", dir); code != 0 {
		t.Fatalf("status exit = %d: %s", code, errOut)
	}

	code, out, errOut := run(t, "callers", "add", "--json", "--root", dir)
	if code != 0 {
		t.Fatalf("callers exit = %d: %s", code, errOut)
	}

	var callers struct {
		Symbol struct {
			QName string `json:"qname"`
			Kind  string `json:"kind"`
			File  string `json:"file"`
		} `json:"symbol"`
		Callers []struct {
			QName      string  `json:"qname"`
			Kind       string  `json:"kind"`
			Confidence float64 `json:"confidence"`
		} `json:"callers"`
	}
	if err := json.Unmarshal([]byte(out), &callers); err != nil {
		t.Fatalf("callers output is not JSON: %v\n%s", err, out)
	}
	if callers.Symbol.QName != "add" {
		t.Fatalf("symbol = %q, want add", callers.Symbol.QName)
	}
	if len(callers.Callers) != 1 {
		t.Fatalf("callers = %d, want 1: %s", len(callers.Callers), out)
	}
	if callers.Callers[0].QName != "sum" || callers.Callers[0].Kind != "CALLS" {
		t.Fatalf("caller = %+v, want sum/CALLS", callers.Callers[0])
	}
	if callers.Callers[0].Confidence < 0.99 {
		t.Fatalf("import-resolved caller confidence = %.2f, want 1.0", callers.Callers[0].Confidence)
	}

	code, out, _ = run(t, "find", "add", "--json", "--root", dir)
	if code != 0 {
		t.Fatalf("find exit = %d", code)
	}
	var card struct {
		Symbol  map[string]any `json:"symbol"`
		Callers int            `json:"callers"`
		Callees int            `json:"callees"`
	}
	if err := json.Unmarshal([]byte(out), &card); err != nil {
		t.Fatalf("find output is not JSON: %v\n%s", err, out)
	}
	if card.Callers != 1 {
		t.Fatalf("find callers = %d, want 1", card.Callers)
	}
}

func TestFindMissingSymbol(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.ts", "export function add() {}\n")

	code, _, errOut := run(t, "find", "nope", "--root", dir)
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (not found)\n%s", code, errOut)
	}
}

func TestFindAmbiguousSymbol(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.ts", "export function run(): void {}\n")
	writeFile(t, dir, "b.ts", "export function run(): void {}\n")

	code, out, _ := run(t, "find", "run", "--json", "--root", dir)
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (ambiguous)", code)
	}

	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("ambiguous output is not JSON: %v\n%s", err, out)
	}
	if payload.Error != "ambiguous" {
		t.Fatalf("error = %q, want ambiguous", payload.Error)
	}
}

func TestImpactPathAndMap(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.ts", "export function add(a: number, b: number): number {\n  return a + b;\n}\n")
	writeFile(t, dir, "b.ts", "import { add } from \"./a\";\n\nexport function sum(): number {\n  return add(1, 2);\n}\n")
	writeFile(t, dir, "c.ts", "import { sum } from \"./b\";\n\nexport function app(): number {\n  return sum();\n}\n")

	if code, _, errOut := run(t, "status", "--json", "--root", dir); code != 0 {
		t.Fatalf("status exit = %d: %s", code, errOut)
	}

	code, out, errOut := run(t, "impact", "add", "--json", "--root", dir)
	if code != 0 {
		t.Fatalf("impact exit = %d: %s", code, errOut)
	}
	var impact struct {
		Dependents []struct {
			QName      string  `json:"qname"`
			Depth      int     `json:"depth"`
			Confidence float64 `json:"confidence"`
		} `json:"dependents"`
		Files []struct {
			Path    string `json:"path"`
			Symbols int    `json:"symbols"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &impact); err != nil {
		t.Fatalf("impact output is not JSON: %v\n%s", err, out)
	}
	if len(impact.Dependents) != 2 {
		t.Fatalf("dependents = %d, want 2 (sum at depth 1, app at depth 2)", len(impact.Dependents))
	}
	byQName := map[string]int{}
	for _, d := range impact.Dependents {
		byQName[d.QName] = d.Depth
		if d.Confidence < 0.99 {
			t.Fatalf("%s confidence = %.2f, want 1.0 (import edge)", d.QName, d.Confidence)
		}
	}
	if byQName["sum"] != 1 || byQName["app"] != 2 {
		t.Fatalf("depths = %+v, want sum:1 app:2", byQName)
	}
	if len(impact.Files) != 2 {
		t.Fatalf("affected files = %d, want 2", len(impact.Files))
	}

	code, out, _ = run(t, "path", "app", "add", "--json", "--root", dir)
	if code != 0 {
		t.Fatalf("path exit = %d", code)
	}
	var pathResult struct {
		Hops []struct {
			Kind       string  `json:"kind"`
			Confidence float64 `json:"confidence"`
		} `json:"hops"`
	}
	if err := json.Unmarshal([]byte(out), &pathResult); err != nil {
		t.Fatalf("path output is not JSON: %v\n%s", err, out)
	}
	if len(pathResult.Hops) != 2 {
		t.Fatalf("hops = %d, want 2 (app → sum → add)", len(pathResult.Hops))
	}

	code, out, _ = run(t, "map", "--json", "--root", dir)
	if code != 0 {
		t.Fatalf("map exit = %d", code)
	}
	var mapResult struct {
		Modules []struct {
			Name string `json:"name"`
		} `json:"modules"`
		Top []struct {
			QName string `json:"qname"`
		} `json:"top"`
	}
	if err := json.Unmarshal([]byte(out), &mapResult); err != nil {
		t.Fatalf("map output is not JSON: %v\n%s", err, out)
	}
	if len(mapResult.Modules) == 0 || len(mapResult.Top) == 0 {
		t.Fatalf("map is empty: %s", out)
	}
}

func TestImpactUnknownTarget(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.ts", "export function add() {}\n")

	code, _, _ := run(t, "impact", "nope", "--root", dir)
	if code != 3 {
		t.Fatalf("exit = %d, want 3", code)
	}
}
