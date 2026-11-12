package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/check"
)

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
