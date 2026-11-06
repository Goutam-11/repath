package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverFiltersUnsupportedFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.ts", "export function a() {}\n")
	write(t, dir, "nested/b.tsx", "export const B = () => null;\n")
	write(t, dir, "c.md", "# docs\n")
	write(t, dir, "d.min.js", "function d(){}\n")
	write(t, dir, "e.d.ts", "declare function e(): void;\n")
	write(t, dir, "node_modules/f.ts", "export function f() {}\n")
	write(t, dir, "big.ts", string(make([]byte, maxFileSize+1)))

	got, err := Discover(context.Background(), dir)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	want := []string{"a.ts", "nested/b.tsx"}
	if len(got) != len(want) {
		t.Fatalf("got %d files: %+v", len(got), got)
	}
	for i, path := range want {
		if got[i].Path != path {
			t.Fatalf("got[%d] = %q, want %q", i, got[i].Path, path)
		}
		if got[i].Abs == "" || got[i].Size == 0 {
			t.Fatalf("source not filled: %+v", got[i])
		}
	}
}
