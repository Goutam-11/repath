package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Goutam-11/repath/internal/adapter"
)

func copyAdapterFixture(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "fixtures", "ts-adapter")
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

func TestAdapterCommand(t *testing.T) {
	dir := copyAdapterFixture(t)
	scip := filepath.Join("..", "..", "testdata", "fixtures", "ts-adapter", "index.scip")

	code, beforeOut, errOut := run(t, "status", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("status before exit = %d: %s", code, errOut)
	}
	var before struct {
		Unresolved int `json:"unresolved"`
	}
	if err := json.Unmarshal([]byte(beforeOut), &before); err != nil {
		t.Fatal(err)
	}
	if before.Unresolved != 1 {
		t.Fatalf("status before unresolved = %d, want 1", before.Unresolved)
	}

	code, out, errOut := run(t, "adapter", "--scip", scip, "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("adapter exit = %d: %s", code, errOut)
	}
	var stats adapter.Stats
	if err := json.Unmarshal([]byte(out), &stats); err != nil {
		t.Fatalf("adapter output is not JSON: %v\n%s", err, out)
	}
	if stats.Matched != 1 || stats.Edges != 1 || stats.Conflicts != 0 {
		t.Fatalf("stats = %+v", stats)
	}

	code, afterOut, errOut := run(t, "status", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("status after exit = %d: %s", code, errOut)
	}
	var after struct {
		Unresolved int             `json:"unresolved"`
		Adapters   []adapter.Stats `json:"adapters"`
	}
	if err := json.Unmarshal([]byte(afterOut), &after); err != nil {
		t.Fatal(err)
	}
	if after.Unresolved != 0 {
		t.Fatalf("status after unresolved = %d, want 0", after.Unresolved)
	}
	if len(after.Adapters) != 1 || after.Adapters[0].Edges != 1 {
		t.Fatalf("status adapters = %+v", after.Adapters)
	}
}

func TestAdapterCommandErrors(t *testing.T) {
	dir := t.TempDir()
	if code, _, _ := run(t, "adapter", "--root", dir); code != 2 {
		t.Fatalf("missing --scip exit = %d, want 2", code)
	}
	if code, _, _ := run(t, "adapter", "--scip", filepath.Join(dir, "missing.scip"), "--root", dir); code != 3 {
		t.Fatalf("missing file exit = %d, want 3", code)
	}
}

func TestAdapterCommandIndexesLazily(t *testing.T) {
	dir := copyAdapterFixture(t)
	scip := filepath.Join("..", "..", "testdata", "fixtures", "ts-adapter", "index.scip")
	code, out, errOut := run(t, "adapter", "--scip", scip, "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("adapter exit = %d: %s", code, errOut)
	}
	var stats adapter.Stats
	if err := json.Unmarshal([]byte(out), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Edges != 1 || stats.Matched != 1 {
		t.Fatalf("adapter did not index lazily: %+v", stats)
	}
}
