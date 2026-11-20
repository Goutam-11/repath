package index_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

func scopeRepo(t *testing.T) string {
	t.Helper()
	dir := newGitRepo(t)
	writeSource(t, dir, "a/one.py", "def alpha():\n    return 1\n")
	writeSource(t, dir, "a/two.py", "from a.one import alpha\n\n\ndef use():\n    return alpha()\n")
	writeSource(t, dir, "b/one.py", "def beta():\n    return 2\n")
	writeSource(t, dir, "root.py", "def root_fn():\n    return 3\n")
	commitAll(t, dir, "scope fixture")
	return dir
}

func TestCleanScope(t *testing.T) {
	ok := map[string]string{
		"./a/":     "a",
		"a//b":     "a/b",
		"a/./c":    "a/c",
		" a ":      "a",
		"x\\y":     "x/y",
		"a/../a/b": "a/b",
	}
	for raw, want := range ok {
		got, err := index.CleanScope([]string{raw})
		if err != nil {
			t.Fatalf("CleanScope(%q): %v", raw, err)
		}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("CleanScope(%q) = %v, want [%s]", raw, got, want)
		}
	}

	for _, raw := range []string{"", "   ", ".", "/abs/path", "../escape", ".."} {
		if _, err := index.CleanScope([]string{raw}); err == nil {
			t.Fatalf("CleanScope(%q) accepted an invalid scope", raw)
		}
	}

	got, err := index.CleanScope([]string{"b", "a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("CleanScope sorted/deduped = %v, want [a b]", got)
	}
}

func TestScopeRestrictsAndRebuilds(t *testing.T) {
	dir := scopeRepo(t)

	if got := runIndexOptions(t, dir, index.Options{WholeRepo: true}); got.Files != 4 {
		t.Fatalf("whole repo files = %d, want 4", got.Files)
	}

	scoped := runIndexOptions(t, dir, index.Options{Scope: []string{"a"}})
	if scoped.Files != 2 {
		t.Fatalf("scoped files = %d, want the two files under a/", scoped.Files)
	}
	st := openIndex(t, dir)
	meta, err := st.Meta(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := index.ScopeFromMeta(meta); len(got) != 1 || got[0] != "a" {
		t.Fatalf("recorded scope = %v, want [a]", got)
	}
	paths, err := st.FilePaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "a/") {
			t.Fatalf("out-of-scope file survived the rebuild: %s", p)
		}
	}
	st.Close()

	// No explicit scope preserves what the index records (a lazy refresh must
	// not flip the scope).
	preserved := runIndexOptions(t, dir, index.Options{})
	if preserved.Files != 2 {
		t.Fatalf("preserving run files = %d, want 2", preserved.Files)
	}

	// A plain `index` asks for the whole repository and rebuilds.
	whole := runIndexOptions(t, dir, index.Options{WholeRepo: true})
	if whole.Files != 4 {
		t.Fatalf("whole-repo rebuild files = %d, want 4", whole.Files)
	}
	st = openIndex(t, dir)
	meta, err = st.Meta(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := index.ScopeFromMeta(meta); len(got) != 0 {
		t.Fatalf("scope after plain index = %v, want none", got)
	}
	st.Close()
}

// TestScopeParity pins cold / incremental / --full equality inside a scope.
func TestScopeParity(t *testing.T) {
	dir := scopeRepo(t)
	cold := runIndexOptions(t, dir, index.Options{Scope: []string{"a"}})
	if cold.Files != 2 {
		t.Fatalf("cold scoped files = %d, want 2", cold.Files)
	}
	first := scopeSnapshot(t, dir)

	// A comment edit inside the scope, re-resolved by the normal incremental
	// pass (which preserves the recorded scope).
	path := filepath.Join(dir, "a", "one.py")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n# scope parity probe\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if got := runIndexOptions(t, dir, index.Options{}); got.Indexed != 1 {
		t.Fatalf("incremental run indexed %d files, want 1", got.Indexed)
	}
	second := scopeSnapshot(t, dir)

	if got := runIndexOptions(t, dir, index.Options{Full: true}); got.Files != 2 {
		t.Fatalf("full scoped files = %d, want 2", got.Files)
	}
	third := scopeSnapshot(t, dir)

	if !equalStrings(first, second) {
		t.Fatalf("incremental graph differs from cold:\n cold: %v\n warm: %v", first, second)
	}
	if !equalStrings(first, third) {
		t.Fatalf("second --full graph differs from cold")
	}
}

