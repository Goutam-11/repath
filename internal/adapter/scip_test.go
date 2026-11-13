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
		"scip-typescript npm . . src/`cart.ts`/Cart#total().":      "total",
		"scip-typescript npm . . src/`cart.ts`/Cart#":              "Cart",
		"scip-typescript npm . . src/`cart.ts`/make().":            "make",
		"scip-typescript npm . . src/`cart.ts`/":                   "",
		"local 2":                                                  "",
		"scip-typescript npm . . src/`cart.ts`/Outer#Inner#":       "Inner",
		"scip-python python py-adapter 0.0.0 models/Base#total().": "total",
		"scip-python python py-adapter 0.0.0 cart/Cart#":           "Cart",
		"scip-python python py-adapter 0.0.0 app/run().":           "run",
		"scip-python python py-adapter 0.0.0 app/__init__:":        "",
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

func TestParseSCIPPythonFixture(t *testing.T) {
	index, err := LoadSCIP(filepath.Join("..", "..", "testdata", "fixtures", "py-adapter", "index.scip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Documents) != 3 {
		t.Fatalf("documents = %d, want 3", len(index.Documents))
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
	for _, want := range []string{"app.py", "cart.py", "models.py"} {
		if !paths[want] {
			t.Fatalf("document paths = %+v, missing %s", paths, want)
		}
	}
	if refs == 0 {
		t.Fatal("fixture has no reference occurrences")
	}
}

func TestPythonSCIPAdapterGate(t *testing.T) {
	root := copyFixture(t, "py-adapter")
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
	if beforePending != 2 {
		t.Fatalf("adapter-off pending = %d, want 2 (two untyped/inherited total calls)", beforePending)
	}

	parsed, err := LoadSCIP(filepath.Join("..", "..", "testdata", "fixtures", "py-adapter", "index.scip"))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := ApplySCIP(ctx, st, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Adapter != "scip-python" {
		t.Fatalf("adapter = %q, want scip-python", stats.Adapter)
	}
	if stats.Matched != 2 || stats.Edges != 2 || stats.Conflicts != 0 {
		t.Fatalf("stats = %+v, want 2 matched, 2 edges, 0 conflicts", stats)
	}

	afterPending, err := st.CountPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterPending != 0 {
		t.Fatalf("adapter-on pending = %d, want 0", afterPending)
	}

	edges, err := st.Edges(ctx, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, edge := range edges {
		if edge.DstQName != "Base.total" || edge.Kind != "CALLS" {
			continue
		}
		found++
		if edge.Rule != "scip.py.method" || edge.Confidence != adapterConfidence {
			t.Fatalf("adapter edge = %+v, want scip.py.method at %.2f", edge, adapterConfidence)
		}
	}
	if found != 2 {
		t.Fatalf("Base.total adapter edges = %d, want 2: %+v", found, edges)
	}
	for _, edge := range edges {
		if strings.HasPrefix(edge.Rule, "scip.ts.") {
			t.Fatalf("Python edge used a TypeScript adapter rule: %+v", edge)
		}
	}

	saved, err := SavedStats(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].Adapter != "scip-python" || saved[0].Edges != 2 {
		t.Fatalf("saved stats = %+v", saved)
	}
}

func TestFamiliesFor(t *testing.T) {
	ts, err := LoadSCIP(filepath.Join("..", "..", "testdata", "fixtures", "ts-adapter", "index.scip"))
	if err != nil {
		t.Fatal(err)
	}
	py, err := LoadSCIP(filepath.Join("..", "..", "testdata", "fixtures", "py-adapter", "index.scip"))
	if err != nil {
		t.Fatal(err)
	}
	rust, err := LoadSCIP(filepath.Join("..", "..", "testdata", "fixtures", "rs-adapter", "index.scip"))
	if err != nil {
		t.Fatal(err)
	}
	combined := SCIPIndex{Documents: append(append(ts.Documents, py.Documents...), rust.Documents...)}
	families := familiesFor(combined)
	if len(families) != 3 || families[0].adapter != "scip" || families[1].adapter != "scip-python" || families[2].adapter != "scip-rust" {
		t.Fatalf("families = %+v, want scip, scip-python, scip-rust", families)
	}
	if got := familiesFor(SCIPIndex{}); len(got) != 1 || got[0].adapter != "scip" {
		t.Fatalf("empty families = %+v, want the legacy scip run", got)
	}
}

func TestRustSymbolName(t *testing.T) {
	cases := map[string]string{
		"local 0": "",
		"rust-analyzer cargo rs-adapter 0.1.0 crate/":                     "",
		"rust-analyzer cargo rs-adapter 0.1.0 models/":                    "",
		"rust-analyzer cargo rs-adapter 0.1.0 models/Total#":              "Total",
		"rust-analyzer cargo rs-adapter 0.1.0 models/Total#total().":      "total",
		"rust-analyzer cargo rs-adapter 0.1.0 run_generic().":             "run_generic",
		"rust-analyzer cargo rs-adapter 0.1.0 models/Cart#new().":         "new",
		"rust-analyzer cargo rs-adapter 0.1.0 models/Kind#A#":             "A",
		"rust-analyzer cargo rs-adapter 0.1.0 models/Cart#count.":         "count",
		"rust-analyzer cargo rs-adapter 0.1.0 LIMIT.":                     "LIMIT",
		"rust-analyzer cargo rs-adapter 0.1.0 models/impl#[Cart]":         "",
		"rust-analyzer cargo rs-adapter 0.1.0 impl#[Cart][Total]total().": "total",
	}
	for symbol, want := range cases {
		if got := rustSymbolName(symbol); got != want {
			t.Errorf("rustSymbolName(%q) = %q, want %q", symbol, got, want)
		}
	}
}

func TestParseSCIPRustFixture(t *testing.T) {
	index, err := LoadSCIP(filepath.Join("..", "..", "testdata", "fixtures", "rs-adapter", "index.scip"))
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
		if !strings.EqualFold(doc.Language, "rust") {
			t.Fatalf("document language = %q, want rust", doc.Language)
		}
		for _, occ := range doc.Occurrences {
			if occ.Roles&1 == 0 {
				refs++
			}
		}
	}
	for _, want := range []string{"src/lib.rs", "src/models.rs"} {
		if !paths[want] {
			t.Fatalf("document paths = %+v, missing %s", paths, want)
		}
	}
	if refs == 0 {
		t.Fatal("fixture has no reference occurrences")
	}
}

func TestRustSCIPAdapterGate(t *testing.T) {
	root := copyFixture(t, "rs-adapter")
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
	if beforePending != 2 {
		t.Fatalf("adapter-off pending = %d, want 2 (generic-bound total calls)", beforePending)
	}
	beforeEdges, err := st.Edges(ctx, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeEdges) != 4 {
		t.Fatalf("adapter-off edges = %d, want 4: %+v", len(beforeEdges), beforeEdges)
	}

	parsed, err := LoadSCIP(filepath.Join("..", "..", "testdata", "fixtures", "rs-adapter", "index.scip"))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := ApplySCIP(ctx, st, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Adapter != "scip-rust" {
		t.Fatalf("adapter = %q, want scip-rust", stats.Adapter)
	}
	if stats.Matched != 2 || stats.Edges != 2 || stats.Conflicts != 0 {
		t.Fatalf("stats = %+v, want 2 matched, 2 edges, 0 conflicts", stats)
	}
	if stats.Definitions == 0 || stats.References == 0 || stats.Skipped == 0 {
		t.Fatalf("stats = %+v, want non-zero definitions/references/skips", stats)
	}

	afterPending, err := st.CountPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterPending != 0 {
		t.Fatalf("adapter-on pending = %d, want 0", afterPending)
	}
	afterEdges, err := st.Edges(ctx, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterEdges) != 6 {
		t.Fatalf("adapter-on edges = %d, want 6: %+v", len(afterEdges), afterEdges)
	}

	found := 0
	typedTotal, typedCart := 0, 0
	for _, edge := range afterEdges {
		if edge.Kind != "CALLS" || edge.Rule != "rs.typed.method" {
			continue
		}
		switch edge.DstQName {
		case "Total.total":
			typedTotal++
		case "Cart.total":
			typedCart++
		}
	}
	found = 0
	for _, edge := range afterEdges {
		if edge.DstQName != "Total.total" || edge.Kind != "CALLS" {
			continue
		}
		if edge.Rule != "scip.rs.method" {
			continue
		}
		found++
		if edge.Confidence != adapterConfidence {
			t.Fatalf("adapter edge = %+v, want confidence %.2f", edge, adapterConfidence)
		}
		if edge.SrcQName != "run_generic" && edge.SrcQName != "run_generic_again" {
			t.Fatalf("adapter source = %q, want a generic-bound function", edge.SrcQName)
		}
	}
	if found != 2 {
		t.Fatalf("Total.total adapter edges = %d, want 2: %+v", found, afterEdges)
	}
	if typedTotal != 1 || typedCart != 1 {
		t.Fatalf("tree-sitter typed-method edges = %d/%d, want Total.total/Cart.total unchanged", typedTotal, typedCart)
	}
	for _, edge := range afterEdges {
		if strings.HasPrefix(edge.Rule, "scip.ts.") || strings.HasPrefix(edge.Rule, "scip.py.") {
			t.Fatalf("Rust edge used another language's adapter rule: %+v", edge)
		}
	}

	saved, err := SavedStats(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].Adapter != "scip-rust" || saved[0].Edges != 2 {
		t.Fatalf("saved stats = %+v", saved)
	}
}
