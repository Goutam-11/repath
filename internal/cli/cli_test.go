package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(args, &out, &errb)
	return code, out.String(), errb.String()
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

func TestStatusJSON(t *testing.T) {
	code, out, _ := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	var report statusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if report.Indexed {
		t.Fatal("indexed = true before the engine exists")
	}
	if report.SchemaVersion != 0 {
		t.Fatalf("schema_version = %d, want 0", report.SchemaVersion)
	}
}

func TestStatusRejectsUnknownFlag(t *testing.T) {
	code, _, errOut := run(t, "status", "--verbose")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "unknown flag") {
		t.Fatalf("stderr = %q", errOut)
	}
}
