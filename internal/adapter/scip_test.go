package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

func copyFixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "fixtures", name)
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

func TestSymbolName(t *testing.T) {
	cases := map[string]string{
		"scip-typescript npm . . src/`cart.ts`/Cart#total().": "total",
		"scip-typescript npm . . src/`cart.ts`/Cart#":         "Cart",
		"scip-typescript npm . . src/`cart.ts`/make().":       "make",
		"scip-typescript npm . . src/`cart.ts`/":              "",
		"local 2":                                             "",
		"scip-typescript npm . . src/`cart.ts`/Outer#Inner#":  "Inner",
	}
	for symbol, want := range cases {
		if got := symbolName(symbol); got != want {
			t.Errorf("symbolName(%q) = %q, want %q", symbol, got, want)
		}
	}
}

func TestParseSCIPFixture(t *testing.T) {
	index, err := LoadSCIP(filepath.Join("..", "..", "testdata", "fixtures", "ts-adapter", "index.scip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Documents) != 2 {
		t.Fatalf("documents = %d, want 2", len(index.Documents))
	}

	paths := map[string]bool{}
	var refs int
	for _, doc := range index.Documents {
		paths[doc.Path] = true
		for _, occ := range doc.Occurrences {
			if occ.Roles&1 == 0 {
				refs++
			}
		}
	}
	if !paths["src/app.ts"] || !paths["src/cart.ts"] {
		t.Fatalf("document paths = %+v", paths)
	}
	if refs == 0 {
		t.Fatal("fixture has no reference occurrences")
	}
}

func TestSCIPAdapterGate(t *testing.T) {
	root := copyFixture(t, "ts-adapter")
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		t.Fatal(err)
	}

	beforePending, err := st.CountPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	beforeEdges, err := st.Edges(ctx, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if beforePending != 1 {
		t.Fatalf("adapter-off pending = %d, want 1 (cart.total is untyped)", beforePending)
	}
	if len(beforeEdges) != 2 {
		t.Fatalf("adapter-off edges = %d, want 2", len(beforeEdges))
	}

	parsed, err := LoadSCIP(filepath.Join("..", "..", "testdata", "fixtures", "ts-adapter", "index.scip"))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := ApplySCIP(ctx, st, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Matched != 1 || stats.Edges != 1 || stats.Conflicts != 0 {
		t.Fatalf("stats = %+v, want 1 matched, 1 edge, 0 conflicts", stats)
	}

	afterPending, err := st.CountPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	afterEdges, err := st.Edges(ctx, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if afterPending != 0 {
		t.Fatalf("adapter-on pending = %d, want 0", afterPending)
	}
	if len(afterEdges) != 3 {
		t.Fatalf("adapter-on edges = %d, want 3", len(afterEdges))
	}

	found := false
	for _, edge := range afterEdges {
		if edge.SrcQName == "run" && edge.DstQName == "Cart.total" && edge.Kind == "CALLS" {
			found = true
			if edge.Rule != "scip.ts.method" {
				t.Fatalf("adapter edge rule = %q, want scip.ts.method", edge.Rule)
			}
			if edge.Confidence != adapterConfidence {
				t.Fatalf("adapter edge confidence = %v, want %v", edge.Confidence, adapterConfidence)
			}
		}
	}
	if !found {
		t.Fatalf("adapter edge run -> Cart.total not found: %+v", afterEdges)
	}

	saved, err := SavedStats(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].Adapter != "scip" || saved[0].Edges != 1 {
		t.Fatalf("saved stats = %+v", saved)
	}
}

func TestLoadSCIPMissingFile(t *testing.T) {
	_, err := LoadSCIP(filepath.Join(t.TempDir(), "missing.scip"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestSCIPAdapterSubprojectPaths(t *testing.T) {
	root := copyFixture(t, "ts-adapter")
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		t.Fatal(err)
	}

	parsed, err := LoadSCIP(filepath.Join("..", "..", "testdata", "fixtures", "ts-adapter", "index.scip"))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a SCIP index rooted at src/: paths lose the repo-relative prefix.
	for i := range parsed.Documents {
		parsed.Documents[i].Path = strings.TrimPrefix(parsed.Documents[i].Path, "src/")
	}
	stats, err := ApplySCIP(ctx, st, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Matched != 1 || stats.Edges != 1 {
		t.Fatalf("stats = %+v, want the suffix match to resolve one reference", stats)
	}
}
