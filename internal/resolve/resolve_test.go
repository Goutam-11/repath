package resolve_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/resolve"
	"github.com/Goutam-11/repath/internal/store"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolvePendingUsesLanguageRuleIDs pins the rule id of a pending retry to
// the referring file's language. Before the Rust case existed, every
// non-Python file fell through to the TypeScript ladder, so Rust retry edges
// were emitted as ts.global.unique (24 of them on rayon).
func TestResolvePendingUsesLanguageRuleIDs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Cargo.toml", "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n")
	writeFile(t, root, "src/lib.rs", "pub fn rs_helper() -> i32 {\n    1\n}\n\npub struct RsStruct;\n")
	writeFile(t, root, "use.ts", "export function ts_helper() { return 1; }\n")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		t.Fatalf("index run: %v", err)
	}

	rustID, ok, err := st.FileByPath(ctx, "src/lib.rs")
	if err != nil || !ok {
		t.Fatalf("file id for src/lib.rs: ok=%v err=%v", ok, err)
	}
	ix, err := resolve.Build(ctx, st, root, map[int64]bool{rustID: true})
	if err != nil {
		t.Fatalf("resolve build: %v", err)
	}

	r, found := ix.ResolvePending(rustID, "call", "rs_helper")
	if !found {
		t.Fatalf("rust retry did not resolve: %+v", r)
	}
	if r.Rule != "rs.global.unique" {
		t.Fatalf("rust retry rule = %q, want rs.global.unique", r.Rule)
	}
	if r.Confidence != 0.60 {
		t.Fatalf("rust retry confidence = %v, want 0.60", r.Confidence)
	}
	// A retry corrects the rule id, not the edge kind: a struct target must not
	// become a REFERENCES construction edge (that is rustTarget's job).
	if r.Kind != "" {
		t.Fatalf("rust retry kind = %q, want the unset CALLS kind", r.Kind)
	}
	if _, found := ix.ResolvePending(rustID, "call", "RsStruct"); !found {
		t.Fatal("rust retry did not resolve the struct name")
	}

	tsID, ok, err := st.FileByPath(ctx, "use.ts")
	if err != nil || !ok {
		t.Fatalf("file id for use.ts: ok=%v err=%v", ok, err)
	}
	tsx, err := resolve.Build(ctx, st, root, map[int64]bool{tsID: true})
	if err != nil {
		t.Fatalf("resolve build: %v", err)
	}
	r, found = tsx.ResolvePending(tsID, "call", "ts_helper")
	if !found {
		t.Fatalf("typescript retry did not resolve: %+v", r)
	}
	if r.Rule != "ts.global.unique" {
		t.Fatalf("typescript retry rule = %q, want ts.global.unique", r.Rule)
	}
}

// TestPythonInheritanceThroughUnchangedFile pins the incremental half of the
// MRO: a re-resolved subclass must still see the base chain of a file this run
// did not touch, which is only possible because the persisted EXTENDS edges are
// loaded as a resolution input. A cold run resolves the same edges from its own
// extends pre-pass.
func TestPythonInheritanceThroughUnchangedFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "base.py", `class Root:
    def step(self):
        return 1


class Base(Root):
    def other(self):
        return 2
`)
	writeFile(t, root, "child.py", `from base import Base


class Child(Base):
    def go(self):
        return self.step()

    def go2(self):
        return self.other()
`)
	writeFile(t, root, "other.py", "def helper():\n    return 1\n")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		t.Fatalf("cold index: %v", err)
	}

	want := map[string]string{
		"Child.go|Root.step":   "py.self.inherited",
		"Child.go2|Base.other": "py.self.inherited",
	}
	coldEdges := pythonEdges(t, st)
	coldCount := len(coldEdges)
	for key, rule := range want {
		if got := coldEdges[key]; got != rule {
			t.Fatalf("cold edge %s rule = %q, want %q (all: %v)", key, got, rule, coldEdges)
		}
	}

	// Re-resolve child.py only: Base and its own base live in untouched files.
	path := filepath.Join(root, "child.py")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open child.py: %v", err)
	}
	if _, err := f.WriteString("\n# change\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	result, err := index.Run(ctx, st, root, index.Options{})
	if err != nil {
		t.Fatalf("incremental index: %v", err)
	}
	if result.Indexed != 1 {
		t.Fatalf("incremental run indexed %d files, want 1", result.Indexed)
	}
	after := pythonEdges(t, st)
	if len(after) != coldCount {
		t.Fatalf("edge count changed after an MRO edit: %d -> %d", coldCount, len(after))
	}
	for key, rule := range want {
		if got := after[key]; got != rule {
			t.Fatalf("incremental edge %s rule = %q, want %q (all: %v)", key, got, rule, after)
		}
	}
}

// TestPythonExternalBaseStaysUnresolved pins the documented "by design" case: a
// base from the standard library is external, so an inherited self call stays
// unresolved instead of being guessed against a local class of the same name.
func TestPythonExternalBaseStaysUnresolved(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "test_case.py", `import unittest


class MyTest(unittest.TestCase):
    def check(self):
        self.assertEqual(1, 1)
`)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		t.Fatalf("index: %v", err)
	}

	if edges := pythonEdges(t, st); len(edges) != 0 {
		t.Fatalf("external base produced edges: %v", edges)
	}
	pending, err := st.Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].Name != "assertEqual" || pending[0].Form != "this" {
		t.Fatalf("pending = %+v, want one this/assertEqual row", pending)
	}
}

// pythonEdges reads the resolved Python edges as "srcQName|dstQName" → rule.
func pythonEdges(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	edges, err := st.Edges(context.Background(), 0.5)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	out := make(map[string]string, len(edges))
	for _, e := range edges {
		if !strings.HasSuffix(e.SrcPath, ".py") {
			continue
		}
		out[e.SrcQName+"|"+e.DstQName] = e.Rule
	}
	return out
}
