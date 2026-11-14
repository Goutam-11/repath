package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/check"
)

func copySARIFFixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "fixtures", "sarif", name)
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

func TestCheckCycleIsWarningAndGated(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "alpha/a.ts", "import { b } from \"../beta/b\";\nexport function a(): number { return b(); }\n")
	writeFile(t, dir, "beta/b.ts", "import { a } from \"../alpha/a\";\nexport function b(): number { return a(); }\n")

	code, out, errOut := run(t, "check", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("cycle-only check exit = %d, want 0 (cycles are hints)\nstderr: %s", code, errOut)
	}
	var report check.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("check output is not JSON: %v\n%s", err, out)
	}
	if len(report.Findings) == 0 || report.Findings[0].Kind != "module-cycle" {
		t.Fatalf("findings = %+v, want a module-cycle", report.Findings)
	}
	if report.Counts.Warning == 0 || report.Counts.Error != 0 {
		t.Fatalf("counts = %+v, want a warning and no error", report.Counts)
	}

	code, _, _ = run(t, "check", "--root", dir, "--fail-on", "warning", "--json")
	if code != check.ExitFindings {
		t.Fatalf("--fail-on warning exit = %d, want %d", code, check.ExitFindings)
	}
	code, _, _ = run(t, "check", "--root", dir, "--fail-on", "none", "--json")
	if code != 0 {
		t.Fatalf("--fail-on none exit = %d, want 0", code)
	}
}

func TestCheckFailsOnTestBoundaryByDefault(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/app.ts", "import { ping } from \"../test/helper\";\nexport function run(): void { ping(); }\n")
	writeFile(t, dir, "test/helper.ts", "export function ping(): void {}\n")

	code, out, errOut := run(t, "check", "--root", dir, "--json")
	if code != check.ExitFindings {
		t.Fatalf("test-boundary check exit = %d, want %d\nstderr: %s", code, check.ExitFindings, errOut)
	}
	var report check.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("check output is not JSON: %v\n%s", err, out)
	}
	if report.Counts.Error == 0 {
		t.Fatalf("counts = %+v, want an error", report.Counts)
	}
}

func TestCheckWarningOnlyPassesByDefault(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/a.ts", "export function used(): void {}\nfunction unused(): void {}\n")

	code, out, errOut := run(t, "check", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("warning-only check exit = %d, want 0\nstderr: %s\n%s", code, errOut, out)
	}
	var report check.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("check output is not JSON: %v\n%s", err, out)
	}
	if report.Counts.Warning == 0 {
		t.Fatalf("counts = %+v, want a warning", report.Counts)
	}
	if report.Counts.Error != 0 {
		t.Fatalf("counts = %+v, want no errors", report.Counts)
	}

	code, _, _ = run(t, "check", "--root", dir, "--fail-on", "warning", "--json")
	if code != check.ExitFindings {
		t.Fatalf("--fail-on warning exit = %d, want %d", code, check.ExitFindings)
	}

	code, out, _ = run(t, "check", "--root", dir)
	if code != 0 {
		t.Fatalf("text check exit = %d, want 0", code)
	}
	if !strings.Contains(out, "dead-code") || !strings.Contains(out, "unused") {
		t.Fatalf("text output missing dead-code finding:\n%s", out)
	}
}

func TestCheckRejectsBadArguments(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.ts", "export function a(): void {}\n")

	for _, args := range [][]string{
		{"check", "--root", dir, "--fail-on", "nope"},
		{"check", "extra", "--root", dir},
		{"check", "--root", dir, "--max-findings", "0"},
	} {
		code, _, errOut := run(t, args...)
		if code != 2 {
			t.Fatalf("%v exit = %d, want 2 (stderr %q)", args, code, errOut)
		}
	}
}

