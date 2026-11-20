package resolve_test

import (
	"context"
	"fmt"
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
	// Python provenance: pkg/__init__.py star-imports the name, so a direct
	// `from pkg import py_helper` misses the export lookup, while a bare call
	// has no import binding at all.
	writeFile(t, root, "pkg/__init__.py", "from .impl import *\n")
	writeFile(t, root, "pkg/impl.py", "def py_helper() -> int:\n    return 1\n")
	writeFile(t, root, "py_import.py", "from pkg import py_helper\n\n\ndef run_imported() -> int:\n    return py_helper()\n")
	writeFile(t, root, "py_free.py", "def run_bare() -> int:\n    return py_helper()\n")

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
	// #82: a TypeScript bare-name retry is the same name-only guess as the
	// forward ladder, so it stays below the default 0.5 floor.
	if r.Confidence != 0.40 {
		t.Fatalf("typescript retry confidence = %v, want 0.40", r.Confidence)
	}

	importedID, ok, err := st.FileByPath(ctx, "py_import.py")
	if err != nil || !ok {
		t.Fatalf("file id for py_import.py: ok=%v err=%v", ok, err)
	}
	freeID, ok, err := st.FileByPath(ctx, "py_free.py")
	if err != nil || !ok {
		t.Fatalf("file id for py_free.py: ok=%v err=%v", ok, err)
	}
	pyx, err := resolve.Build(ctx, st, root, nil)
	if err != nil {
		t.Fatalf("python resolve build: %v", err)
	}
	r, found = pyx.ResolvePending(importedID, "call", "py_helper")
	if !found || r.Rule != "py.global.unique" {
		t.Fatalf("python import-provenance retry = %+v found=%v", r, found)
	}
	// #97: importing the name is the provenance that keeps the recovery at 0.60.
	if r.Confidence != 0.60 {
		t.Fatalf("python import-provenance retry confidence = %v, want 0.60", r.Confidence)
	}
	r, found = pyx.ResolvePending(freeID, "call", "py_helper")
	if !found || r.Rule != "py.global.unique" {
		t.Fatalf("python bare retry = %+v found=%v", r, found)
	}
	// No import binding: a bare name-only guess, below the default floor.
	if r.Confidence != 0.40 {
		t.Fatalf("python bare retry confidence = %v, want 0.40", r.Confidence)
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

// indexRepo indexes a throwaway repo with the real pipeline and returns its
// store, which is the only way to exercise resolution end to end.
func indexRepo(t *testing.T, root string) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		t.Fatalf("index: %v", err)
	}
	return st
}

// tsEdges reads the resolved TypeScript edges as "srcQName|dstQName" → rule.
func tsEdges(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	edges, err := st.Edges(context.Background(), 0.5)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	out := make(map[string]string, len(edges))
	for _, e := range edges {
		if strings.HasSuffix(e.SrcPath, ".py") || strings.HasSuffix(e.SrcPath, ".rs") {
			continue
		}
		out[e.SrcQName+"|"+e.DstQName] = e.Rule
	}
	return out
}

// TestTypeScriptWorkspacePackageResolves pins #56 workspace detection: a
// package.json `workspaces` entry maps `@scope/pkg` to the package's source
// entry (manifest `main`/`exports`, then src/index), and a subpath resolves
// inside the package directory. It is local-manifest only, no registry.
func TestTypeScriptWorkspacePackageResolves(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "package.json", `{"name":"mono","private":true,"workspaces":["packages/*"]}`)
	writeFile(t, root, "packages/widget/package.json", `{"name":"@demo/widget","main":"src/index.ts"}`)
	writeFile(t, root, "packages/widget/src/index.ts", "export * from './impl';\n")
	writeFile(t, root, "packages/widget/src/impl.ts", "export function gadget() { return 1; }\n")
	writeFile(t, root, "packages/gizmo/package.json", `{"name":"@demo/gizmo","exports":"./index.ts"}`)
	writeFile(t, root, "packages/gizmo/index.ts", "export function spin() { return 2; }\n")
	writeFile(t, root, "packages/gizmo/extra.ts", "export function extra() { return 3; }\n")
	writeFile(t, root, "use.ts", `import { gadget } from "@demo/widget";
import { spin } from "@demo/gizmo";
import { extra } from "@demo/gizmo/extra";

export function run(): number {
  return gadget() + spin() + extra();
}
`)

	st := indexRepo(t, root)
	want := map[string]string{
		"run|gadget": "ts.import.workspace.star",
		"run|spin":   "ts.import.workspace",
		"run|extra":  "ts.import.workspace",
	}
	got := tsEdges(t, st)
	for key, rule := range want {
		if got[key] != rule {
			t.Fatalf("edge %s rule = %q, want %q (all: %v)", key, got[key], rule, got)
		}
	}
	pending, err := st.Pending(context.Background())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("workspace imports left %d pending rows: %+v", len(pending), pending)
	}
}

// TestTypeScriptBarrelDepthIsBounded pins the chain cap from #56: eight hops
// resolve, nine stop and stay unresolved instead of being guessed. The global
// pending retry still links the name, which is why the assertion is on the rule
// and not only on an unresolved count.
func TestTypeScriptBarrelDepthIsBounded(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "impl.ts", "export function helper() { return 1; }\n")
	for hop := 1; hop <= 9; hop++ {
		source := "./impl"
		if hop > 1 {
			source = fmt.Sprintf("./hop%d", hop-1)
		}
		writeFile(t, root, fmt.Sprintf("hop%d.ts", hop),
			fmt.Sprintf("export { helper } from %q;\n", source))
	}
	writeFile(t, root, "use.ts", `import { helper as eight } from "./hop8";
import { helper as nine } from "./hop9";

export function viaEight(): number {
  return eight();
}

export function viaNine(): number {
  return nine();
}
`)

	st := indexRepo(t, root)
	got := tsEdges(t, st)

	if rule := got["viaEight|helper"]; rule != "ts.import.reexport" {
		t.Fatalf("eight-hop chain rule = %q, want ts.import.reexport (all: %v)", rule, got)
	}
	// Nine hops exceed maxBarrelDepth: the walk stops, so the aliased call has
	// no edge and stays in the pending queue rather than being linked by name
	// (the pending retry only re-links identifiers whose own name is unique).
	if rule, found := got["viaNine|helper"]; found {
		t.Fatalf("nine-hop chain was followed: rule %q (all: %v)", rule, got)
	}
	pending, err := st.Pending(context.Background())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].Name != "nine" {
		t.Fatalf("pending = %+v, want one row for the aliased nine call", pending)
	}
}
