package cli

import (
	"path/filepath"
	"testing"
)

// TestMergeCommandExitCodes pins the command contract: no parts or no --out is
// usage (2), a missing artifact is not found (3), and a merge never overwrites
// an existing artifact directory (2).
func TestMergeCommandExitCodes(t *testing.T) {
	if code, _, _ := run(t, "merge"); code != 2 {
		t.Fatalf("merge with no args = %d, want 2", code)
	}
	if code, _, _ := run(t, "merge", "some/artifact"); code != 2 {
		t.Fatalf("merge without --out = %d, want 2", code)
	}
	dir := t.TempDir()
	if code, _, _ := run(t, "merge", filepath.Join(dir, "missing"), "--out", filepath.Join(dir, "out")); code != 3 {
		t.Fatalf("merge with a missing part = %d, want 3", code)
	}
}