func TestCheckSARIFDiagnostics(t *testing.T) {
	dir := copySARIFFixture(t, "ts")
	sarifPath := filepath.Join(dir, "tsc.sarif")

	code, out, errOut := run(t, "check", "--root", dir, "--sarif", sarifPath, "--json")
	if code != check.ExitFindings {
		t.Fatalf("check --sarif exit = %d, want %d\nstderr: %s", code, check.ExitFindings, errOut)
	}
	var report check.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("check output is not JSON: %v\n%s", err, out)
	}
	if report.Diagnostics == nil {
		t.Fatal("diagnostics summary is missing")
	}
	if report.Diagnostics.Results != 5 || report.Diagnostics.Attached != 3 ||
		report.Diagnostics.Skipped != 1 || report.Diagnostics.Duplicates != 1 {
		t.Fatalf("diagnostics = %+v", report.Diagnostics)
	}
	byRule := map[string]check.Finding{}
	for _, finding := range report.Findings {
		if finding.Kind == "diagnostic" {
			byRule[finding.Rule] = finding
		}
	}
	if len(byRule) != 3 {
		t.Fatalf("diagnostic findings = %d, want 3: %+v", len(byRule), report.Findings)
	}
	if f, ok := byRule["sarif.tsc.TS2322"]; !ok || f.QName != "Cart.total" || f.Severity != "error" {
		t.Fatalf("TS2322 finding = %+v (all %+v)", f, byRule)
	}
	if f, ok := byRule["sarif.tsc.TS2532"]; !ok || f.QName != "run" || f.Severity != "warning" {
		t.Fatalf("TS2532 finding = %+v", f)
	}
	if f, ok := byRule["sarif.tsc.TS6133"]; !ok || f.Severity != "info" {
		t.Fatalf("TS6133 finding = %+v", f)
	}

	code, _, _ = run(t, "check", "--root", dir, "--sarif", sarifPath, "--fail-on", "none", "--json")
	if code != 0 {
		t.Fatalf("--fail-on none exit = %d, want 0", code)
	}
	code, _, _ = run(t, "check", "--root", dir, "--sarif", sarifPath, "--fail-on", "warning", "--json")
	if code != check.ExitFindings {
		t.Fatalf("--fail-on warning exit = %d, want %d", code, check.ExitFindings)
	}

	code, out, _ = run(t, "check", "--root", dir, "--sarif", sarifPath)
	if code != check.ExitFindings {
		t.Fatalf("text check exit = %d, want %d\n%s", code, check.ExitFindings, out)
	}
	if !strings.Contains(out, "SARIF: 1 source(s) · 5 results · 3 attached · 1 skipped · 1 duplicate(s)") {
		t.Fatalf("text output missing the SARIF summary:\n%s", out)
	}
	if !strings.Contains(out, "diagnostic") || !strings.Contains(out, "Cart.total") {
		t.Fatalf("text output missing a diagnostic finding:\n%s", out)
	}
}

func TestCheckWithoutSARIFIsUnchanged(t *testing.T) {
	dir := copySARIFFixture(t, "ts")

	code, out, errOut := run(t, "check", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("structural-only check exit = %d, want 0\nstderr: %s\n%s", code, errOut, out)
	}
	if strings.Contains(out, `"diagnostics"`) || strings.Contains(out, `"kind":"diagnostic"`) {
		t.Fatalf("structural-only output gained SARIF fields:\n%s", out)
	}
	var report check.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Diagnostics != nil {
		t.Fatalf("diagnostics = %+v, want nil", report.Diagnostics)
	}
}