// TestScopedIndexNeverTouchesTheCommitCache pins the cache rule: the
// commit-keyed cache describes the whole tree, so a scoped run must neither
// seed from it nor write it.
func TestScopedIndexNeverTouchesTheCommitCache(t *testing.T) {
	dir := scopeRepo(t)
	ctx := context.Background()

	st, err := index.Prepare(ctx, dir, false)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if counts, err := st.Counts(ctx); err != nil || counts.Files != 4 {
		t.Fatalf("prepared files = %d (err %v), want 4", counts.Files, err)
	}
	st.Close()

	head := gitRun(t, dir, "rev-parse", "HEAD")
	cachePath := filepath.Join(dir, ".git", "repath", "commits", head+".db")
	cache := openReadOnly(t, cachePath)
	if counts, err := cache.Counts(ctx); err != nil || counts.Files != 4 {
		t.Fatalf("seeded cache files = %d (err %v), want 4", counts.Files, err)
	}
	cache.Close()

	// A scoped build plus an explicit StoreCommit must leave the cache alone.
	st = openIndex(t, dir)
	if _, err := index.Run(ctx, st, dir, index.Options{Scope: []string{"a"}}); err != nil {
		t.Fatalf("scoped run: %v", err)
	}
	if err := index.StoreCommit(ctx, dir, st); err != nil {
		t.Fatalf("store commit: %v", err)
	}
	if counts, err := st.Counts(ctx); err != nil || counts.Files != 2 {
		t.Fatalf("scoped index files = %d (err %v), want 2", counts.Files, err)
	}
	st.Close()

	cache = openReadOnly(t, cachePath)
	if counts, err := cache.Counts(ctx); err != nil || counts.Files != 4 {
		t.Fatalf("cache was rewritten by a scoped run: files = %d (err %v)", counts.Files, err)
	}
	cache.Close()

	// A scoped Prepare with no build must not seed the active index from the
	// whole-repo cache either.
	if err := os.Remove(filepath.Join(dir, ".repath", "index.db")); err != nil {
		t.Fatal(err)
	}
	empty, err := index.PrepareScoped(ctx, dir, true, []string{"a"})
	if err != nil {
		t.Fatalf("prepare scoped: %v", err)
	}
	defer empty.Close()
	if counts, err := empty.Counts(ctx); err != nil || counts.Files != 0 {
		t.Fatalf("scoped prepare seeded %d files, want an empty index", counts.Files)
	}
}

func openReadOnly(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatalf("open read-only %s: %v", path, err)
	}
	return st
}

// scopeSnapshot renders the graph as sorted lines so row-set equality is a
// string comparison.
func scopeSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	ctx := context.Background()
	st := openIndex(t, dir)
	defer st.Close()

	paths, err := st.FilePaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := st.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := st.GraphEdges(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := st.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var out []string
	for _, p := range paths {
		out = append(out, "file "+p)
	}
	for _, n := range nodes {
		out = append(out, "node "+n.QName+"|"+paths[n.FileID]+"|"+n.Kind)
	}
	for _, e := range edges {
		out = append(out, "edge "+e.SrcNode.QName+"|"+e.SrcPath+" -> "+e.DstNode.QName+"|"+e.DstPath+" "+e.Kind+" "+e.Rule)
	}
	for _, p := range pending {
		out = append(out, "pending "+p.Name+"|"+paths[p.FileID]+"|"+p.Kind+"|"+p.Form)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestScopeSurvivesAHeadChange pins the rebuild path: a new commit invalidates
// the active database, but the recorded scope must be carried into the rebuilt
// index instead of silently widening to the whole repository.
func TestScopeSurvivesAHeadChange(t *testing.T) {
	dir := scopeRepo(t)
	ctx := context.Background()

	if got := runIndexOptions(t, dir, index.Options{Scope: []string{"a"}}); got.Files != 2 {
		t.Fatalf("scoped files = %d, want 2", got.Files)
	}

	// A new commit moves HEAD, so the active database is stale on the next open.
	writeSource(t, dir, "b/two.py", "def gamma():\n    return 4\n")
	commitAll(t, dir, "head change")
	head := gitRun(t, dir, "rev-parse", "HEAD")

	st, err := index.Prepare(ctx, dir, false)
	if err != nil {
		t.Fatalf("prepare after head change: %v", err)
	}
	defer st.Close()
	counts, err := st.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Files != 2 {
		t.Fatalf("rebuilt files = %d, want the carried scope's 2", counts.Files)
	}
	meta, err := st.Meta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := index.ScopeFromMeta(meta); len(got) != 1 || got[0] != "a" {
		t.Fatalf("scope after rebuild = %v, want [a]", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "repath", "commits", head+".db")); err == nil {
		t.Fatal("a scoped rebuild wrote a whole-repo commit cache entry")
	}
}
