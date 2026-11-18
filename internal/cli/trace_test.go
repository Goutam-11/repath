package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/trace"
)

func traceFixtureDir() string { return filepath.Join("trace", "cpu") }

func traceProfilePath() string {
	return filepath.Join("..", "..", "testdata", "fixtures", "trace", "cpu", "profile.cpuprofile")
}

func TestTraceCommand(t *testing.T) {
	dir := copyAdapterFixtureNamed(t, traceFixtureDir())
	profile := traceProfilePath()

	code, out, errOut := run(t, "trace", "--file", profile, "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("trace exit = %d: %s", code, errOut)
	}
	var report trace.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("trace output is not JSON: %v\n%s", err, out)
	}
	if report.Mapped != 3 || report.ObservedEdges != 2 || report.Confirmed != 1 || report.ObservedOnly != 1 {
		t.Fatalf("report = %+v, want 3 mapped / 2 edges / 1 confirmed / 1 observed-only", report)
	}

	code, text, errOut := run(t, "trace", "--file", profile, "--root", dir)
	if code != 0 {
		t.Fatalf("trace text exit = %d: %s", code, errOut)
	}
	for _, want := range []string{"v8-cpuprofile", "dispatch → greet", "dyn.trace", "observed only", "run → dispatch"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text output missing %q:\n%s", want, text)
		}
	}
}

func TestTraceCommandErrors(t *testing.T) {
	dir := copyAdapterFixtureNamed(t, traceFixtureDir())
	if code, _, _ := run(t, "trace", "--root", dir); code != 2 {
		t.Fatalf("missing --file exit = %d, want 2", code)
	}
	if code, _, _ := run(t, "trace", "--file", filepath.Join(dir, "missing.cpuprofile"), "--root", dir); code != 3 {
		t.Fatalf("missing profile exit = %d, want 3", code)
	}
	tmp := t.TempDir()
	writeFile(t, tmp, "bad.json", `{`)
	if code, _, _ := run(t, "trace", "--file", filepath.Join(tmp, "bad.json"), "--root", dir); code != 2 {
		t.Fatalf("malformed profile exit = %d, want 2", code)
	}
}