func TestCheckSARIFErrorsAreClean(t *testing.T) {
	dir := copySARIFFixture(t, "ts")

	code, _, errOut := run(t, "check", "--root", dir, "--sarif", filepath.Join(dir, "missing.sarif"))
	if code != 3 || !strings.Contains(errOut, "not found") {
		t.Fatalf("missing SARIF exit = %d, stderr = %q; want 3 and a not-found message", code, errOut)
	}

	bad := filepath.Join(dir, "old.sarif")
	if err := os.WriteFile(bad, []byte(`{"version":"2.0.0","runs":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = run(t, "check", "--root", dir, "--sarif", bad)
	if code != 2 || !strings.Contains(errOut, "only 2.1.0") {
		t.Fatalf("bad-version SARIF exit = %d, stderr = %q; want 2 and a 2.1.0 message", code, errOut)
	}

	code, _, errOut = run(t, "check", "--root", dir, "--sarif")
	if code != 2 || !strings.Contains(errOut, "--sarif needs") {
		t.Fatalf("bare --sarif exit = %d, stderr = %q", code, errOut)
	}
}

func TestCheckSARIFRepeatableAndDirectory(t *testing.T) {
	dir := copySARIFFixture(t, "ts")
	copyDirInto(t, filepath.Join("..", "..", "testdata", "fixtures", "sarif", "py", "app"), filepath.Join(dir, "app"))
	for _, name := range []string{"tsc.sarif", "ruff.sarif"} {
		src := filepath.Join("..", "..", "testdata", "fixtures", "sarif")
		if name == "ruff.sarif" {
			src = filepath.Join(src, "py")
		} else {
			src = filepath.Join(src, "ts")
		}
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	code, out, errOut := run(t, "check", "--root", dir,
		"--sarif", filepath.Join(dir, "tsc.sarif"),
		"--sarif", filepath.Join(dir, "ruff.sarif"), "--json")
	if code != check.ExitFindings {
		t.Fatalf("repeatable --sarif exit = %d, want %d\nstderr: %s", code, check.ExitFindings, errOut)
	}
	var report check.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Diagnostics == nil || report.Diagnostics.Sources != 2 ||
		report.Diagnostics.Attached != 5 || report.Diagnostics.Skipped != 2 ||
		report.Diagnostics.Duplicates != 1 {
		t.Fatalf("combined diagnostics = %+v", report.Diagnostics)
	}

	code, out, errOut = run(t, "check", "--root", dir, "--sarif", dir, "--json")
	if code != check.ExitFindings {
		t.Fatalf("directory --sarif exit = %d, want %d\nstderr: %s", code, check.ExitFindings, errOut)
	}
	var directoryReport check.Report
	if err := json.Unmarshal([]byte(out), &directoryReport); err != nil {
		t.Fatal(err)
	}
	if directoryReport.Diagnostics == nil || directoryReport.Diagnostics.Sources != 2 {
		t.Fatalf("directory diagnostics = %+v", directoryReport.Diagnostics)
	}
}

func copyDirInto(t *testing.T, src, dst string) {
	t.Helper()
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
		t.Fatalf("copy dir: %v", err)
	}
}

func TestCheckReportsUnresolvedSummary(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/a.ts", "export function run(): void {\n  unknownThing();\n}\n")

	code, out, errOut := run(t, "check", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("check exit = %d, want 0: %s", code, errOut)
	}
	var report check.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("check output is not JSON: %v\n%s", err, out)
	}
	if report.Unresolved.Total != 1 {
		t.Fatalf("check unresolved total = %d, want 1", report.Unresolved.Total)
	}
	if report.Unresolved.ByReason["missing-definition"] != 1 {
		t.Fatalf("check unresolved_by_reason = %+v, want one missing-definition", report.Unresolved.ByReason)
	}
	sum := 0
	for _, count := range report.Unresolved.ByReason {
		sum += count
	}
	if sum != report.Unresolved.Total {
		t.Fatalf("check reason sum = %d, want total %d", sum, report.Unresolved.Total)
	}

	// check and status share one classifier, so their totals must agree.
	_, statusOut, _ := run(t, "status", "--json", "--root", dir)
	var status statusReport
	if err := json.Unmarshal([]byte(statusOut), &status); err != nil {
		t.Fatalf("status output is not JSON: %v\n%s", err, statusOut)
	}
	if status.Unresolved != report.Unresolved.Total {
		t.Fatalf("status unresolved = %d, check unresolved = %d", status.Unresolved, report.Unresolved.Total)
	}
}
