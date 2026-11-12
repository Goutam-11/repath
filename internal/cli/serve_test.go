package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeRejectsBadArguments(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.ts", "export function a(): void {}\n")

	for _, args := range [][]string{
		{"serve", "extra", "--root", dir},
		{"serve", "--addr", "not-a-host-port", "--root", dir},
	} {
		code, _, errOut := run(t, args...)
		if code != 2 {
			t.Fatalf("%v exit = %d, want 2 (stderr %q)", args, code, errOut)
		}
	}
}

func TestServeRequiresAnIndex(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.ts", "export function a(): void {}\n")

	code, _, errOut := run(t, "serve", "--root", dir)
	if code != 3 {
		t.Fatalf("serve without an index exit = %d, want 3\nstderr: %s", code, errOut)
	}
	if !strings.Contains(errOut, "run repath index or status first") {
		t.Fatalf("stderr = %q", errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, ".repath", "index.db")); !os.IsNotExist(err) {
		t.Fatalf("serve created an index: %v", err)
	}
}
