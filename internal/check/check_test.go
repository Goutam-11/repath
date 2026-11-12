package check

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
	"github.com/Goutam-11/repath/internal/testpath"
)

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runCheck(t *testing.T, files map[string]string, opts Options) Report {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		writeFile(t, root, name, content)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	defer st.Close()

	report, err := Run(ctx, st, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func find(report Report, kind, rule string) []Finding {
	var out []Finding
	for _, finding := range report.Findings {
		if finding.Kind == kind && (rule == "" || finding.Rule == rule) {
			out = append(out, finding)
		}
	}
	return out
}

func TestModuleCycle(t *testing.T) {
	report := runCheck(t, map[string]string{
		"alpha/a.ts": "import { b } from \"../beta/b\";\nexport function a(): number { return b(); }\n",
		"beta/b.ts":  "import { a } from \"../alpha/a\";\nexport function b(): number { return a(); }\n",
	}, Options{MinConfidence: 0.5})

	cycles := find(report, "module-cycle", "")
	if len(cycles) != 1 {
		t.Fatalf("module cycles = %d, want 1: %+v", len(cycles), report.Findings)
	}
	if cycles[0].Severity != SeverityWarning || len(cycles[0].Modules) != 2 {
		t.Fatalf("cycle = %+v", cycles[0])
	}
	if ExitCode(report, "none") != 0 || ExitCode(report, "error") != 0 {
		t.Fatal("cycles are hints and must not fail at --fail-on error")
	}
	if ExitCode(report, "warning") != ExitFindings {
		t.Fatalf("--fail-on warning = %d, want %d", ExitCode(report, "warning"), ExitFindings)
	}
}

func TestDeadCodeIsPrivateAndUnreferenced(t *testing.T) {
	report := runCheck(t, map[string]string{
		"src/a.ts": "export function used(): number {\n  return helper();\n}\n\nfunction helper(): number {\n  return 1;\n}\n\nfunction unused(): number {\n  return 2;\n}\n",
	}, Options{MinConfidence: 0.5})

	dead := find(report, "dead-code", "dead.unreferenced")
	if len(dead) != 1 {
		t.Fatalf("dead code = %d, want 1: %+v", len(dead), report.Findings)
	}
	if dead[0].QName != "unused" || dead[0].File != "src/a.ts" || dead[0].Severity != SeverityWarning {
		t.Fatalf("dead finding = %+v", dead[0])
	}
}

func TestBoundaryViolations(t *testing.T) {
	report := runCheck(t, map[string]string{
		// Production code depending on a test helper.
		"src/app.ts":     "import { ping } from \"../test/helper\";\nexport function run(): void { ping(); }\n",
		"test/helper.ts": "export function ping(): void {}\n",
		// Python private access across a module boundary.
		"app/__init__.py": "",
		"app/main.py":     "from lib.helper import _hidden\n\ndef run():\n    _hidden()\n",
		"lib/__init__.py": "",
		"lib/helper.py":   "def _hidden():\n    pass\n",
	}, Options{MinConfidence: 0.5})

	testBoundary := find(report, "module-boundary", "boundary.test")
	if len(testBoundary) != 1 {
		t.Fatalf("test-boundary findings = %d, want 1: %+v", len(testBoundary), report.Findings)
	}
	if testBoundary[0].Severity != SeverityError || testBoundary[0].QName != "ping" {
		t.Fatalf("test-boundary = %+v", testBoundary[0])
	}

	privateCrossing := find(report, "module-boundary", "boundary.private")
	if len(privateCrossing) != 1 {
		t.Fatalf("private-crossing findings = %d, want 1: %+v", len(privateCrossing), report.Findings)
	}
	if privateCrossing[0].Severity != SeverityWarning || privateCrossing[0].QName != "_hidden" {
		t.Fatalf("private-crossing = %+v", privateCrossing[0])
	}
	if privateCrossing[0].Module != "app" {
		t.Fatalf("private-crossing source module = %q, want app", privateCrossing[0].Module)
	}
}

func TestHotspotsAndUnresolvedRate(t *testing.T) {
	files := map[string]string{
		"src/hub.ts": "export function hub(): number { return 1; }\n",
		"legacy/old.ts": "export function old(): number {\n" +
			"  return missingOne() + missingTwo() + missingThree() + missingFour() + missingFive();\n" +
			"}\n",
	}
	for i := 0; i < 10; i++ {
		files["src/c"+string(rune('0'+i))+".ts"] =
			"import { hub } from \"./hub\";\nexport function c" + string(rune('0'+i)) + "(): number { return hub(); }\n"
	}
	report := runCheck(t, files, Options{MinConfidence: 0.5})

	hotspots := find(report, "hotspot", "hotspot.fan-in")
	if len(hotspots) == 0 {
		t.Fatalf("no fan-in hotspot: %+v", report.Findings)
	}
	if hotspots[0].QName != "hub" || hotspots[0].Count != 10 || hotspots[0].Severity != SeverityInfo {
		t.Fatalf("hotspot = %+v", hotspots[0])
	}

	unresolved := find(report, "unresolved-rate", "unresolved.module-rate")
	if len(unresolved) != 1 {
		t.Fatalf("unresolved-rate findings = %d, want 1: %+v", len(unresolved), report.Findings)
	}
	if unresolved[0].Module != "legacy" || unresolved[0].Count != 5 {
		t.Fatalf("unresolved-rate = %+v", unresolved[0])
	}
	if ExitCode(report, "error") != 0 {
		t.Fatal("a warning-only report must exit 0 at --fail-on error")
	}
	if ExitCode(report, "warning") != ExitFindings {
		t.Fatal("--fail-on warning must fail on the unresolved-rate warning")
	}
}

func TestReportTruncationKeepsCounts(t *testing.T) {
	files := map[string]string{
		"src/dead.ts": "",
	}
	body := ""
	for i := 0; i < 5; i++ {
		body += "function unused" + string(rune('0'+i)) + "(): number { return " + string(rune('0'+i)) + "; }\n"
	}
	files["src/dead.ts"] = body

	report := runCheck(t, files, Options{MinConfidence: 0.5, MaxFindings: 2})
	if !report.Truncated {
		t.Fatal("report was not marked truncated")
	}
	if len(report.Findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(report.Findings))
	}
	if report.Counts.Warning < 5 {
		t.Fatalf("counts = %+v, want at least 5 warnings", report.Counts)
	}
}

func TestRustPrivateCrossing(t *testing.T) {
	report := runCheck(t, map[string]string{
		"alpha/lib.rs": `pub struct Thing { value: i32 }

impl Thing {
    pub fn new() -> Thing { Thing { value: 1 } }
    fn hidden_method(&self) -> i32 { self.value }
}

fn hidden() -> i32 { 1 }

pub fn visible() -> i32 { hidden() }
`,
		"beta/lib.rs": `fn run() -> i32 {
    let thing = Thing::new();
    thing.hidden_method() + hidden()
}
`,
	}, Options{MinConfidence: 0.5})

	crossings := find(report, "module-boundary", "boundary.private")
	got := map[string]Finding{}
	for _, finding := range crossings {
		got[finding.QName] = finding
	}
	for _, want := range []string{"hidden", "Thing.hidden_method"} {
		finding, ok := got[want]
		if !ok {
			t.Fatalf("missing private crossing for %q: %+v", want, crossings)
		}
		if finding.Severity != SeverityWarning || finding.Module != "beta" {
			t.Fatalf("private crossing %q = %+v", want, finding)
		}
	}
	if len(crossings) != 2 {
		t.Fatalf("private crossings = %d, want 2: %+v", len(crossings), crossings)
	}
}

func TestTSMethodPrivacyInheritsClassExport(t *testing.T) {
	report := runCheck(t, map[string]string{
		"src/app.ts": `class Internal {
  hidden(): number { return 1; }
}

export class Public {
  visible(): number { return 2; }
}

export function makePublic(): Public { return new Public(); }
`,
	}, Options{MinConfidence: 0.5})

	dead := find(report, "dead-code", "dead.unreferenced")
	if len(dead) != 1 {
		t.Fatalf("dead code = %d, want only Internal.hidden: %+v", len(dead), dead)
	}
	if dead[0].QName != "Internal.hidden" {
		t.Fatalf("dead code = %+v, want Internal.hidden (the exported class method stays public)", dead[0])
	}
}

func TestPythonDunderAndEntrypointNamesAreNotDead(t *testing.T) {
	report := runCheck(t, map[string]string{
		"svc/app.py": `def main():
    pass

def __init__():
    pass

def __main__():
    pass

def _unused_helper():
    pass
`,
	}, Options{MinConfidence: 0.5})

	dead := find(report, "dead-code", "dead.unreferenced")
	if len(dead) != 1 || dead[0].QName != "_unused_helper" {
		t.Fatalf("dead code = %+v, want only _unused_helper", dead)
	}
}

func TestTestPathClassifier(t *testing.T) {
	cases := map[string]bool{
		"src/a.ts":             false,
		"src/a.test.ts":        true,
		"src/a.spec.ts":        true,
		"tests/helper.ts":      true,
		"test/helper.py":       true,
		"pkg/test_util.py":     true,
		"pkg/conftest.py":      true,
		"src/__tests__/a.ts":   true,
		"tests/integration.rs": true,
		"testdata/fixture.ts":  true,
		"pkg/__tests__/a.js":   true,
		"src/a.test.jsx":       true,
		"src/a.spec.jsx":       true,
		"pkg/test_client.py":   true,
		"tests/conftest.py":    true,
		"contest/winner.ts":    false,
		"testament/play.ts":    false,
	}
	for path, want := range cases {
		if got := testpath.IsTest(path); got != want {
			t.Fatalf("testpath.IsTest(%q) = %v, want %v", path, got, want)
		}
	}
}
